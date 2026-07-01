package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// buildContainerEnvFiles computes the full container environment — the
// teploy.yml `env:` block (${VAR}-expanded from the local environment),
// overlaid with extra per-deploy values (e.g. singledeploy's per-host
// servers.yml tags), overlaid with decrypted `teploy secret` values so a
// secret always wins over a plaintext default — and uploads it to a fresh
// per-deploy env file instead of returning it for use as `docker run -e`
// arguments.
//
// This exists because `-e KEY=value` arguments are visible in this host's
// `ps aux` / `/proc/<pid>/cmdline` output for the life of the `docker run`
// invocation — fine for plaintext config, not for decrypted secrets. An
// `--env-file` is read once at container creation and never appears in
// argv. (Note this does not hide the resolved values from `docker inspect`
// on the running container — Docker bakes the final env into the
// container's own config either way, `-e` or `--env-file`; that's how the
// app's process gets the value into its environment at all. This closes
// the ps-aux/proc-cmdline exposure specifically, not all exposure.)
//
// Returns the ordered --env-file path list for deploy.Config.EnvFiles: the
// existing persisted /deployments/<app>/.env first (if present, managed by
// `teploy env set`), then this fresh file last so its values — including
// secrets — take precedence for any overlapping key.
func buildContainerEnvFiles(ctx context.Context, executor ssh.Executor, app, persistedEnvFile string, appEnv, extra, secrets map[string]string) ([]string, error) {
	merged := make(map[string]string, len(appEnv)+len(extra)+len(secrets))
	for k, v := range appEnv {
		merged[k] = os.Expand(v, os.Getenv)
	}
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range secrets {
		merged[k] = v
	}

	var envFiles []string
	if persistedEnvFile != "" {
		envFiles = append(envFiles, persistedEnvFile)
	}
	if len(merged) == 0 {
		return envFiles, nil
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, merged[k])
	}

	path := fmt.Sprintf("/deployments/%s/.deploy-env", app)
	if err := executor.Upload(ctx, strings.NewReader(sb.String()), path, "0600"); err != nil {
		return nil, fmt.Errorf("uploading deploy env file: %w", err)
	}
	return append(envFiles, path), nil
}
