package gitproxy

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crohr/smart-git-proxy/internal/config"
	"github.com/crohr/smart-git-proxy/internal/logging"
	"github.com/crohr/smart-git-proxy/internal/metrics"
	"github.com/crohr/smart-git-proxy/internal/mirror"
)

// Integration test: depth=1 fetch twice, ensure second uses cache (no error, cache populated).
func TestPackCacheDepth1Fetch(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}

	// Setup temp dirs
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream-src")
	mirrorDir := filepath.Join(root, "mirror")
	clientDir := filepath.Join(root, "client")

	makeUpstreamRepo(t, upstream)

	cfg := &config.Config{
		ListenAddr:           ":0",
		AllowedUpstreams:     []string{"localhost"},
		MirrorDir:            mirrorDir,
		SyncStaleAfter:       2 * time.Second,
		AuthMode:             "none",
		LogLevel:             "debug",
		UploadPackThreads:    2,
		SerializeUploadPack:  true,
		EnablePackCache:      true,
		MaintainAfterSync:    false,
		MirrorMaxSize:        config.SizeSpec{},
		Route53HostedZoneID:  "",
		Route53RecordName:    "",
		AWSCloudMapServiceID: "",
	}

	logger, err := logging.New(cfg.LogLevel)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	m, err := mirror.New(cfg.MirrorDir, cfg.SyncStaleAfter, cfg.MirrorMaxSize, cfg.UploadPackThreads, cfg.MaintainAfterSync, logger)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}

	metricsRegistry := metrics.New()
	srv := New(cfg, m, logger, metricsRegistry)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Pre-seed mirror to avoid upstream network access; mark lastSync as fresh
	host, owner, repo := "localhost", "org", "repo"
	repoKey := host + "/" + owner + "/" + repo
	mirrorPath := m.RepoPath(host, owner, repo)
	if err := os.MkdirAll(filepath.Dir(mirrorPath), 0o755); err != nil {
		t.Fatalf("mkdir mirror parent: %v", err)
	}
	// clone --mirror from file:// upstream into mirror path
	mustRun(t, "", "git", "clone", "--mirror", upstream, mirrorPath)
	m.SetLastSync(repoKey, time.Now())

	targetURL := ts.URL + "/localhost/" + owner + "/" + repo + ".git"

	// First fetch (uses mirror hit, populates cache)
	doFetch(t, clientDir, targetURL, "dev")

	// Second fetch should hit cache; ensure it succeeds
	doFetch(t, clientDir, targetURL, "dev")
}

func makeUpstreamRepo(t *testing.T, path string) {
	t.Helper()
	mustRun(t, "", "git", "init", path)
	mustRun(t, path, "sh", "-c", "echo first > file.txt")
	mustRun(t, path, "git", "add", "file.txt")
	mustRun(t, path, "git", "commit", "-m", "first")
	mustRun(t, path, "sh", "-c", "echo second >> file.txt")
	mustRun(t, path, "git", "add", "file.txt")
	mustRun(t, path, "git", "commit", "-m", "second")
	mustRun(t, path, "git", "branch", "-M", "dev")
}

func doFetch(t *testing.T, clientDir, proxyURL, branch string) {
	t.Helper()
	cloneDir := filepath.Join(clientDir, "clone-"+branch+"-"+time.Now().Format("150405.000"))
	args := []string{
		"-c", "protocol.version=2",
		"fetch", "--no-tags", "--prune", "--no-recurse-submodules", "--depth=1",
		proxyURL, "+refs/heads/" + branch + ":refs/remotes/origin/" + branch,
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = cloneDir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	// Need a worktree: init first
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}
	mustRun(t, cloneDir, "git", "init")
	mustRun(t, cloneDir, "git", "remote", "add", "origin", proxyURL)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fetch failed: %v\n%s", err, string(out))
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd %s %s failed: %v\n%s", name, strings.Join(args, " "), err, string(out))
	}
}
