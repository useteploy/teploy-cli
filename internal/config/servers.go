package config

import (
	"crypto/rand"
	"encoding/hex"
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
	// ID is the server's stable identity (X02 §1.3): minted once when the
	// entry is created, preserved by rename/update and by AddServer's
	// upsert, and never re-derived. Empty on legacy entries written before
	// the field existed — consumers (teploy-dash) fall back to their
	// name-derived hash for those; minting on re-add would silently re-key
	// every id-based reference, which is the exact hazard the field removes.
	ID    string            `yaml:"id,omitempty" json:"id,omitempty"`
	Host  string            `yaml:"host" json:"host"`
	User  string            `yaml:"user,omitempty" json:"user,omitempty"`     // default: root
	Role  string            `yaml:"role,omitempty" json:"role,omitempty"`     // app, lb, or empty (single-server)
	Tags  map[string]string `yaml:"tags,omitempty" json:"tags,omitempty"`     // per-host env vars injected during deploy
	VpnIP string            `yaml:"vpn_ip,omitempty" json:"vpn_ip,omitempty"` // VPN mesh IP (tailscale, headscale, netbird)
}

// newServerID mints a random stable server identity: "srv-" + 16 hex chars.
// Random (not name-derived) because the ID names a registration, not content
// that must be re-derivable — a rename must not change it, which no hash of
// the name can guarantee (X02 §1.2).
func newServerID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating server id: %w", err)
	}
	return "srv-" + hex.EncodeToString(b[:]), nil
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

// EffectiveUser resolves the SSH user to connect as, layering teploy.yml's
// `user:` on top of ResolveServer's result. ResolveServer defaults a
// literal-IP/hostname server: (one not in servers.yml) to "root" and has no
// knowledge of the AppConfig, so callers apply the app-level user: here.
// Precedence: an explicit --user flag (already baked into `resolved`) wins;
// otherwise teploy.yml's `user:` overrides the root default; otherwise the
// resolved value stands. Used by both `teploy deploy` and `teploy validate`
// so they connect as the same account.
func EffectiveUser(resolved, flagUser, appUser string) string {
	if flagUser == "" && appUser != "" {
		return appUser
	}
	return resolved
}

// Sentinel errors for the servers.yml mutation paths. Typed so callers —
// notably teploy-dash, which maps them onto HTTP status codes (404 vs 409)
// — can distinguish failure modes without string matching (audit
// UPSTREAM-2, from teploy-dash A36).
var (
	// ErrServerNotFound is returned when a mutation names a server that is
	// not in servers.yml (or servers.yml itself is absent).
	ErrServerNotFound = errors.New("server not found")
	// ErrServerExists is returned when a rename destination is already
	// taken; the existing entry is never overwritten.
	ErrServerExists = errors.New("server already exists")
)

// saveServers commits the whole servers map in one atomic step: the new
// content is written to a sibling temp file, synced, and renamed over path.
// The previous os.WriteFile truncated-then-wrote in place, so a crash or
// full disk mid-write could leave a torn servers.yml — and every rename was
// two such writes (remove, then add), widening the window (audit
// UPSTREAM-2). The rename swap means readers see either the complete old
// file or the complete new one, never a partial write.
func saveServers(path string, cfg *ServersConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling servers config: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".servers-*")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("setting temp config permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing servers config: %w", err)
	}
	return nil
}

// loadServersForMutate reads servers.yml for a mutation that requires the
// file and a target entry to already exist.
func loadServersForMutate(path string) (*ServersConfig, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no servers config at %s", ErrServerNotFound, path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading servers config: %w", err)
	}
	var cfg ServersConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing servers config: %w", err)
	}
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]Server)
	}
	return &cfg, nil
}

// RenameServer renames a server entry in one atomic commit, preserving
// every field of the original record — host, user, role, tags, vpn_ip. This
// is the operation teploy-dash used to synthesize as remove+add across two
// CLI invocations, which lost tags/vpn_ip and could be interrupted between
// the two writes (audit UPSTREAM-2, from teploy-dash A36). Destination
// collisions are rejected, never overwritten; renaming to the same name is
// a verified no-op.
func RenameServer(path, oldName, newName string) error {
	cfg, err := loadServersForMutate(path)
	if err != nil {
		return err
	}
	entry, ok := cfg.Servers[oldName]
	if !ok {
		return fmt.Errorf("%w: %q", ErrServerNotFound, oldName)
	}
	if oldName == newName {
		return nil
	}
	if _, exists := cfg.Servers[newName]; exists {
		return fmt.Errorf("%w: %q", ErrServerExists, newName)
	}
	cfg.Servers[newName] = entry
	delete(cfg.Servers, oldName)
	return saveServers(path, cfg)
}

// ServerUpdates carries the optional fields of `server update`. A nil field
// leaves the stored value unchanged; a non-nil field — including the empty
// string — sets it, so `--vpn-ip ""` clears the mesh address. Tags are
// deliberately not settable: they are per-host deploy env managed in
// servers.yml itself, and update never touches them.
type ServerUpdates struct {
	Host  *string
	User  *string
	Role  *string
	VpnIP *string
}

// UpdateServer changes only the supplied fields of a server entry and
// commits the whole file atomically. Unspecified fields — and every other
// entry — are preserved as-is (audit UPSTREAM-2: dash's remove+add edit
// path dropped tags/vpn_ip and was non-atomic).
func UpdateServer(path, name string, upd ServerUpdates) error {
	cfg, err := loadServersForMutate(path)
	if err != nil {
		return err
	}
	entry, ok := cfg.Servers[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrServerNotFound, name)
	}
	if upd.Host != nil {
		entry.Host = *upd.Host
	}
	if upd.User != nil {
		entry.User = *upd.User
	}
	if upd.Role != nil {
		entry.Role = *upd.Role
	}
	if upd.VpnIP != nil {
		entry.VpnIP = *upd.VpnIP
	}
	cfg.Servers[name] = entry
	return saveServers(path, cfg)
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
	existing, existed := cfg.Servers[name]
	merged := Server{
		Host:  host,
		User:  user,
		Role:  role,
		VpnIP: vpnIP,
		Tags:  existing.Tags, // settable only via servers.yml — never drop on re-add
		ID:    existing.ID,   // preserved on re-add; minted below only for new entries
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
	if !existed {
		id, err := newServerID()
		if err != nil {
			return err
		}
		merged.ID = id
	}
	cfg.Servers[name] = merged

	return saveServers(path, cfg)
}

// RemoveServer removes a server entry from the given servers.yml file.
func RemoveServer(path, name string) error {
	cfg, err := loadServersForMutate(path)
	if err != nil {
		return err
	}

	if _, ok := cfg.Servers[name]; !ok {
		return fmt.Errorf("server %q not found", name)
	}

	delete(cfg.Servers, name)

	return saveServers(path, cfg)
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
