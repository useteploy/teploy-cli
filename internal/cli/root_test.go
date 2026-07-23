package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/build"
	"github.com/useteploy/teploy/internal/config"
	teployenv "github.com/useteploy/teploy/internal/env"
)

func TestProjectDirEstablishesRelativeProjectSemantics(t *testing.T) {
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(originalDir); err != nil {
			t.Errorf("restoring working directory: %v", err)
		}
	}()

	invocationDir := t.TempDir()
	projectDir := filepath.Join(invocationDir, "project")
	for _, dir := range []string{"build", "env", "certs"} {
		if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectFile(t, filepath.Join(projectDir, "teploy.yml"), `app: dash-contract
domain: dash-contract.example.com
server: prod
context: build
dockerfile: Dockerfile
env_files:
  - env/app.env
tls:
  cert: certs/origin.pem
  key: certs/origin.key
`)
	writeProjectFile(t, filepath.Join(projectDir, "build", "Dockerfile"), "FROM scratch\n")
	writeProjectFile(t, filepath.Join(projectDir, "env", "app.env"), "PROJECT_DIR_WORKS=yes\n")
	writeProjectFile(t, filepath.Join(projectDir, "certs", "origin.pem"), "certificate")
	writeProjectFile(t, filepath.Join(projectDir, "certs", "origin.key"), "private key")

	if err := os.Chdir(invocationDir); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd("test")
	root.AddCommand(&cobra.Command{
		Use:  "project-dir-probe",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			cwdInfo, err := os.Stat(cwd)
			if err != nil {
				return err
			}
			projectInfo, err := os.Stat(projectDir)
			if err != nil {
				return err
			}
			if !os.SameFile(cwdInfo, projectInfo) {
				return fmt.Errorf("cwd = %s, want %s", cwd, projectDir)
			}

			cfg, err := config.LoadApp(".")
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if mode, err := build.DetectAt(cfg.Context, cfg.Dockerfile); err != nil || mode != build.ModeDockerfile {
				return fmt.Errorf("resolving build paths: mode=%s err=%v", mode, err)
			}
			vars, err := teployenv.LoadLocalEnvFiles(".", cfg.EnvFiles)
			if err != nil {
				return fmt.Errorf("loading env files: %w", err)
			}
			if vars["PROJECT_DIR_WORKS"] != "yes" {
				return fmt.Errorf("relative env file was not loaded: %#v", vars)
			}
			if cfg.TLS == nil {
				return fmt.Errorf("tls config was not loaded")
			}
			if _, err := os.ReadFile(cfg.TLS.Cert); err != nil {
				return fmt.Errorf("reading relative TLS cert: %w", err)
			}
			if _, err := os.ReadFile(cfg.TLS.Key); err != nil {
				return fmt.Errorf("reading relative TLS key: %w", err)
			}
			return nil
		},
	})
	root.SetArgs([]string{"project-dir-probe", "--project-dir", "project"})
	if err := root.Execute(); err != nil {
		t.Fatalf("executing with --project-dir: %v", err)
	}
}

func writeProjectFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
