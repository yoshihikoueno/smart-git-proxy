package mirror

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crohr/smart-git-proxy/internal/config"
	"github.com/hashicorp/go-set/v3"
	"golang.org/x/sync/singleflight"
)

// Status indicates what happened during EnsureRepo
type Status string

const (
	StatusHit   Status = "mirror-hit"   // Served from existing fresh mirror
	StatusClone Status = "mirror-clone" // Had to clone new mirror
	StatusSync  Status = "mirror-sync"  // Had to sync stale mirror
)

// Mirror manages bare git repository mirrors.
type Mirror struct {
	root              string
	staleAfter        time.Duration
	log               *slog.Logger
	cache             *Cache
	packThreads       int
	maintainAfterSync bool

	group      singleflight.Group
	maintGroup singleflight.Group
	lastSync   sync.Map // map[repoKey]time.Time
	repoLocks  sync.Map // map[repoKey]*sync.Mutex
}

// New creates a new Mirror manager.
// maxSize is the maximum cache size (absolute or percentage, zero = 80% of available disk).
func New(root string, staleAfter time.Duration, maxSize config.SizeSpec, packThreads int, maintainAfterSync bool, log *slog.Logger) (*Mirror, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create mirror root: %w", err)
	}
	return &Mirror{
		root:              root,
		staleAfter:        staleAfter,
		log:               log,
		cache:             NewCache(root, maxSize, log),
		packThreads:       packThreads,
		maintainAfterSync: maintainAfterSync,
	}, nil
}

// RepoPath returns the filesystem path for a repo mirror.
func (m *Mirror) RepoPath(host, owner, repo string) string {
	return filepath.Join(m.root, host, owner, repo+".git")
}

// EnsureRepo ensures the mirror exists and is synced.
// authHeader is the Authorization header value from the client request (can be empty).
// Returns the path to the bare repo and the cache status.
func (m *Mirror) EnsureRepo(ctx context.Context, host, owner, repo, upstreamURL, authHeader string) (string, Status, error) {
	start := time.Now()
	repoPath := m.RepoPath(host, owner, repo)
	key := fmt.Sprintf("%s/%s/%s", host, owner, repo)

	m.log.Debug("ensure repo started", "repo", key)

	// Use singleflight for clone to handle the race where:
	// 1. Client A sees repo doesn't exist, starts clone
	// 2. Git creates the directory (but clone isn't done)
	// 3. Client B sees directory exists, skips singleflight, tries to serve incomplete repo
	// By always going through singleflight for clone, Client B will wait for Client A's clone to complete.
	cloneCheckStart := time.Now()
	result, err, shared := m.group.Do("clone:"+key, func() (interface{}, error) {
		// Check inside singleflight to avoid TOCTOU race
		if _, err := os.Stat(repoPath); os.IsNotExist(err) {
			if err := m.cloneRepo(ctx, repoPath, upstreamURL, authHeader); err != nil {
				return StatusClone, err
			}
			m.lastSync.Store(key, time.Now())
			m.cache.Touch(key)
			// Trigger LRU eviction check in background after clone
			go m.cache.MaybeEvict()
			return StatusClone, nil
		}
		// Repo already exists, signal that no clone was needed
		return StatusHit, nil
	})
	m.log.Debug("clone check complete", "repo", key, "duration_ms", time.Since(cloneCheckStart).Milliseconds(), "shared", shared)
	if err != nil {
		return "", "", err
	}
	status := result.(Status)
	if shared {
		m.log.Info("waited for in-flight clone check", "repo", key, "status", status, "wait_duration_ms", time.Since(cloneCheckStart).Milliseconds())
	}
	if status == StatusClone {
		m.log.Debug("ensure repo complete (clone)", "repo", key, "total_duration_ms", time.Since(start).Milliseconds())
		return repoPath, status, nil
	}

	// Touch cache on access (for LRU tracking)
	m.cache.Touch(key)

	// Check if we need to sync first - sync validates auth implicitly via git fetch
	// This avoids a separate ls-remote call (~110ms) when we're going to fetch anyway
	status = StatusHit
	if m.isStale(key) {
		syncStart := time.Now()
		// Sync using singleflight (concurrent requests share same fetch)
		_, err, shared := m.group.Do("sync:"+key, func() (interface{}, error) {
			return nil, m.syncRepo(ctx, repoPath, upstreamURL, authHeader)
		})
		if shared {
			m.log.Debug("waited for in-flight sync", "repo", key, "wait_duration_ms", time.Since(syncStart).Milliseconds())
		}
		if err != nil {
			// Continue serving stale data, but still report as hit
			m.log.Warn("sync failed, serving stale", "repo", key, "err", err, "duration_ms", time.Since(syncStart).Milliseconds())
		} else {
			status = StatusSync
			m.lastSync.Store(key, time.Now())
			m.log.Debug("ensure repo complete (sync)", "repo", key, "sync_duration_ms", time.Since(syncStart).Milliseconds(), "total_duration_ms", time.Since(start).Milliseconds())

			if m.maintainAfterSync {
				m.scheduleOptimize(repoPath, false)
			}
		}

		return repoPath, StatusSync, nil
	} else {
		m.log.Debug("ensure repo complete (hit)", "repo", key, "total_duration_ms", time.Since(start).Milliseconds())
	}

	// Repo is fresh - validate auth only for private repos (cache hit case)
	// If sync was successful, authentication validity is already guaranteed
	if m.requiresAuth(repoPath) && status != StatusSync {
		authStart := time.Now()
		if err := m.validateAuth(ctx, upstreamURL, authHeader); err != nil {
			m.log.Warn("auth validation failed", "repo", key, "err", err, "duration_ms", time.Since(authStart).Milliseconds())
			return "", "", fmt.Errorf("authentication required: %w", err)
		}
		m.log.Debug("auth validation passed", "repo", key, "duration_ms", time.Since(authStart).Milliseconds())
	}
	return repoPath, status, nil
}

// isStale returns true if the repo needs syncing.
func (m *Mirror) isStale(key string) bool {
	lastSync, ok := m.lastSync.Load(key)
	if !ok {
		return true
	}
	return time.Since(lastSync.(time.Time)) > m.staleAfter
}

// requiresAuth checks if a repo was cloned with authentication.
func (m *Mirror) requiresAuth(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, ".requires-auth"))
	return err == nil
}

// markRequiresAuth marks a repo as requiring authentication.
func (m *Mirror) markRequiresAuth(repoPath string) error {
	return os.WriteFile(filepath.Join(repoPath, ".requires-auth"), []byte("1"), 0o644)
}

// validateAuth validates the auth token can access the upstream repo using git ls-remote.
func (m *Mirror) validateAuth(ctx context.Context, upstreamURL, authHeader string) error {
	start := time.Now()
	parsedUrl, err := url.Parse(upstreamURL)
	if err != nil {
		m.log.Error("auth validation failed", "duration_ms", time.Since(start).Milliseconds(), "upstream", upstreamURL)
		return fmt.Errorf("Failed to parse upstreamURL: %s", err)
	}
	parsedUrl = parsedUrl.JoinPath("info", "refs")
	parsedUrl.Query().Set("service", "git-upload-pack")
	res, err := http.Get(parsedUrl.String())

	if err != nil {
		// When failed to auhenticate due to upstream outage, proceed the process assuming that the auth info is valid
		m.log.Warn("auth validation failed due to upstream outage", "duration_ms", time.Since(start).Milliseconds(), "upstream", upstreamURL)
		if ok, err := m.checkAuthCache(authHeader); err != nil {
			return fmt.Errorf("auth cache not available: %s", err)
		} else if ok {
			m.log.Info("authenticated using auth cache")
		} else {
			return fmt.Errorf("Failed to authenticate using auth cache: %s", err)
		}
	} else if res.StatusCode == http.StatusUnauthorized {
		m.log.Error("auth validation failed", "duration_ms", time.Since(start).Milliseconds(), "upstream", upstreamURL)
		m.removeAuthCache(authHeader)
		return fmt.Errorf("git ls-remote failed: %w\n", err)
	} else {
		m.addAuthCache(authHeader)
		m.log.Debug("auth validation complete", "duration_ms", time.Since(start).Milliseconds(), "upstream", upstreamURL)
	}
	return nil
}

func (m *Mirror) addAuthCache(authHeader string) error {
	var s *set.Set[[20]byte]
	var err error
	if s, err = m.getAuthCache(); err != nil {
		m.log.Debug("get auth cache failed")
		s = set.New[[20]byte](1)
	}
	s.Insert(sha1.Sum([]byte(authHeader)))
	return m.storeAuthCache(s)
}

func (m *Mirror) checkAuthCache(authHeader string) (bool, error) {
	var s *set.Set[[20]byte]
	var err error
	if s, err = m.getAuthCache(); err != nil {
		m.log.Warn("auth cache retrieval failed")
		return false, err
	}
	return s.Contains(sha1.Sum([]byte(authHeader))), nil
}

func (m *Mirror) removeAuthCache(authHeader string) error {
	var s *set.Set[[20]byte]
	var err error
	if s, err = m.getAuthCache(); err != nil {
		m.log.Warn("auth cache retrieval failed")
		return err
	}
	s.Remove(sha1.Sum([]byte(authHeader)))
	return m.storeAuthCache(s)
}

func (m *Mirror) storeAuthCache(s *set.Set[[20]byte]) error {
	if blob, err := json.Marshal(s); err != nil {
		m.log.Warn("auth cache marshal failed")
		return err
	} else {
		path := filepath.Join(m.root, ".auth-cace.json")
		return os.WriteFile(path, blob, 0o600)
	}
}

func (m *Mirror) getAuthCache() (*set.Set[[20]byte], error) {
	path := filepath.Join(m.root, ".auth-cace.json")
	var blob []byte
	var err error
	s := set.New[[20]byte](1)
	if blob, err = os.ReadFile(path); err != nil {
		m.log.Warn("auth cache retrieval failed")
		return nil, err
	}

	if err := s.UnmarshalJSON(blob); err != nil {
		m.log.Warn("auth cache unmarshal failed")
		return nil, err
	}
	return s, nil
}

func (m *Mirror) validateAuthUsingCache(authHeader string) (bool, error) {
	if s, err := m.getAuthCache(); err != nil {
		m.log.Warn("auth cache retrieval failed")
		return false, err
	} else {
		return s.Contains(sha1.Sum([]byte(authHeader))), nil
	}
}

// cloneRepo creates a new bare mirror.
func (m *Mirror) cloneRepo(ctx context.Context, repoPath, upstreamURL, authHeader string) error {
	start := time.Now()
	m.log.Info("cloning mirror", "path", repoPath, "upstream", upstreamURL, "hasAuth", authHeader != "")

	// Create parent directory
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	m.log.Debug("parent directory ready", "duration_ms", time.Since(start).Milliseconds())

	// Disable GC and reduce memory pressure for large repos
	args := []string{
		"-c", "gc.auto=0",
		"-c", "core.compression=0",
		"-c", "pack.window=0",
		"-c", "pack.depth=0",
		"-c", "pack.deltaCacheSize=1",
		"-c", "pack.threads=1",
		"clone", "--bare", "--mirror", upstreamURL, repoPath,
	}

	cloneStart := time.Now()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv(authHeader)
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.log.Debug("git clone failed", "duration_ms", time.Since(cloneStart).Milliseconds(), "path", repoPath)
		return fmt.Errorf("git clone failed: %w\noutput: %s", err, output)
	}
	m.log.Debug("git clone command complete", "duration_ms", time.Since(cloneStart).Milliseconds(), "path", repoPath)

	// Mark repo as requiring auth if it was cloned with auth
	if authHeader != "" {
		if err := m.markRequiresAuth(repoPath); err != nil {
			m.log.Warn("failed to mark repo as requiring auth", "path", repoPath, "err", err)
		}
	}

	m.log.Info("clone complete", "path", repoPath, "total_duration_ms", time.Since(start).Milliseconds())

	// Optimize repo in background (bitmap index, commit-graph, maintenance)
	m.scheduleOptimize(repoPath, true)

	return nil
}

// optimizeRepo runs maintenance tasks; if full is true, run repack+bitmap, otherwise only midx+commit-graph.
// Should be called in background after clone to not block the first request.
func (m *Mirror) optimizeRepo(ctx context.Context, repoPath string, full bool) {
	start := time.Now()
	m.log.Debug("optimizing repo", "path", repoPath, "full", full)

	// Avoid lock contention if another git process is writing commit-graph
	lockPath := filepath.Join(repoPath, "objects", "info", "commit-graph.lock")
	if _, err := os.Stat(lockPath); err == nil {
		m.log.Debug("commit-graph lock present, skipping maintenance", "path", repoPath)
		return
	}

	if full {
		repackStart := time.Now()
		args := []string{"-C", repoPath, "repack", "-a", "-d", "-b", "--write-bitmap-index"}
		if m.packThreads > 0 {
			args = append([]string{"-c", fmt.Sprintf("pack.threads=%d", m.packThreads)}, args...)
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		if output, err := cmd.CombinedOutput(); err != nil {
			m.log.Warn("git repack failed", "path", repoPath, "err", err, "output", string(output))
		} else {
			m.log.Debug("git repack complete", "path", repoPath, "duration_ms", time.Since(repackStart).Milliseconds())
		}
	}

	// Commit graph
	graphStart := time.Now()
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "commit-graph", "write", "--reachable")
	if output, err := cmd.CombinedOutput(); err != nil {
		m.log.Warn("git commit-graph write failed", "path", repoPath, "err", err, "output", string(output))
	} else {
		m.log.Debug("git commit-graph complete", "path", repoPath, "duration_ms", time.Since(graphStart).Milliseconds())
	}

	// Multi-pack-index bitmap (Git >=2.43)
	midxStart := time.Now()
	cmd = exec.CommandContext(ctx, "git", "-C", repoPath, "multi-pack-index", "write", "--bitmap")
	if output, err := cmd.CombinedOutput(); err != nil {
		m.log.Warn("git multi-pack-index write failed", "path", repoPath, "err", err, "output", string(output))
	} else {
		m.log.Debug("git multi-pack-index complete", "path", repoPath, "duration_ms", time.Since(midxStart).Milliseconds())
	}

	m.log.Info("repo optimization complete", "path", repoPath, "full", full, "total_duration_ms", time.Since(start).Milliseconds())
}

// syncRepo fetches updates from upstream.
func (m *Mirror) syncRepo(ctx context.Context, repoPath, upstreamURL, authHeader string) error {
	start := time.Now()
	m.log.Debug("syncing mirror", "path", repoPath, "hasAuth", authHeader != "")

	// Disable GC and reduce memory pressure for large repos
	args := []string{
		"-C", repoPath,
		"-c", "gc.auto=0",
		"-c", "core.compression=0",
		"-c", "pack.window=0",
		"-c", "pack.depth=0",
		"-c", "pack.deltaCacheSize=1",
		"-c", "pack.threads=1",
		"fetch", "--all", "--prune", "--force",
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv(authHeader)
	output, err := cmd.CombinedOutput()
	if err != nil {
		m.log.Debug("git fetch failed", "duration_ms", time.Since(start).Milliseconds(), "path", repoPath)
		return fmt.Errorf("git fetch failed: %w\noutput: %s", err, output)
	}

	m.log.Debug("sync complete", "path", repoPath, "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// GetRepoLock returns a mutex for the given repo (for exclusive operations).
func (m *Mirror) GetRepoLock(host, owner, repo string) *sync.Mutex {
	key := fmt.Sprintf("%s/%s/%s", host, owner, repo)
	lock, _ := m.repoLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// MaintainRepo runs maintenance on a given repo key (host/owner/repo).
// If full is true, perform a repack with bitmap; otherwise only midx+commit-graph.
func (m *Mirror) MaintainRepo(ctx context.Context, repoKey string, full bool) error {
	parts := strings.Split(repoKey, "/")
	if len(parts) < 3 {
		return fmt.Errorf("invalid repo key %q, expected host/owner/repo", repoKey)
	}
	repoPath := m.RepoPath(parts[0], parts[1], parts[2])
	if _, err := os.Stat(repoPath); err != nil {
		return fmt.Errorf("repo not found at %s: %w", repoPath, err)
	}
	m.optimizeRepo(ctx, repoPath, full)
	return nil
}

func (m *Mirror) Root() string {
	return m.root
}

// MaintainAll scans mirror root and runs maintenance on every *.git repo.
func (m *Mirror) MaintainAll(ctx context.Context, full bool) error {
	return filepath.WalkDir(m.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasSuffix(d.Name(), ".git") {
			m.optimizeRepo(ctx, p, full)
		}
		return nil
	})
}

// scheduleOptimize runs optimizeRepo with a per-repo singleflight to avoid concurrent maintenance.
func (m *Mirror) scheduleOptimize(repoPath string, full bool) {
	go func() {
		_, err, _ := m.maintGroup.Do(repoPath, func() (interface{}, error) {
			m.optimizeRepo(context.Background(), repoPath, full)
			return nil, nil
		})
		if err != nil {
			m.log.Warn("optimize singleflight failed", "path", repoPath, "err", err)
		}
	}()
}

// SetLastSync is a test helper to seed lastSync for a repo key.
func (m *Mirror) SetLastSync(repoKey string, t time.Time) {
	m.lastSync.Store(repoKey, t)
}

// gitEnv returns environment variables for git commands.
// Uses GIT_CONFIG_* env vars to pass auth without persisting to repo config.
func gitEnv(authHeader string) []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if authHeader != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraheader",
			fmt.Sprintf("GIT_CONFIG_VALUE_0=Authorization: %s", authHeader),
		)
	}
	return env
}
