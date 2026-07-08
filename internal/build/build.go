package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"

	"github.com/useteploy/teploy/internal/ssh"
)

// Mode represents the detected build method.
type Mode int

const (
	ModeNone       Mode = iota // pre-built image, no build needed
	ModeDockerfile             // Dockerfile found
	ModeNixpacks               // no Dockerfile, use Nixpacks
)

func (m Mode) String() string {
	switch m {
	case ModeDockerfile:
		return "dockerfile"
	case ModeNixpacks:
		return "nixpacks"
	default:
		return "none"
	}
}

// Detect examines the directory and returns the appropriate build mode.
// Priority: Dockerfile → Nixpacks fallback.
func Detect(dir string) Mode {
	mode, _ := DetectAt(dir, "")
	return mode
}

// DetectAt resolves the build mode for a given context directory and
// optional Dockerfile (relative to the context; empty means "Dockerfile").
// A Dockerfile at the resolved path selects ModeDockerfile. When the caller
// named a Dockerfile explicitly and it is missing, that is an error rather
// than a silent fall-through to Nixpacks — an explicit path that points at
// nothing is a config mistake worth surfacing. With no explicit Dockerfile
// and none present, it falls back to ModeNixpacks.
func DetectAt(contextDir, dockerfile string) (Mode, error) {
	if contextDir == "" {
		contextDir = "."
	}
	name := dockerfile
	if name == "" {
		name = "Dockerfile"
	}
	resolved := filepath.Join(contextDir, name)
	if _, err := os.Stat(resolved); err == nil {
		return ModeDockerfile, nil
	}
	if dockerfile != "" {
		return ModeNone, fmt.Errorf("dockerfile %q not found (looked at %s)", dockerfile, resolved)
	}
	return ModeNixpacks, nil
}

// ImageTag returns the image tag used for server-built images.
func ImageTag(app, version string) string {
	return app + "-build-" + version
}

// BuildConfig holds parameters for a server-side build.
type BuildConfig struct {
	App      string
	Version  string
	Mode     Mode
	BuildDir string // remote directory containing the synced source
	// Context is the build-context subdirectory relative to BuildDir
	// (empty or "." means BuildDir itself).
	Context string
	// Dockerfile is the Dockerfile path relative to Context (empty means
	// "Dockerfile" in the context root).
	Dockerfile string
	Platform   string // e.g. "linux/arm64" (optional)
}

// Builder runs Docker or Nixpacks builds on the server via SSH.
type Builder struct {
	exec   ssh.Executor
	stdout io.Writer
}

// NewBuilder creates a Builder backed by the given SSH executor.
func NewBuilder(exec ssh.Executor, stdout io.Writer) *Builder {
	return &Builder{exec: exec, stdout: stdout}
}

// Build runs the appropriate build command on the server and returns the image tag.
func (b *Builder) Build(ctx context.Context, cfg BuildConfig) (string, error) {
	tag := ImageTag(cfg.App, cfg.Version)

	switch cfg.Mode {
	case ModeDockerfile:
		return tag, b.buildDockerfile(ctx, tag, cfg.BuildDir, cfg.Context, cfg.Dockerfile, cfg.Platform)
	case ModeNixpacks:
		return tag, b.buildNixpacks(ctx, tag, cfg.App, cfg.BuildDir, cfg.Context)
	default:
		return "", fmt.Errorf("unknown build mode: %s", cfg.Mode)
	}
}

func (b *Builder) buildDockerfile(ctx context.Context, tag, buildDir, contextSub, dockerfile, platform string) error {
	cmd := "docker build -t " + tag
	if platform != "" {
		cmd += " --platform " + platform
	}
	// Remote paths are POSIX; use path.Join and quote for the shell.
	cmd += remoteBuildTail(buildDir, contextSub, dockerfile)
	return b.exec.RunStream(ctx, cmd, b.stdout, b.stdout)
}

func (b *Builder) buildNixpacks(ctx context.Context, tag, app, buildDir, contextSub string) error {
	// Ensure Nixpacks is installed (lazy installation).
	if err := b.ensureNixpacks(ctx); err != nil {
		return err
	}

	cachePath := fmt.Sprintf("/deployments/%s/cache", app)
	buildTarget := subDir(path.Join, buildDir, contextSub)
	cmd := fmt.Sprintf("nixpacks build %s --name %s --cache-path %s", buildTarget, tag, cachePath)
	return b.exec.RunStream(ctx, cmd, b.stdout, b.stdout)
}

// subDir joins an optional context subdir onto a root, returning the root
// unchanged for "" or ".". join is path.Join for remote (POSIX) paths and
// filepath.Join for local ones.
func subDir(join func(...string) string, root, contextSub string) string {
	if contextSub == "" || contextSub == "." {
		return root
	}
	return join(root, contextSub)
}

// remoteBuildTail builds the trailing docker-build arguments for a remote
// (shell-string) build: an optional `-f <dockerfile>` plus the context dir.
// When neither Context nor Dockerfile is customized it returns exactly
// " <root>" — byte-identical to the long-standing default command — so the
// common case is unchanged. Custom paths are quoted for the remote shell.
func remoteBuildTail(root, contextSub, dockerfile string) string {
	if contextSub == "" && dockerfile == "" {
		return " " + root
	}
	contextDir := subDir(path.Join, root, contextSub)
	tail := ""
	if dockerfile != "" {
		if dfPath := path.Join(contextDir, dockerfile); dfPath != path.Join(contextDir, "Dockerfile") {
			tail += " -f " + ssh.ShellQuote(dfPath)
		}
	}
	if contextDir == root {
		tail += " " + root
	} else {
		tail += " " + ssh.ShellQuote(contextDir)
	}
	return tail
}

func (b *Builder) ensureNixpacks(ctx context.Context) error {
	if _, err := b.exec.Run(ctx, "which nixpacks"); err == nil {
		return nil
	}

	fmt.Fprintln(b.stdout, "Installing Nixpacks...")
	return b.exec.RunStream(ctx, "curl -sSL https://nixpacks.com/install.sh | bash", b.stdout, b.stdout)
}

// LocalBuildConfig holds parameters for building locally and streaming to server.
type LocalBuildConfig struct {
	App     string
	Version string
	Mode    Mode
	Dir     string // local source directory
	// Context is the build-context subdirectory relative to Dir (empty or
	// "." means Dir itself).
	Context string
	// Dockerfile is the Dockerfile path relative to Context (empty means
	// "Dockerfile" in the context root).
	Dockerfile string
	Host       string
	User       string
	KeyPath    string
	Platform   string       // e.g. "linux/arm64" (optional, overrides auto-detection)
	Exec       ssh.Executor // optional: if set, enables layer-optimized transfer
}

// LocalBuild builds the image on the local machine, then streams it to the
// server via `docker save | ssh docker load`. Returns the image tag.
func LocalBuild(ctx context.Context, cfg LocalBuildConfig, stdout io.Writer) (string, error) {
	tag := ImageTag(cfg.App, cfg.Version)

	// Build locally.
	switch cfg.Mode {
	case ModeDockerfile:
		if err := localBuildDockerfile(ctx, tag, cfg.Dir, cfg.Context, cfg.Dockerfile, cfg.Platform, stdout); err != nil {
			return "", err
		}
	case ModeNixpacks:
		if err := localBuildNixpacks(ctx, tag, subDir(filepath.Join, cfg.Dir, cfg.Context), stdout); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown build mode: %s", cfg.Mode)
	}

	// Stream image to server. Try layer-optimized (gzip) transfer first.
	fmt.Fprintln(stdout, "Streaming image to server...")
	if cfg.Exec != nil {
		if err := LayerOptimizedTransfer(ctx, tag, cfg.App, cfg.Host, cfg.User, cfg.KeyPath, cfg.Exec, stdout); err != nil {
			fmt.Fprintf(stdout, "  Layer-optimized transfer unavailable (%v), using plain transfer\n", err)
			if err := streamImage(ctx, tag, cfg.Host, cfg.User, cfg.KeyPath, stdout); err != nil {
				return "", fmt.Errorf("streaming image: %w", err)
			}
		}
	} else {
		if err := streamImage(ctx, tag, cfg.Host, cfg.User, cfg.KeyPath, stdout); err != nil {
			return "", fmt.Errorf("streaming image: %w", err)
		}
	}
	fmt.Fprintln(stdout, "  Image loaded on server")

	return tag, nil
}

func localBuildDockerfile(ctx context.Context, tag, dir, contextSub, dockerfile, platform string, stdout io.Writer) error {
	args := []string{"build", "-t", tag}

	if platform != "" {
		// Explicit platform from config.
		args = append(args, "--platform", platform)
	} else if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		// Cross-compile for linux/amd64 when building on macOS ARM.
		args = append(args, "--platform", "linux/amd64")
	}

	// exec.Command takes an argv, so no shell quoting is needed here.
	args = localBuildTail(args, dir, contextSub, dockerfile)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("local docker build failed: %w", err)
	}
	return nil
}

// localBuildTail appends the trailing docker-build arguments for a local
// (argv) build: an optional `-f <dockerfile>` plus the context dir. Mirrors
// remoteBuildTail but produces argv entries (no shell quoting) using host
// path semantics.
func localBuildTail(args []string, dir, contextSub, dockerfile string) []string {
	if contextSub == "" && dockerfile == "" {
		return append(args, dir)
	}
	contextDir := subDir(filepath.Join, dir, contextSub)
	if dockerfile != "" {
		if dfPath := filepath.Join(contextDir, dockerfile); dfPath != filepath.Join(contextDir, "Dockerfile") {
			args = append(args, "-f", dfPath)
		}
	}
	return append(args, contextDir)
}

func localBuildNixpacks(ctx context.Context, tag, dir string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "nixpacks", "build", dir, "--name", tag)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("local nixpacks build failed: %w", err)
	}
	return nil
}

func streamImage(ctx context.Context, tag, host, user, keyPath string, stdout io.Writer) error {
	sshArgs := []string{"-o", "StrictHostKeyChecking=no"}
	if keyPath != "" {
		sshArgs = append(sshArgs, "-i", keyPath)
	}
	sshTarget := fmt.Sprintf("%s@%s", user, host)
	sshArgs = append(sshArgs, sshTarget, "docker", "load")

	// docker save <tag> | ssh <host> docker load
	save := exec.CommandContext(ctx, "docker", "save", tag)
	load := exec.CommandContext(ctx, "ssh", sshArgs...)

	pipe, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating pipe: %w", err)
	}
	load.Stdin = pipe
	load.Stdout = stdout
	load.Stderr = stdout

	if err := save.Start(); err != nil {
		return fmt.Errorf("starting docker save: %w", err)
	}
	if err := load.Start(); err != nil {
		save.Process.Kill()
		return fmt.Errorf("starting ssh load: %w", err)
	}

	saveErr := save.Wait()
	loadErr := load.Wait()
	if saveErr != nil {
		return fmt.Errorf("docker save: %w", saveErr)
	}
	if loadErr != nil {
		return fmt.Errorf("ssh docker load: %w", loadErr)
	}
	return nil
}

// PruneImages removes build images older than 72 hours for the given app.
func (b *Builder) PruneImages(ctx context.Context, app string) error {
	// Remove images matching the app build tag that are older than 72h.
	cmd := fmt.Sprintf(
		"docker image ls --filter reference='%s-build-*' --format '{{.ID}} {{.CreatedAt}}' | "+
			"awk -v cutoff=\"$(date -d '72 hours ago' +%%s 2>/dev/null || date -v-72H +%%s)\" "+
			"'{ cmd=\"date -d \\\"\"$2\" \"$3\"\\\" +%%s 2>/dev/null || date -j -f \\\"%%Y-%%m-%%d %%H:%%M:%%S\\\" \\\"\"$2\" \"$3\"\\\" +%%s\"; "+
			"cmd | getline ts; close(cmd); if (ts < cutoff) print $1 }' | "+
			"xargs -r docker rmi 2>/dev/null || true",
		app,
	)
	_, err := b.exec.Run(ctx, cmd)
	return err
}
