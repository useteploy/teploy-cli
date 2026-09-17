package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// getLocalLayers returns the layer diff IDs for a local Docker image.
func getLocalLayers(ctx context.Context, tag string) ([]string, error) {
	out, err := osexec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .RootFS.Layers}}", tag).Output()
	if err != nil {
		return nil, fmt.Errorf("inspecting local image: %w", err)
	}
	var layers []string
	if err := json.Unmarshal(out, &layers); err != nil {
		return nil, fmt.Errorf("parsing layers: %w", err)
	}
	return layers, nil
}

// getRemoteLayers returns layer diff IDs for an image on the server.
func getRemoteLayers(ctx context.Context, exec ssh.Executor, tag string) ([]string, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("docker inspect --format '{{json .RootFS.Layers}}' %s", tag))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	var layers []string
	if err := json.Unmarshal([]byte(out), &layers); err != nil {
		return nil, fmt.Errorf("parsing remote layers: %w", err)
	}
	return layers, nil
}

// findPreviousImage finds the most recent app build image on the server.
func findPreviousImage(ctx context.Context, exec ssh.Executor, app string) string {
	out, _ := exec.Run(ctx, fmt.Sprintf(
		"docker images --filter reference='%s-build-*' --format '{{.Repository}}:{{.Tag}}' | head -1",
		app,
	))
	return strings.TrimSpace(out)
}

// localImageID returns the local image's immutable content ID.
func localImageID(ctx context.Context, tag string) (string, error) {
	out, err := osexec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", tag).Output()
	if err != nil {
		return "", fmt.Errorf("inspecting local image: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// remoteImageID returns the server-side image's immutable content ID.
func remoteImageID(ctx context.Context, exec ssh.Executor, tag string) (string, error) {
	out, err := exec.Run(ctx, fmt.Sprintf("docker image inspect --format '{{.Id}}' %s", ssh.ShellQuote(tag)))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// LayerOptimizedTransfer compresses the image with gzip before streaming.
// Reports layer statistics for user feedback.
// Returns an error if optimization isn't applicable (caller falls back to full transfer).
//
// The skip-transfer check compares the local and remote images' immutable
// IDs (`docker image inspect`, not the broader `docker inspect` namespace):
// the old "docker inspect <tag> succeeded means it's there" check treated
// the mutable TAG as identity, so rebuilding the same version after a
// working-tree or base-image change skipped the transfer and deployed the
// stale remote bytes under the new build's name (audit F55).
func LayerOptimizedTransfer(ctx context.Context, tag, app, host, user, keyPath string, exec ssh.Executor, stdout io.Writer) error {
	// 1. Skip only when the server provably has the EXACT same bytes.
	if localID, err := localImageID(ctx, tag); err == nil {
		if remoteID, rErr := remoteImageID(ctx, exec, tag); rErr == nil && remoteID != "" && remoteID == localID {
			fmt.Fprintln(stdout, "  Image already on server with identical content — skipping transfer")
			return nil
		}
	}

	// 2. Find previous image and report layer stats.
	prevTag := findPreviousImage(ctx, exec, app)
	if prevTag != "" && prevTag != tag+":latest" {
		localLayers, localErr := getLocalLayers(ctx, tag)
		remoteLayers, remoteErr := getRemoteLayers(ctx, exec, prevTag)

		if localErr == nil && remoteErr == nil {
			shared := countSharedLayers(localLayers, remoteLayers)
			newCount := len(localLayers) - shared
			if shared > 0 {
				fmt.Fprintf(stdout, "  Layer stats: %d/%d layers shared with previous build, %d new\n",
					shared, len(localLayers), newCount)
			}
		}
	}

	// 3. Use gzip-compressed transfer: docker save | gzip | ssh "gunzip | docker load"
	fmt.Fprintln(stdout, "  Streaming compressed image to server...")

	// Strict host-key verification for the transfer channel (audit F27):
	// a securely verified control connection does not authenticate this
	// separate ssh(1) process. accept-new mirrors the control connection's
	// policy; by transfer time it has already recorded the host key.
	acceptNew := false
	if a, ok := exec.(interface{ AcceptNewHost() bool }); ok && a.AcceptNewHost() {
		acceptNew = true
	}
	sshArgs := ssh.ExternalSSHArgs(host, keyPath, acceptNew)
	sshTarget := ssh.RsyncTarget(user, host, "")
	sshTarget = strings.TrimSuffix(sshTarget, ":") // this channel dials a command, not rsync
	sshArgs = append(sshArgs, sshTarget, "gunzip | docker load")

	save := osexec.CommandContext(ctx, "docker", "save", tag)
	gzip := osexec.CommandContext(ctx, "gzip", "-1") // fast compression
	load := osexec.CommandContext(ctx, "ssh", sshArgs...)

	// Pipeline: docker save | gzip | ssh "gunzip | docker load"
	savePipe, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating save pipe: %w", err)
	}
	gzip.Stdin = savePipe

	gzipPipe, err := gzip.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating gzip pipe: %w", err)
	}
	load.Stdin = gzipPipe
	load.Stdout = stdout
	load.Stderr = stdout

	// Check if gzip is available locally.
	if _, err := osexec.LookPath("gzip"); err != nil {
		return fmt.Errorf("gzip not found locally")
	}

	if err := save.Start(); err != nil {
		return fmt.Errorf("starting docker save: %w", err)
	}
	if err := gzip.Start(); err != nil {
		save.Process.Kill()
		return fmt.Errorf("starting gzip: %w", err)
	}
	if err := load.Start(); err != nil {
		save.Process.Kill()
		gzip.Process.Kill()
		return fmt.Errorf("starting ssh load: %w", err)
	}

	saveErr := save.Wait()
	gzipErr := gzip.Wait()
	loadErr := load.Wait()

	if saveErr != nil {
		return fmt.Errorf("docker save: %w", saveErr)
	}
	if gzipErr != nil {
		return fmt.Errorf("gzip: %w", gzipErr)
	}
	if loadErr != nil {
		return fmt.Errorf("ssh docker load: %w", loadErr)
	}

	return nil
}

// countSharedLayers counts the number of matching layers from the start.
func countSharedLayers(a, b []string) int {
	shared := 0
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			shared++
		} else {
			break
		}
	}
	return shared
}

// humanSize formats bytes into human-readable format.
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// TempFileSize returns the size of a docker save output for the given tag.
func TempFileSize(ctx context.Context, tag string) (int64, error) {
	f, err := os.CreateTemp("", "teploy-size-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	cmd := osexec.CommandContext(ctx, "docker", "save", "-o", f.Name(), tag)
	if err := cmd.Run(); err != nil {
		return 0, err
	}

	info, err := os.Stat(f.Name())
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
