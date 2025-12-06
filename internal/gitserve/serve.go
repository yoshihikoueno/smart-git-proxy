package gitserve

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ServeInfoRefs handles GET /info/refs?service=git-upload-pack
// It runs git-upload-pack --stateless-rpc --advertise-refs and adds the pkt-line header.
func ServeInfoRefs(w http.ResponseWriter, r *http.Request, repoPath string, cacheStatus string) error {
	start := time.Now()
	log := slog.Default()

	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" {
		http.Error(w, "unsupported service", http.StatusBadRequest)
		return fmt.Errorf("unsupported service: %s", service)
	}

	// Check for Git protocol version
	gitProtocol := r.Header.Get("Git-Protocol")
	isV2 := strings.Contains(gitProtocol, "version=2")

	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	if cacheStatus != "" {
		w.Header().Set("X-Git-Proxy-Status", cacheStatus)
	}
	w.WriteHeader(http.StatusOK)

	// For protocol v1, write the service announcement
	// Protocol v2 doesn't need this prefix
	if !isV2 {
		// Write pkt-line service announcement
		// Format: 4-digit hex length + "# service=git-upload-pack\n" + flush
		announcement := "# service=git-upload-pack\n"
		pktLine := fmt.Sprintf("%04x%s", len(announcement)+4, announcement)
		if _, err := w.Write([]byte(pktLine)); err != nil {
			return err
		}
		// Flush packet (0000)
		if _, err := w.Write([]byte("0000")); err != nil {
			return err
		}
	}

	// Run git upload-pack to get refs
	cmdStart := time.Now()
	cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", repoPath)
	cmd.Env = gitEnv(gitProtocol)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git upload-pack: %w", err)
	}
	log.Debug("git upload-pack started (advertise-refs)", "path", repoPath, "startup_duration_ms", time.Since(cmdStart).Milliseconds())

	// Stream output to client
	copyStart := time.Now()
	n, err := io.Copy(w, stdout)
	if err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("copy stdout: %w", err)
	}
	log.Debug("git upload-pack output streamed (advertise-refs)", "path", repoPath, "bytes", n, "copy_duration_ms", time.Since(copyStart).Milliseconds())

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait git upload-pack: %w, stderr: %s", err, stderrBuf.String())
	}
	log.Debug("git upload-pack complete (advertise-refs)", "path", repoPath, "total_duration_ms", time.Since(start).Milliseconds())

	return nil
}

// ServeUploadPack handles POST /git-upload-pack
// It runs git-upload-pack --stateless-rpc with the request body as stdin.
func ServeUploadPack(w http.ResponseWriter, r *http.Request, repoPath string, cacheStatus string) error {
	start := time.Now()
	log := slog.Default()

	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	if cacheStatus != "" {
		w.Header().Set("X-Git-Proxy-Status", cacheStatus)
	}

	// Handle gzip-compressed request body
	var body io.Reader = r.Body
	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		gzStart := time.Now()
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		body = gz
		log.Debug("gzip reader initialized", "path", repoPath, "duration_ms", time.Since(gzStart).Milliseconds())
	}

	cmdStart := time.Now()
	cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", repoPath)
	cmd.Stdin = body
	cmd.Env = gitEnv(r.Header.Get("Git-Protocol"))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git upload-pack: %w", err)
	}
	log.Debug("git upload-pack started", "path", repoPath, "startup_duration_ms", time.Since(cmdStart).Milliseconds())

	// Stream stdout to response
	w.WriteHeader(http.StatusOK)
	copyStart := time.Now()
	n, err := io.Copy(w, stdout)
	if err != nil {
		_ = cmd.Wait()
		return fmt.Errorf("copy stdout: %w, stderr: %s", err, stderrBuf.String())
	}
	log.Debug("git upload-pack output streamed", "path", repoPath, "bytes", n, "copy_duration_ms", time.Since(copyStart).Milliseconds())

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait git upload-pack: %w, stderr: %s", err, stderrBuf.String())
	}
	log.Debug("git upload-pack complete", "path", repoPath, "total_duration_ms", time.Since(start).Milliseconds())

	return nil
}

// gitEnv returns a minimal environment for local git commands.
// Isolates from user/system git config to avoid interference.
func gitEnv(gitProtocol string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if gitProtocol != "" {
		env = append(env, "GIT_PROTOCOL="+gitProtocol)
	}
	return env
}
