package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// resolveApp returns the app config plus an open SSH connection for a command
// that operates on a single app. It unifies the two ways a command can be told
// which app + server to act on:
//
//   - Without --app (appName == ""): load teploy.yml from the cwd and connect
//     to the app's configured server. This is the normal interactive path.
//   - With --app (appName != ""): require --host and read the deployed state
//     from the server — no teploy.yml needed. This is the path teploy-dash and
//     other automation use, where there is no app directory on disk.
//
// In the --app path the returned AppConfig is minimal: App + Server are always
// set, and Domain is enriched from server-side state when available (commands
// like maintenance need it). Fields that only live in teploy.yml (e.g. custom
// Ingress) are not recoverable from server state, so --app assumes the default
// Caddy-managed ingress; commands that depend on non-default ingress should be
// run from an app directory.
//
// The caller owns the returned executor and must Close it.
func resolveApp(ctx context.Context, flags *Flags, appName string) (*config.AppConfig, ssh.Executor, error) {
	if appName == "" {
		appCfg, err := config.LoadApp(".")
		if err != nil {
			return nil, nil, err
		}
		ex, err := connectForApp(ctx, flags, appCfg)
		if err != nil {
			return nil, nil, err
		}
		return appCfg, ex, nil
	}

	if flags.Host == "" {
		return nil, nil, fmt.Errorf("--host is required when using --app")
	}
	// This path builds an AppConfig directly instead of going through
	// config.LoadApp, so it never reaches AppConfig.validate() — appName
	// (raw --app flag input) must be validated here before it reaches
	// state.Read or any shell-interpolated command downstream (every
	// command that supports --app, e.g. exec/logs/status/maintenance,
	// goes through this helper).
	if err := config.ValidateName(appName); err != nil {
		return nil, nil, err
	}
	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return nil, nil, err
	}
	// Skipped in --json mode — see connectForApp's matching comment below;
	// same issue, same fix.
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	ex, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return nil, nil, err
	}
	appCfg := &config.AppConfig{App: appName, Server: flags.Host}
	if st, err := state.Read(ctx, ex, appName); err == nil && st != nil {
		appCfg.Domain = st.Domain
	}
	return appCfg, ex, nil
}

// connectForApp resolves the server from app config and establishes an SSH connection.
// Uses the first server from appCfg.Servers if Server is empty.
func connectForApp(ctx context.Context, flags *Flags, appCfg *config.AppConfig) (ssh.Executor, error) {
	serverName := appCfg.Server
	if serverName == "" && len(appCfg.Servers) > 0 {
		serverName = appCfg.Servers[0]
	}
	if serverName == "" {
		return nil, fmt.Errorf("no server specified — set 'server' in teploy.yml")
	}

	host, user, key, err := config.ResolveServer(serverName, flags.Host, flags.User, flags.Key)
	if err != nil {
		return nil, err
	}

	// Skipped in --json mode: this printed unconditionally to stdout,
	// ahead of the actual JSON payload commands like `status --json`
	// emit — a consumer treating stdout as one parseable blob (rather
	// than reading Stdout/Stderr separately) would get "Connecting
	// to...\n{...}" instead of valid JSON. Found checking whether
	// teploy-dash's exact `status --host X --json` call parses cleanly.
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	return ssh.Connect(ctx, ssh.ConnectConfig{
		Host:    host,
		User:    user,
		KeyPath: key,
	})
}

// sortedAccessoryNames returns accessory names sorted for deterministic ordering.
func sortedAccessoryNames(accessories map[string]config.AccessoryConfig) []string {
	names := make([]string, 0, len(accessories))
	for name := range accessories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
