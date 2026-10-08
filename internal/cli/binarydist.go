package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// deployTeployBinaryToServer fetches the teploy release binary matching the
// target server's platform and uploads it there. Used by `teploy autodeploy
// setup` (internal/cli/autodeploy.go) to install the binary `teploy
// autodeploy serve` needs to run as a resident process on the server —
// reuses the exact same fetch/verify/extract pipeline as `teploy update`
// (update.go's fetchLatestRelease/downloadToBytes/checksumFor/
// extractBinary), just targeting the server's platform instead of the
// operator's local one, and uploading instead of self-replacing.
func deployTeployBinaryToServer(ctx context.Context, exec ssh.Executor, remotePath string, capabilities ...string) (version string, err error) {
	goos, goarch, err := serverPlatform(ctx, exec)
	if err != nil {
		return "", fmt.Errorf("detecting server platform: %w", err)
	}

	latest, err := fetchLatestRelease(ctx)
	if err != nil {
		return "", fmt.Errorf("checking latest teploy release: %w", err)
	}
	latestVersion := strings.TrimPrefix(latest.TagName, "v")

	// goreleaser publishes archives named teploy_{os}_{arch}.tar.gz plus a
	// checksums.txt — matches update.go's runUpdate exactly, just with the
	// server's goos/goarch instead of runtime.GOOS/runtime.GOARCH. Windows
	// servers aren't a supported target (teploy assumes a Linux/systemd
	// host throughout — see internal/harden), so there's no zip case here.
	const ext = "tar.gz"
	const binName = "teploy"
	assetName := fmt.Sprintf("teploy_%s_%s.%s", goos, goarch, ext)

	archiveURL := fmt.Sprintf("%s/%s/%s", downloadBase, latest.TagName, assetName)
	checksumURL := fmt.Sprintf("%s/%s/checksums.txt", downloadBase, latest.TagName)

	archive, err := downloadToBytes(ctx, archiveURL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", assetName, err)
	}
	checksums, err := downloadToBytes(ctx, checksumURL)
	if err != nil {
		return "", fmt.Errorf("downloading checksums: %w", err)
	}
	want, err := checksumFor(checksums, assetName)
	if err != nil {
		return "", err
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("checksum mismatch for %s — refusing to install (want %s, got %s)", assetName, want, got)
	}

	binData, err := extractBinary(archive, ext, binName)
	if err != nil {
		return "", fmt.Errorf("extracting binary: %w", err)
	}

	if err := installServerBinary(ctx, exec, remotePath, binData, latestVersion, capabilities...); err != nil {
		return "", err
	}

	return latestVersion, nil
}

// serverPlatform runs uname on the target and maps the result to Go's
// GOOS/GOARCH naming, matching goreleaser's asset naming convention
// (teploy_<goos>_<goarch>.tar.gz).
func serverPlatform(ctx context.Context, exec ssh.Executor) (goos, goarch string, err error) {
	unameS, err := exec.Run(ctx, "uname -s")
	if err != nil {
		return "", "", fmt.Errorf("running uname -s: %w", err)
	}
	switch strings.TrimSpace(unameS) {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", fmt.Errorf("unsupported server OS %q — teploy autodeploy serve requires Linux (or Darwin for local testing)", strings.TrimSpace(unameS))
	}

	unameM, err := exec.Run(ctx, "uname -m")
	if err != nil {
		return "", "", fmt.Errorf("running uname -m: %w", err)
	}
	switch strings.TrimSpace(unameM) {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", fmt.Errorf("unsupported server architecture %q", strings.TrimSpace(unameM))
	}

	return goos, goarch, nil
}

// installServerBinary validates a private candidate before one atomic,
// serialized publication. The incumbent is never replaced by a refused
// candidate; privileged destinations use the supported passwordless sudo path.
func installServerBinary(ctx context.Context, executor ssh.Executor, destination string, data []byte, version string, capabilities ...string) error {
	if !path.IsAbs(destination) || path.Clean(destination) != destination {
		return fmt.Errorf("binary destination must be a clean absolute path")
	}
	out, err := executor.Run(ctx, "umask 077; mktemp -d /tmp/teploy-install.XXXXXXXX")
	if err != nil {
		return fmt.Errorf("creating binary staging directory: %w", err)
	}
	dir := strings.TrimSpace(out)
	if !strings.HasPrefix(dir, "/tmp/teploy-install.") || strings.ContainsAny(dir, "\n\r") || path.Dir(dir) != "/tmp" {
		return fmt.Errorf("invalid binary staging directory")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = executor.Run(cleanup, "rm -rf -- "+ssh.ShellQuote(dir))
	}()
	candidate := path.Join(dir, "teploy")
	if err := executor.Upload(ctx, bytes.NewReader(data), candidate, "0700"); err != nil {
		return fmt.Errorf("uploading teploy candidate: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	actual, err := executor.Run(checkCtx, ssh.ShellQuote(candidate)+" version")
	if err != nil {
		return fmt.Errorf("candidate binary failed to run: %w", err)
	}
	if strings.TrimSpace(actual) != "teploy "+version {
		return fmt.Errorf("candidate version mismatch: expected teploy %s", version)
	}
	for _, capability := range capabilities {
		var args []string
		for _, arg := range strings.Fields(capability) {
			args = append(args, ssh.ShellQuote(arg))
		}
		if len(args) == 0 {
			return fmt.Errorf("empty binary capability")
		}
		if _, err := executor.Run(checkCtx, ssh.ShellQuote(candidate)+" "+strings.Join(args, " ")+" --help >/dev/null 2>&1"); err != nil {
			return fmt.Errorf("candidate does not support %s: %w", capability, err)
		}
	}
	prefix := ""
	if strings.HasPrefix(destination, "/usr/") || strings.HasPrefix(destination, "/opt/") {
		prefix = sudoPrefixFor(ctx, executor)
		if prefix != "" {
			prefix = "sudo -n "
		}
	}
	script := fmt.Sprintf(`set -eu
stage=$(mktemp %s)
trap 'rm -f -- "$stage"' EXIT HUP INT TERM
cp -- %s "$stage"
chmod 0755 "$stage"
mv -f -- "$stage" %s`, ssh.ShellQuote(path.Join(path.Dir(destination), ".teploy-candidate.XXXXXXXX")), ssh.ShellQuote(candidate), ssh.ShellQuote(destination))
	// A unique sibling gives atomic rename on the destination filesystem.
	// flock prevents concurrent installers from interleaving publication.
	command := prefix + "flock -w 30 " + ssh.ShellQuote(destination+".install.lock") + " sh -c " + ssh.ShellQuote(script)
	if _, err := executor.Run(ctx, command); err != nil {
		return fmt.Errorf("publishing verified binary: %w", err)
	}
	return nil
}
