package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Server represents a single server entry in servers.yml.
//
// JSON tags are explicit (lowercase) so `server list --json` emits keys that
// consumers — notably the teploy-dash frontend, which is case-sensitive — can
// read. Without them Go would emit capitalized field names (Host/User/…).
type Server struct {
	Host  string            `yaml:"host" json:"host"`
	User  string            `yaml:"user,omitempty" json:"user,omitempty"`     // default: root
	Role  string            `yaml:"role,omitempty" json:"role,omitempty"`     // app, lb, or empty (single-server)
	Tags  map[string]string `yaml:"tags,omitempty" json:"tags,omitempty"`     // per-host env vars injected during deploy
	VpnIP string            `yaml:"vpn_ip,omitempty" json:"vpn_ip,omitempty"` // VPN mesh IP (tailscale, headscale, netbird)
}

// ServersConfig is the top-level structure of ~/.teploy/servers.yml.
type ServersConfig struct {
	Servers map[string]Server `yaml:"servers"`
}

// DefaultServersPath returns ~/.teploy/servers.yml.
func DefaultServersPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".teploy", "servers.yml"), nil
}

// LoadServers reads and parses the servers config file.
func LoadServers(path string) (*ServersConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading servers config: %w", err)
	}

	var cfg ServersConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing servers config: %w", err)
	}
	return &cfg, nil
}

// ResolveServer looks up a server by name or treats the input as a raw host.
// Priority: flags → env vars → servers.yml.
func ResolveServer(name string, flagHost, flagUser, flagKey string) (host, user, keyPath string, err error) {
	// 1. Flags override everything
	if flagHost != "" {
		host = flagHost
		user = flagUser
		if user == "" {
			user = os.Getenv("TEPLOY_USER")
		}
		if user == "" {
			user = "root"
		}
		keyPath = flagKey
		if keyPath == "" {
			keyPath = os.Getenv("TEPLOY_SSH_KEY")
		}
		return host, user, keyPath, nil
	}

	// 2. Env vars
	if envHost := os.Getenv("TEPLOY_HOST"); envHost != "" {
		host = envHost
		user = os.Getenv("TEPLOY_USER")
		if user == "" {
			user = "root"
		}
		keyPath = os.Getenv("TEPLOY_SSH_KEY")
		return host, user, keyPath, nil
	}

	// 3. servers.yml lookup
	//
	// TEPLOY_SSH_KEY applies here too, same as cases 1/2 — found live
	// while testing `teploy scale` in an isolated environment: this was
	// the only one of the three ResolveServer branches that silently
	// dropped the env var, so a named-server lookup (by far the most
	// common way teploy resolves a host) could never honor it, even
	// though the connect-failure error message advertises it
	// unconditionally ("provide --key, set TEPLOY_SSH_KEY, or place a
	// key at ~/.ssh/id_ed25519").
	envKey := os.Getenv("TEPLOY_SSH_KEY")

	serversPath, err := DefaultServersPath()
	if err != nil {
		return "", "", "", err
	}

	cfg, err := LoadServers(serversPath)
	if err != nil {
		// If no servers.yml, treat the name as a raw IP/hostname
		if errors.Is(err, os.ErrNotExist) {
			return name, "root", envKey, nil
		}
		return "", "", "", err
	}

	server, ok := cfg.Servers[name]
	if !ok {
		// Not found in servers.yml — treat as raw IP/hostname
		return name, "root", envKey, nil
	}

	user = server.User
	if user == "" {
		user = "root"
	}
	return server.Host, user, envKey, nil
}

// AddServer adds or updates a server entry in the given servers.yml file.
// Creates the file and parent directory if they don't exist.
func AddServer(path, name, host, user, role, vpnIP string) error {
	cfg := &ServersConfig{Servers: make(map[string]Server)}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("parsing servers config: %w", err)
		}
		if cfg.Servers == nil {
			cfg.Servers = make(map[string]Server)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading servers config: %w", err)
	}

	// Preserve fields that aren't being changed. AddServer is an upsert, but it
	// used to replace the whole entry — so re-adding a server (e.g. to change
	// its host) silently dropped Tags entirely (there's no tags param; tags are
	// hand-edited in servers.yml) and cleared VpnIP/Role/User. Tags drive
	// per-host env injection at deploy time, so losing them broke deploys. Keep
	// existing values; only overwrite an optional field when a new value is given.
	existing := cfg.Servers[name]
	merged := Server{
		Host:  host,
		User:  user,
		Role:  role,
		VpnIP: vpnIP,
		Tags:  existing.Tags, // settable only via servers.yml — never drop on re-add
	}
	if merged.Host == "" {
		merged.Host = existing.Host
	}
	if merged.User == "" {
		merged.User = existing.User
	}
	if merged.Role == "" {
		merged.Role = existing.Role
	}
	if merged.VpnIP == "" {
		merged.VpnIP = existing.VpnIP
	}
	cfg.Servers[name] = merged

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling servers config: %w", err)
	}

	return os.WriteFile(path, out, 0644)
}

// RemoveServer removes a server entry from the given servers.yml file.
func RemoveServer(path, name string) error {
	cfg := &ServersConfig{Servers: make(map[string]Server)}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading servers config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parsing servers config: %w", err)
	}

	if _, ok := cfg.Servers[name]; !ok {
		return fmt.Errorf("server %q not found", name)
	}

	delete(cfg.Servers, name)

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling servers config: %w", err)
	}

	return os.WriteFile(path, out, 0644)
}

// ListServers returns all configured servers from the given file.
func ListServers(path string) (map[string]Server, error) {
	cfg, err := LoadServers(path)
	if err != nil {
		return nil, err
	}
	return cfg.Servers, nil
}

// GetServersByRole returns servers with the specified role from the given file.
// If role is "app", servers with an empty role are also included (default role is "app").
func GetServersByRole(path, role string) (map[string]Server, error) {
	all, err := ListServers(path)
	if err != nil {
		return nil, err
	}

	result := make(map[string]Server)
	for name, srv := range all {
		srvRole := srv.Role
		if srvRole == "" {
			srvRole = "app" // default role
		}
		if srvRole == role {
			result[name] = srv
		}
	}
	return result, nil
}
