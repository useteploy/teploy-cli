package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	githubRepo   = "useteploy/teploy"
	releaseAPI   = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	downloadBase = "https://github.com/" + githubRepo + "/releases/download"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

func newUpdateCmd(currentVersion string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update teploy to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(currentVersion, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "force update even if already on latest")

	return cmd
}

func runUpdate(currentVersion string, force bool) error {
	fmt.Printf("Current version: %s\n", currentVersion)

	// Fetch latest release info.
	fmt.Println("Checking for updates...")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	latest, err := fetchLatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	latestVersion := strings.TrimPrefix(latest.TagName, "v")
	fmt.Printf("Latest version: %s\n", latestVersion)

	if !force && latestVersion == currentVersion {
		fmt.Println("Already up to date")
		return nil
	}

	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// goreleaser publishes archives named teploy_{os}_{arch}.tar.gz (zip on
	// Windows) plus a checksums.txt — not bare per-platform binaries.
	ext := "tar.gz"
	binName := "teploy"
	if goos == "windows" {
		ext = "zip"
		binName = "teploy.exe"
	}
	assetName := fmt.Sprintf("teploy_%s_%s.%s", goos, goarch, ext)

	archiveURL := fmt.Sprintf("%s/%s/%s", downloadBase, latest.TagName, assetName)
	checksumURL := fmt.Sprintf("%s/%s/checksums.txt", downloadBase, latest.TagName)

	// Download the checksum manifest and the archive.
	fmt.Printf("Downloading %s...\n", assetName)
	archive, err := downloadToBytes(ctx, archiveURL)
	if err != nil {
		return fmt.Errorf("downloading update: %w", err)
	}
	checksums, err := downloadToBytes(ctx, checksumURL)
	if err != nil {
		return fmt.Errorf("downloading checksums: %w", err)
	}

	// Verify the archive against the published checksum before trusting it.
	want, err := checksumFor(checksums, assetName)
	if err != nil {
		return err
	}
	got := fmt.Sprintf("%x", sha256.Sum256(archive))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s — refusing to install (want %s, got %s)", assetName, want, got)
	}
	fmt.Println("  Checksum verified")

	// Extract the teploy binary from the archive.
	binData, err := extractBinary(archive, ext, binName)
	if err != nil {
		return fmt.Errorf("extracting binary: %w", err)
	}

	// Write to a temp file, make executable, and sanity-check it runs.
	tmpFile, err := os.CreateTemp("", "teploy-update-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.Write(binData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing update: %w", err)
	}
	tmpFile.Close()

	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}

	out, err := exec.Command(tmpPath, "version").Output()
	if err != nil {
		return fmt.Errorf("downloaded binary is invalid: %w", err)
	}
	fmt.Printf("  Verified: %s", out)

	currentBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding current binary: %w", err)
	}

	fmt.Printf("Replacing %s...\n", currentBinary)
	if err := replaceBinary(tmpPath, currentBinary); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("Updated to %s\n", latestVersion)
	return nil
}

func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "teploy-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("no releases found — update manually from GitHub")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parsing release info: %w", err)
	}
	return &release, nil
}

// downloadToBytes fetches url into memory, following the redirect GitHub issues
// to its asset CDN.
func downloadToBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "teploy-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s returned status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// checksumFor returns the hex sha256 recorded for asset in a goreleaser
// checksums.txt ("<sha256>  <filename>" per line).
func checksumFor(checksums []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && path.Base(fields[1]) == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s — refusing to install unverified binary", asset)
}

// extractBinary pulls binName out of a tar.gz or zip archive held in memory.
func extractBinary(archive []byte, ext, binName string) ([]byte, error) {
	if ext == "zip" {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == binName {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("%s not found in archive", binName)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if path.Base(hdr.Name) == binName {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%s not found in archive", binName)
}

func replaceBinary(src, dst string) error {
	// Read the new binary.
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	// Write to destination (overwrite).
	if err := os.WriteFile(dst, data, 0755); err != nil {
		// If permission denied, suggest sudo.
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied — try: sudo cp %s %s", src, dst)
		}
		return err
	}
	return nil
}
