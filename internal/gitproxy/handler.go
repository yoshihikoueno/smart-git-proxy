package gitproxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"log/slog"

	"golang.org/x/sync/singleflight"

	"github.com/crohr/smart-git-proxy/internal/config"
	"github.com/crohr/smart-git-proxy/internal/metrics"
	"github.com/crohr/smart-git-proxy/internal/mirror"
)

// Kind represents the type of git request.
type Kind string

const (
	KindInfo        Kind = "info"
	KindUploadPack  Kind = "upload-pack"
	KindReceivePack Kind = "receive-pack"
	KindUnknown     Kind = "unknwon"
)

type Server struct {
	cfg     *config.Config
	mirror  *mirror.Mirror
	log     *slog.Logger
	metrics *metrics.Metrics

	// Track last cache status per repo for display in upload-pack
	statusCache sync.Map // map[repoKey]mirror.Status

	// Cache for depth=1/no-have packs keyed by repo/want/hash-of-request
	packCache      sync.Map // map[string]*packEntry
	packCacheGroup singleflight.Group
}

func New(cfg *config.Config, m *mirror.Mirror, log *slog.Logger, metrics *metrics.Metrics) *Server {
	return &Server{cfg: cfg, mirror: m, log: log, metrics: metrics}
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s.log.Debug("incoming request", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery)

		host, owner, repo, kind, err := s.resolveTarget(r)
		if err != nil {
			s.log.Error("resolve target failed", "err", err, "path", r.URL.Path)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		repoKey := fmt.Sprintf("%s/%s/%s", host, owner, repo)
		s.log.Debug("resolved target", "host", host, "owner", owner, "repo", repo, "kind", kind)
		s.metrics.RequestsTotal.WithLabelValues(repoKey, string(kind), r.RemoteAddr).Inc()

		switch kind {
		case KindReceivePack:
			http.Error(w, "write operation is not supported", http.StatusBadRequest)
		default:
			s.handle(w, r, host, owner, repo, repoKey, start)
		}
	})
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, host, owner, repo, repoKey string, start time.Time) {
	// Build upstream URL for cloning/syncing
	upstreamURL := fmt.Sprintf("https://%s/%s/%s.git", host, owner, repo)

	// Determine auth for upstream sync
	authHeader := ""
	switch s.cfg.AuthMode {
	case "static":
		// Use configured static token
		authHeader = "Bearer " + s.cfg.StaticToken
	case "pass-through":
		// Use auth from client request
		authHeader = r.Header.Get("Authorization")
	}
	s.log.Debug("auth check", "mode", s.cfg.AuthMode, "hasAuth", authHeader != "", "repo", repoKey)

	// Ensure mirror is synced
	ensureStart := time.Now()
	repoPath, status, err := s.mirror.EnsureRepo(r.Context(), host, owner, repo, upstreamURL, authHeader)
	if err != nil {
		s.fail(w, repoKey, KindInfo, err)
		return
	}
	s.log.Debug("ensure repo done", "repo", repoKey, "status", status, "duration_ms", time.Since(ensureStart).Milliseconds())

	// Store status for the upcoming upload-pack request
	s.statusCache.Store(repoKey, status)
	s.log.Info("request", "repo", repoKey, "status", status)

	// Serve refs from local mirror
	serveStart := time.Now()
	if path, err := exec.LookPath("git"); err != nil {
		s.log.Error("setup serve failed", "Failed to find git executable", time.Since(serveStart).Milliseconds())
	} else {
		buf := new(bytes.Buffer)
		handler := &cgi.Handler{
			Path:   path,
			Dir:    repoPath,
			Env:    []string{"GIT_HTTP_EXPORT_ALL=1", fmt.Sprintf("GIT_PROJECT_ROOT=%s", s.mirror.Root())},
			Args:   []string{"http-backend"},
			Stderr: buf,
		}
		handler.ServeHTTP(w, r)
		if buf.Len() != 0 {
			s.log.Info("git http-backend", "stderr", buf.String(), "duration_ms", time.Since(serveStart).Milliseconds())
		}
		s.log.Debug("serve done", "repo", repoKey, "duration_ms", time.Since(serveStart).Milliseconds())
	}

	s.metrics.ResponsesTotal.WithLabelValues(repoKey, string(KindInfo), "200").Inc()
	s.metrics.UpstreamLatency.WithLabelValues(repoKey, string(KindInfo)).Observe(time.Since(start).Seconds())
	s.log.Debug("info/refs complete", "repo", repoKey, "total_duration_ms", time.Since(start).Milliseconds())
}

func (s *Server) resolveTarget(r *http.Request) (host, owner, repo string, kind Kind, err error) {
	// Path format: /{host}/{owner}/{repo}/info/refs or /{host}/{owner}/{repo}/git-upload-pack
	pathStr := strings.TrimPrefix(r.URL.Path, "/")
	if pathStr == "" {
		return "", "", "", "", errors.New("empty path")
	}

	// Parse the path to extract components
	u, err := url.Parse("https://placeholder/" + pathStr)
	if err != nil {
		return "", "", "", "", fmt.Errorf("invalid path: %w", err)
	}

	// Determine kind from suffix
	switch {
	case strings.HasSuffix(u.Path, "/info/refs"):
		kind = KindInfo
	case strings.HasSuffix(u.Path, "/git-upload-pack"):
		kind = KindUploadPack
	case strings.HasSuffix(u.Path, "/git-receive-pack"):
		kind = KindReceivePack
	default:
		kind = KindUnknown
	}

	// Remove git endpoint suffix to get repo path
	re := regexp.MustCompile(`/(HEAD|info/refs|objects/(info/[^/]+|[0-9a-f]{2}/[0-9a-f]{38}|pack/pack-[0-9a-f]{40}\.(pack|idx))|git-(upload|receive)-pack)?$`)
	repoPath := strings.TrimPrefix(u.Path, "/")
	repoPath = re.ReplaceAllLiteralString(repoPath, "")
	repoPath = strings.TrimSuffix(repoPath, ".git")

	// Split into host/owner/repo
	parts := strings.SplitN(repoPath, "/", 3)
	if len(parts) < 3 {
		return "", "", "", "", errors.New("invalid path: expected /{host}/{owner}/{repo}/...")
	}
	host = parts[0]
	owner = parts[1]
	repo = parts[2]

	// Handle nested paths (e.g., owner/repo/subgroup)
	if strings.Contains(repo, "/") {
		// For GitLab-style nested groups, combine them
		repo = path.Base(repo)
	}

	// Validate against allowed upstreams
	allowed := false
	for _, h := range s.cfg.AllowedUpstreams {
		if h == host {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", "", "", "", fmt.Errorf("upstream %q not in allowed list", host)
	}

	return host, owner, repo, kind, nil
}

func (s *Server) fail(w http.ResponseWriter, repo string, kind Kind, err error) {
	s.metrics.ErrorsTotal.WithLabelValues(repo, string(kind)).Inc()
	s.log.Error("request failed", "err", err, "repo", repo, "kind", kind)
	http.Error(w, err.Error(), http.StatusBadGateway)
}
