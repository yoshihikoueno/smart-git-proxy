package gitproxy

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"golang.org/x/sync/singleflight"

	"github.com/crohr/smart-git-proxy/internal/config"
	"github.com/crohr/smart-git-proxy/internal/gitserve"
	"github.com/crohr/smart-git-proxy/internal/metrics"
	"github.com/crohr/smart-git-proxy/internal/mirror"
)

// Kind represents the type of git request.
type Kind string

const (
	KindInfo Kind = "info"
	KindPack Kind = "pack"
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
		case KindInfo:
			s.handleInfoRefs(w, r, host, owner, repo, repoKey, start)
		case KindPack:
			s.handleUploadPack(w, r, host, owner, repo, repoKey, start)
		default:
			http.Error(w, "unsupported path", http.StatusBadRequest)
		}
	})
}

func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, host, owner, repo, repoKey string, start time.Time) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" {
		http.Error(w, "unsupported service", http.StatusBadRequest)
		return
	}

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
	if err := gitserve.ServeInfoRefs(w, r, repoPath, string(status), s.cfg.UploadPackThreads, s.log); err != nil {
		s.log.Error("serve info/refs failed", "err", err, "repo", repoKey, "duration_ms", time.Since(serveStart).Milliseconds())
		// Response already started, can't change status
	}
	s.log.Debug("serve info/refs done", "repo", repoKey, "duration_ms", time.Since(serveStart).Milliseconds())

	s.metrics.ResponsesTotal.WithLabelValues(repoKey, string(KindInfo), "200").Inc()
	s.metrics.UpstreamLatency.WithLabelValues(repoKey, string(KindInfo)).Observe(time.Since(start).Seconds())
	s.log.Debug("info/refs complete", "repo", repoKey, "total_duration_ms", time.Since(start).Milliseconds())
}

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, host, owner, repo, repoKey string, start time.Time) {
	// Get mirror path (should already exist from info/refs)
	repoPath := s.mirror.RepoPath(host, owner, repo)

	// Optionally serialize upload-pack per repo to avoid parallel pack generation
	var lock *sync.Mutex
	if s.cfg.SerializeUploadPack {
		lock = s.mirror.GetRepoLock(host, owner, repo)
		lock.Lock()
		defer lock.Unlock()
	}

	// Get cached status from info/refs call
	cacheStatus := ""
	if v, ok := s.statusCache.Load(repoKey); ok {
		cacheStatus = string(v.(mirror.Status))
	}

	// Try fast-path cached pack for depth=1/no-have (Actions clone)
	if s.cfg.EnablePackCache && s.tryServeCachedPack(w, r, repoPath, repoKey) {
		s.log.Debug("served cached pack", "repo", repoKey, "total_duration_ms", time.Since(start).Milliseconds())
		s.metrics.ResponsesTotal.WithLabelValues(repoKey, string(KindPack), "200").Inc()
		s.metrics.UpstreamLatency.WithLabelValues(repoKey, string(KindPack)).Observe(time.Since(start).Seconds())
		return
	}

	// Serve pack from local mirror
	serveStart := time.Now()
	if err := gitserve.ServeUploadPack(w, r, repoPath, cacheStatus, s.cfg.UploadPackThreads, s.log); err != nil {
		s.log.Error("serve upload-pack failed", "err", err, "repo", repoKey, "duration_ms", time.Since(serveStart).Milliseconds())
		// Response already started, can't change status
	}
	s.log.Debug("serve upload-pack done", "repo", repoKey, "duration_ms", time.Since(serveStart).Milliseconds())

	s.metrics.ResponsesTotal.WithLabelValues(repoKey, string(KindPack), "200").Inc()
	s.metrics.UpstreamLatency.WithLabelValues(repoKey, string(KindPack)).Observe(time.Since(start).Seconds())
	s.log.Debug("upload-pack complete", "repo", repoKey, "total_duration_ms", time.Since(start).Milliseconds())
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
		kind = KindPack
	default:
		return "", "", "", "", fmt.Errorf("unsupported endpoint: %s", u.Path)
	}

	// Remove git endpoint suffix to get repo path
	repoPath := strings.TrimPrefix(u.Path, "/")
	repoPath = strings.TrimSuffix(repoPath, "/info/refs")
	repoPath = strings.TrimSuffix(repoPath, "/git-upload-pack")
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

type packEntry struct {
	path string
	size int64
}

var (
	// Allow capabilities after want: "want <sha> multi_ack_detailed ..."
	wantRe     = regexp.MustCompile(`(?m)^want ([0-9a-f]{40})(?:\s|$)`)
	haveRe     = regexp.MustCompile(`(?m)^have `)
	depthRe    = regexp.MustCompile(`(?m)^deepen (\d+)$`)
	rawWantRe  = regexp.MustCompile(`(?i)want ([0-9a-f]{40})`)
	rawDepthRe = regexp.MustCompile(`(?i)deepen\s+(\d+)`)
)

// tryServeCachedPack attempts a fast-path for depth=1/no-have requests by caching the upload-pack output.
// It returns true if the response was fully served from cache (or after populating cache).
func (s *Server) tryServeCachedPack(w http.ResponseWriter, r *http.Request, repoPath, repoKey string) bool {
	// Only handle gzip and plain bodies; read small requests fully (depth=1 is small)
	body, err := readBodyMaybeGzip(r)
	if err != nil {
		s.log.Debug("pack cache read body failed", "repo", repoKey, "err", err)
		return false
	}
	// Restore body for any fallback path
	r.Body = io.NopCloser(bytes.NewReader(body))

	// Best-effort deframe pkt-lines
	payload := stripPktPayload(body)
	dump := payload
	if len(dump) > 2048 {
		dump = dump[:2048]
	}
	rawDump := body
	if len(rawDump) > 2048 {
		rawDump = rawDump[:2048]
	}

	// Require: no haves, exactly one want, depth=1 (or explicitly deepen 1)
	if haveRe.Match(payload) {
		s.log.Debug("pack cache skip: have present", "repo", repoKey, "body_prefix", string(dump))
		return false
	}
	wants := wantRe.FindAllSubmatch(payload, -1)
	if len(wants) == 0 {
		wants = rawWantRe.FindAllSubmatch(payload, -1)
	}
	if len(wants) != 1 {
		// Fallback: try raw body framing
		wants = rawWantRe.FindAllSubmatch(body, -1)
		if len(wants) != 1 {
			s.log.Debug("pack cache skip: want count", "repo", repoKey, "count", len(wants), "body_prefix", string(dump), "raw_prefix", string(rawDump))
			return false
		}
	}
	want := string(wants[0][1])
	depth := depthRe.FindSubmatch(payload)
	if depth == nil {
		depth = rawDepthRe.FindSubmatch(payload)
	}
	if depth != nil && string(depth[1]) != "1" {
		s.log.Debug("pack cache skip: depth not 1", "repo", repoKey, "depth", string(depth[1]), "body_prefix", string(dump))
		return false
	}
	// If no depth specified, be conservative: require explicit deepen 1
	if depth == nil {
		// Fallback: try raw body framing
		depth = rawDepthRe.FindSubmatch(body)
		if depth == nil {
			s.log.Debug("pack cache skip: no depth", "repo", repoKey, "body_prefix", string(dump), "raw_prefix", string(rawDump))
			return false
		}
	}

	bodyHash := fmt.Sprintf("%x", sha256.Sum256(body))
	cacheKey := repoKey + ":" + want + ":" + bodyHash

	// Cache hit
	if v, ok := s.packCache.Load(cacheKey); ok {
		entry := v.(*packEntry)
		if servePackFile(w, entry.path, s.log) {
			s.log.Debug("pack cache hit", "repo", repoKey, "want", want, "path", entry.path, "size", entry.size)
			return true
		}
		// If file missing/corrupt, drop through to regenerate
		s.packCache.Delete(cacheKey)
	}

	// Singleflight to build the pack once
	_, err, _ = s.packCacheGroup.Do(cacheKey, func() (interface{}, error) {
		tmpFile, err := os.CreateTemp(repoPath, "cached-pack-*.packresp")
		if err != nil {
			return nil, err
		}
		tmpPath := tmpFile.Name()

		args := []string{"upload-pack", "--stateless-rpc", repoPath}
		if s.cfg.UploadPackThreads > 0 {
			args = append([]string{"-c", fmt.Sprintf("pack.threads=%d", s.cfg.UploadPackThreads)}, args...)
		}
		cmd := exec.CommandContext(r.Context(), "git", args...)
		cmd.Stdin = bytes.NewReader(body)
		cmd.Stdout = tmpFile
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		// Minimal isolated env similar to gitserve.gitEnv
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		}
		if gp := r.Header.Get("Git-Protocol"); gp != "" {
			env = append(env, "GIT_PROTOCOL="+gp)
		}
		cmd.Env = env

		if err := cmd.Run(); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("upload-pack cache build failed: %w, stderr: %s", err, stderr.String())
		}
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		info, err := os.Stat(tmpPath)
		if err != nil {
			_ = os.Remove(tmpPath)
			return nil, err
		}
		s.packCache.Store(cacheKey, &packEntry{path: tmpPath, size: info.Size()})
		s.log.Debug("pack cache populated", "repo", repoKey, "want", want, "path", tmpPath, "size", info.Size())
		return nil, nil
	})
	if err != nil {
		s.log.Debug("pack cache generation failed", "repo", repoKey, "err", err)
		return false
	}

	if v, ok := s.packCache.Load(cacheKey); ok {
		entry := v.(*packEntry)
		if servePackFile(w, entry.path, s.log) {
			return true
		}
		// failed to serve, drop cache
		s.packCache.Delete(cacheKey)
	}
	return false
}

func readBodyMaybeGzip(r *http.Request) ([]byte, error) {
	const maxBody = 4 << 20 // 4MiB guard; depth=1 requests are tiny
	var reader io.Reader = r.Body
	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	buf := bytes.NewBuffer(nil)
	_, err := io.CopyN(buf, reader, maxBody+1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if buf.Len() > maxBody {
		return nil, fmt.Errorf("body too large for cache (%d bytes)", buf.Len())
	}
	return buf.Bytes(), nil
}

// pktLinesToPayload strips pkt-line framing and returns concatenated payload lines separated by \n.
func pktLinesToPayload(b []byte) (string, error) {
	var lines []string
	i := 0
	for i+4 <= len(b) {
		n, err := strconv.ParseInt(string(b[i:i+4]), 16, 64)
		if err != nil {
			// best-effort: stop parsing, return what we have
			break
		}
		i += 4
		if n == 0 {
			break
		}
		if n < 4 || i+int(n-4) > len(b) {
			// best-effort: stop parsing, return what we have
			break
		}
		lines = append(lines, string(b[i:i+int(n-4)]))
		i += int(n - 4)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("no pkt lines parsed")
	}
	return strings.Join(lines, "\n"), nil
}

// stripPktPayload returns concatenated pkt-line payload, tolerant to malformed lengths.
func stripPktPayload(b []byte) []byte {
	var out []byte
	i := 0
	for i+4 <= len(b) {
		n, err := strconv.ParseInt(string(b[i:i+4]), 16, 64)
		if err != nil {
			break
		}
		i += 4
		if n == 0 {
			break
		}
		if n < 4 || i+int(n-4) > len(b) {
			break
		}
		out = append(out, b[i:i+int(n-4)]...)
		out = append(out, '\n')
		i += int(n - 4)
	}
	if len(out) == 0 {
		return b
	}
	return out
}

func servePackFile(w http.ResponseWriter, path string, log *slog.Logger) bool {
	f, err := os.Open(path)
	if err != nil {
		log.Debug("pack cache open failed", "path", path, "err", err)
		return false
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	// X-Git-Proxy-Status left unset for cached path
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		log.Debug("pack cache copy failed", "path", path, "err", err)
		return false
	}
	return true
}
