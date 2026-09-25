package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

// validEnvKey is the grammar docker's --env-file accepts for a KEY: a
// nonempty identifier. Validating at the serialization boundary catches
// malformed records from any input path (audit F59).
var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// expandEnvTemplates applies ${VAR} interpolation to the app's EXPLICIT
// YAML env: values, exactly once, from the operator's local environment.
// This is the ONLY place expansion happens: decrypted env_files, server
// secrets, and per-host tags are literal values and must never be handed to
// os.Expand again — a decrypted password containing a literal $ used to be
// substituted or emptied according to whatever happened to be in the
// operator's environment at deploy time (audit F59).
//
// strict (the opt-in --strict-env mode, audit F57/TCL-32) fails the deploy
// listing every ${VAR} that is unset, instead of silently expanding it to
// the empty string — a typo'd variable name currently deploys fine and
// breaks at runtime. Default (non-strict) behavior is unchanged for
// compatibility.
func expandEnvTemplates(env map[string]string, strict bool) error {
	for k, v := range env {
		var missing []string
		expanded := os.Expand(v, func(name string) string {
			if val, ok := os.LookupEnv(name); ok {
				return val
			}
			missing = append(missing, name)
			return ""
		})
		if strict && len(missing) > 0 {
			seen := map[string]bool{}
			var uniq []string
			for _, m := range missing {
				if !seen[m] {
					seen[m] = true
					uniq = append(uniq, m)
				}
			}
			return fmt.Errorf("strict-env: env.%s references unset variable(s): %s — set %s or deploy without --strict-env",
				k, strings.Join(uniq, ", "), strings.Join(uniq, ", "))
		}
		env[k] = expanded
	}
	return nil
}

// buildContainerEnvFiles computes the full container environment — values
// already resolved (YAML templates expanded once by expandEnvTemplates,
// env_files loaded literally, decrypted secrets overlaid last so a secret
// always wins over a plaintext default) — and uploads it to the ATTEMPT's
// env file (F08: /deployments/<app>/meta/att/<hash>.<id>/env, written once
// by this attempt and never rewritten by a later one) instead of returning
// it for use as `docker run -e` arguments.
//
// This exists because `-e KEY=value` arguments are visible in this host's
// `ps aux` / /proc/<pid>/cmdline output for the life of the `docker run`
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
// `teploy env set`), then this attempt's file last so its values —
// including secrets — take precedence for any overlapping key.
func buildContainerEnvFiles(ctx context.Context, executor ssh.Executor, app string, att *releasemeta.Attempt, persistedEnvFile string, appEnv, extra, secrets map[string]string) ([]string, error) {
	merged := make(map[string]string, len(appEnv)+len(extra)+len(secrets))
	for k, v := range appEnv {
		merged[k] = v
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

	for _, k := range keys {
		if !validEnvKey.MatchString(k) {
			return nil, fmt.Errorf("invalid environment key %q — keys must be identifiers (letters, digits, underscore; not starting with a digit)", k)
		}
		// docker's --env-file format is strictly one KEY=value per line, so a
		// value containing a newline does not round-trip: docker reads the
		// continuation lines as further variables and fails with a confusing
		// complaint about a variable name "containing whitespaces". Catch it
		// here and name the culprit instead.
		//
		// Routing just these values through `-e` is deliberately not the fallback:
		// that is the ps-aux/proc-cmdline exposure this whole file exists to avoid,
		// and a multi-line value is as likely to be a private key as a config blob.
		if strings.ContainsAny(merged[k], "\n\r\x00") {
			return nil, fmt.Errorf("env value for %s spans multiple lines, which docker's --env-file cannot represent; "+
				"use a single-line form (YAML flow style, e.g. \"{a: 1, b: 2}\") or mount the content as a file", k)
		}
	}

	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s=%s\n", k, merged[k])
	}

	if att == nil {
		return nil, fmt.Errorf("buildContainerEnvFiles requires a deploy attempt (F08) — the env file is attempt-scoped")
	}
	path := att.EnvFile()
	if _, err := executor.Run(ctx, att.MkdirCmd()); err != nil {
		return nil, fmt.Errorf("creating attempt directory: %w", err)
	}
	if err := executor.Upload(ctx, strings.NewReader(sb.String()), path, "0600"); err != nil {
		return nil, fmt.Errorf("uploading deploy env file: %w", err)
	}
	return append(envFiles, path), nil
}
