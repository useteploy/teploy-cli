package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

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
func deployTeployBinaryToServer(ctx context.Context, exec ssh.Executor, remotePath string) (version string, err error) {
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

	// Upload to a staging path and rename into place, rather than writing
	// remotePath directly: if `teploy autodeploy serve` is already running
	// from remotePath (re-running `autodeploy setup` to pick up a newer
	// release), an SFTP write straight to that path fails with ETXTBSY
	// ("text file busy") — Linux refuses to open-for-write a file that's
	// currently mapped as a running executable. rename() has no such
	// restriction: it only touches the directory entry, so it succeeds
	// even while the old inode is still executing. The running process
	// keeps its old code until the systemd restart later in Setup picks
	// up the new binary at the same path. Found live: re-running
	// `autodeploy setup` against an already-configured app failed outright.
	stagingPath := remotePath + ".new"
	if err := exec.Upload(ctx, bytes.NewReader(binData), stagingPath, "0755"); err != nil {
		return "", fmt.Errorf("uploading teploy binary: %w", err)
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("mv %s %s", ssh.ShellQuote(stagingPath), ssh.ShellQuote(remotePath))); err != nil {
		return "", fmt.Errorf("installing teploy binary: %w", err)
	}

	// Sanity check the uploaded binary actually runs before wiring a
	// systemd unit up to depend on it.
	if _, err := exec.Run(ctx, ssh.ShellQuote(remotePath)+" version"); err != nil {
		return "", fmt.Errorf("uploaded binary failed to run: %w", err)
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
