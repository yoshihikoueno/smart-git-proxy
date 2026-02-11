package gitproxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
