// Package releasemeta stores the immutable per-release execution record
// (audit F14): the full resolved spec of every release this CLI deployed to
// a target, keyed by (app, release id).
//
// Design:
//
//   - One JSON file per release at /deployments/<app>/meta/<hash>.json,
//     written atomically (sibling temp + rename) with mode 0600. Backfilled
//     records carry the container's RESOLVED environment, which docker
//     inspect reveals on the same host anyway, but which has no business
//     being world-readable on disk.
//   - The store is per-target (per-server, per-app directory), exactly like
//     state.json and pins: the server a command talks to IS the target;
//     there is no client-side database to key otherwise.
//   - Writers: successful deploys (container and static). Readers: rollback,
//     state-only rollback, recreate. Rollback never writes a record for its
//     target — the record describes the release, and a release does not
//     change by being rolled back to.
//   - Immutability: readers never mutate. The only writer allowed to change
//     an existing record is a NEW deploy of the same release id (a
//     same-version redeploy with changed config: the live containers were
//     just recreated with that config, so the record follows or it lies).
//   - Migration: releases deployed before this store existed have no record.
//     The first consumer that needs one backfills it from the live
//     containers (a "release-0" record, flagged Backfilled) so existing
//     installs converge without a redeploy. Backfill is best-effort: fields
//     docker cannot report (--env-file references, the teploy health/caddy
//     config) stay empty and callers keep their legacy fallbacks for them.
package releasemeta

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// SchemaVersion is the record schema this CLI writes and accepts.
const SchemaVersion = 1

// deploymentsDir mirrors state.DefaultStateDir without importing a cycle
// through the deploy package.
const deploymentsDir = "/deployments"

// validHash matches release ids safe to use as a meta file-name segment:
// the same grammar the CLI already accepts for versions (imageTagPattern in
// internal/cli) — git short hashes, tags like v1.2.3, and image-derived
// sha256-<hex> labels all pass; path metacharacters do not.
var validHash = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// Port is one published port of a release, with the primary designation that
// disambiguates multi-port containers (audit TCL-14): health checks probe
// the primary's host binding and Caddy dials the primary's container port.
type Port struct {
	HostPort      int    `json:"host_port,omitempty"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto,omitempty"` // "" and "tcp" are the same
	Bind          string `json:"bind,omitempty"`  // host IP the port is published on
	Primary       bool   `json:"primary,omitempty"`
	// Fixed marks an operator-configured host port (publish entries, host
	// ingress): a port two containers can never hold at once, which is what
	// makes the deploy/rollback recreate strategy apply (audit F21).
	Fixed bool `json:"fixed,omitempty"`
}

// Health records the deploy-time health gate so a rollback probes the way
// the target release was actually deployed with, not whatever the current
// teploy.yml says.
type Health struct {
	Mode            string `json:"mode,omitempty"`
	Path            string `json:"path,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
	IntervalSeconds int    `json:"interval_seconds,omitempty"`
}

// CaddyRoute records the Caddy-facing options a release was deployed with,
// so rollback restores the target release's edge config instead of the
// current config file's.
type CaddyRoute struct {
	TLSCert     string            `json:"tls_cert,omitempty"`
	TLSKey      string            `json:"tls_key,omitempty"`
	TLSInternal bool              `json:"tls_internal,omitempty"`
	CaddyExtra  string            `json:"caddy_extra,omitempty"`
	Cache       map[string]string `json:"cache,omitempty"`
	Firewall    *caddy.Firewall   `json:"firewall,omitempty"`
	Access      *caddy.Access     `json:"access,omitempty"`
}

// Static records a type:static release's serving configuration — the piece
// state.json never retained, which is what made state-only static rollback
// impossible before F13/F14.
type Static struct {
	Domain       string            `json:"domain,omitempty"`
	SPA          bool              `json:"spa,omitempty"`
	SPAFallback  string            `json:"spa_fallback,omitempty"`
	Cache        map[string]string `json:"cache,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	CaddyExtra   string            `json:"caddy_extra,omitempty"`
	KeepReleases int               `json:"keep_releases,omitempty"`
}

// Record is the immutable per-release execution record.
type Record struct {
	SchemaVersion int       `json:"schema_version"`
	App           string    `json:"app"`
	Hash          string    `json:"hash"`
	CreatedAt     time.Time `json:"created_at"`
	// Backfilled marks a "release-0" record synthesized from live containers
	// rather than observed at deploy time (see the package doc).
	Backfilled bool   `json:"backfilled,omitempty"`
	Generation uint64 `json:"generation,omitempty"`

	DeploymentType string `json:"deployment_type"` // container | static
	IngressMode    string `json:"ingress_mode"`
	Domain         string `json:"domain,omitempty"`

	ImageRef    string `json:"image_ref,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	// ManifestSHA256 is the effective-config digest (config.
	// NormalizeAndDigest) the release deployed under — the plan/receipt
	// equality surface (C04).
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	// Provenance is the plan-time provenance the deploy resolved to
	// (C04): revision, worktree cleanliness, build context fingerprint,
	// Dockerfile identity, platform, image digest and mutability.
	Provenance *Provenance `json:"provenance,omitempty"`

	Replicas    int               `json:"replicas,omitempty"`
	Processes   map[string]string `json:"processes,omitempty"`
	Cmd         string            `json:"cmd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	EnvFiles    []string          `json:"env_files,omitempty"`
	Volumes     map[string]string `json:"volumes,omitempty"`
	Publish     []string          `json:"publish,omitempty"`
	Ports       []Port            `json:"ports,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Memory      string            `json:"memory,omitempty"`
	CPU         string            `json:"cpu,omitempty"`
	StopTimeout int               `json:"stop_timeout,omitempty"`
	Bind        string            `json:"bind,omitempty"`

	Health *Health     `json:"health,omitempty"`
	Caddy  *CaddyRoute `json:"caddy,omitempty"`
	Static *Static     `json:"static,omitempty"`

	// Recreate is the primary web container's full inspect-derived recreation
	// spec (audit F20), captured from docker's own view of the container the
	// deploy started.
	Recreate *docker.RecreateSpec `json:"recreate,omitempty"`
}

// ValidateHash checks a release id against the grammar safe for meta
// file-name segments (exported for boundary checks like `teploy pin`, whose
// values later key prune-protection sets and meta paths).
func ValidateHash(hash string) error {
	if !validHash.MatchString(hash) {
		return fmt.Errorf("invalid release id %q", hash)
	}
	return nil
}

// Path returns the record path for (app, hash). Both segments are grammar
// checked here so no caller can interpolate an unvalidated id into a remote
// path — the app against the config name grammar (audit A17: it used to be
// checked only for non-emptiness, so a path-metacharacter app reached the
// remote shell), the hash against validHash.
func Path(app, hash string) (string, error) {
	if err := config.ValidateName(app); err != nil {
		return "", fmt.Errorf("release record requires a valid app: %w", err)
	}
	if !validHash.MatchString(hash) {
		return "", fmt.Errorf("invalid release id %q for app %q", hash, app)
	}
	return fmt.Sprintf("%s/%s/meta/%s.json", deploymentsDir, app, hash), nil
}

// Read loads the record for (app, hash). A confirmed-missing record returns
// (nil, nil); every other failure (transport, permission, malformed JSON,
// wrong schema) is an error — callers that can proceed without the record
// degrade explicitly with a warning, callers that cannot fail closed.
func Read(ctx context.Context, exec ssh.Executor, app, hash string) (*Record, error) {
	path, err := Path(app, hash)
	if err != nil {
		return nil, err
	}
	data, present, err := state.ReadRemoteFile(ctx, exec, path)
	if err != nil {
		return nil, fmt.Errorf("reading release metadata for %s@%s: %w", app, hash, err)
	}
	if !present {
		return nil, nil
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing release metadata for %s@%s: %w", app, hash, err)
	}
	if rec.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported release-metadata schema version %d for %s@%s", rec.SchemaVersion, app, hash)
	}
	// Identity check (audit T56): the record loaded from (app, hash)'s path
	// must actually DESCRIBE (app, hash). An accidentally copied, partially
	// migrated, or corrupted-but-valid record used to be accepted on schema
	// alone and could drive rollback/recreate effects at a different
	// release's spec. Every record this package writes carries both fields
	// (Write requires them), so a mismatch is never a legacy artifact.
	if rec.App != app || rec.Hash != hash {
		return nil, fmt.Errorf("release record identity mismatch: requested %s@%s, record describes %s@%s — refusing to use it", app, hash, rec.App, rec.Hash)
	}
	return &rec, nil
}

// Write persists the record atomically. It is the deploy paths' writer: an
// existing record for the same release id is replaced (same-version
// redeploy — see the package doc); readers never call this.
func Write(ctx context.Context, exec ssh.Executor, rec *Record) error {
	if rec == nil {
		return fmt.Errorf("record is required")
	}
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = SchemaVersion
	}
	if rec.SchemaVersion != SchemaVersion {
		return fmt.Errorf("cannot write release-metadata schema version %d", rec.SchemaVersion)
	}
	if rec.App == "" || rec.Hash == "" {
		return fmt.Errorf("record requires app and hash")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	path, err := Path(rec.App, rec.Hash)
	if err != nil {
		return err
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s/%s/meta", deploymentsDir, rec.App)); err != nil {
		return fmt.Errorf("creating metadata directory: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshaling release metadata: %w", err)
	}
	data = append(data, '\n')
	// 0600: backfilled records embed resolved env; docker inspect shows the
	// same values to someone already on the host, but the file must not
	// make them readable to everyone with a shell account.
	return ssh.UploadAtomic(ctx, exec, strings.NewReader(string(data)), path, "0600")
}

// PrimaryContainerPort returns the release's designated primary container
// port (audit TCL-14) — the port health checks publish-probe and Caddy
// dials, which fields[0]-style port picks cannot identify once a container
// publishes more than one port.
func PrimaryContainerPort(rec *Record) (int, bool) {
	if rec == nil {
		return 0, false
	}
	for _, p := range rec.Ports {
		if p.Primary && p.ContainerPort > 0 {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// HasFixedHostPorts reports whether any recorded port is an
// operator-configured host port — the condition that makes two containers of
// the same app unable to run at once and therefore selects the recreate
// strategy on deploy and rollback (audit F21).
func HasFixedHostPorts(rec *Record) bool {
	if rec == nil {
		return false
	}
	if len(rec.Publish) > 0 {
		return true
	}
	for _, p := range rec.Ports {
		if p.Fixed {
			return true
		}
	}
	return false
}

// PortFromPublishSpec parses a verbatim docker -p publish spec
// ("[ip:]host:container[/proto]", "[host:]container[/proto]") into a Port.
// IPv6 bind forms with more than three colon-separated segments are not
// parsed; the caller keeps the raw string in Publish, which is what recreate
// and displace decisions actually read.
func PortFromPublishSpec(spec string) (Port, bool) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return Port{}, false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return Port{}, false
	}
	var bind, host, container string
	switch len(parts) {
	case 1:
		container = parts[0]
	case 2:
		host, container = parts[0], parts[1]
	case 3:
		bind, host, container = parts[0], parts[1], parts[2]
	}
	proto := ""
	if i := strings.Index(container, "/"); i >= 0 {
		proto, container = container[i+1:], container[:i]
	}
	if container == "" {
		return Port{}, false
	}
	p := Port{ContainerPort: atoiOrZero(container), Proto: proto, Bind: bind, Fixed: true}
	p.HostPort = atoiOrZero(host)
	return p, p.ContainerPort > 0
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Backfill synthesizes the "release-0" record for a pre-F14 release from the
// live containers (see the package doc). containers is the caller's
// inventory (docker.Client.ListContainers output) — rollback already holds
// it; re-listing would race the recreation it is about to perform. The
// returned record is also persisted; a persistence failure is returned as a
// warning error alongside the record so the caller can use it in-memory.
//
// What backfill cannot recover, by construction: --env-file references
// (inspect shows resolved values only), the teploy health gate, and the
// Caddy edge config. Those fields stay empty and readers keep their legacy
// fallbacks for them.
func Backfill(ctx context.Context, exec ssh.Executor, dk *docker.Client, containers []docker.Container, app, hash string, st *state.AppState) (*Record, error) {
	if st == nil {
		return nil, fmt.Errorf("backfilling %s@%s: no state to source ingress/domain from", app, hash)
	}
	var web []docker.Container
	processes := map[string]string{}
	for _, c := range containers {
		if c.Labels["teploy.version"] != hash {
			continue
		}
		proc := c.Labels["teploy.process"]
		if proc != "" {
			processes[proc] = ""
		}
		if proc == "web" {
			web = append(web, c)
		}
	}
	if len(web) == 0 {
		return nil, fmt.Errorf("backfilling %s@%s: no web container to inspect", app, hash)
	}
	sort.Slice(web, func(i, j int) bool { return web[i].Name < web[j].Name })

	spec, err := dk.InspectRecreate(ctx, web[0].Name)
	if err != nil {
		return nil, fmt.Errorf("backfilling %s@%s: %w", app, hash, err)
	}

	rec := &Record{
		SchemaVersion: SchemaVersion,
		App:           app,
		Hash:          hash,
		CreatedAt:     time.Now().UTC(),
		Backfilled:    true,

		DeploymentType: st.DeploymentType,
		IngressMode:    st.IngressMode,
		Domain:         st.Domain,
		Replicas:       len(web),
		Processes:      processes,
		Recreate:       spec,
	}

	if st.DeploymentType == "" {
		rec.DeploymentType = "container"
	}
	if st.IngressMode == "" {
		rec.IngressMode = "caddy"
	}

	// Image identity: prefer the immutable image ID (F19's rule); keep the
	// original tag reference alongside it.
	rec.ImageDigest = spec.ImageID
	rec.ImageRef = spec.ImageRef

	// Resolved env, straight from the container's own config. PORT is the
	// primary-port signal teploy's docker run sets; it selects the primary
	// binding below.
	env := map[string]string{}
	portEnv := 0
	for _, kv := range spec.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[k] = v
		if k == "PORT" {
			portEnv = atoiOrZero(v)
		}
	}
	if len(env) > 0 {
		rec.Env = env
	}

	// Ports from the actual bindings. Fixed = outside the ephemeral range
	// this CLI allocates from (49152-65535): an operator-chosen host port.
	// A fixed port published inside the ephemeral range is misclassified as
	// ephemeral, which degrades to today's clean docker-run error rather
	// than an outage; fresh deploys record Publish verbatim and never rely
	// on this heuristic.
	primarySet := false
	for _, b := range spec.PortBindings {
		if b.ContainerPort <= 0 {
			continue
		}
		p := Port{
			HostPort:      b.HostPort,
			ContainerPort: b.ContainerPort,
			Proto:         b.Proto,
			Bind:          b.HostIP,
		}
		p.Fixed = b.HostPort > 0 && (b.HostPort < 49152 || b.HostPort > 65535)
		if !primarySet && b.Proto != "udp" && (b.ContainerPort == portEnv || portEnv == 0) {
			p.Primary = true
			primarySet = true
		}
		rec.Ports = append(rec.Ports, p)
	}

	// Volumes: binds (host:container[:mode]) plus named-volume mounts.
	volumes := map[string]string{}
	for _, bind := range spec.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) < 2 {
			continue
		}
		volumes[parts[0]] = parts[1]
	}
	for _, m := range spec.Mounts {
		if m.Type == "volume" && m.Source != "" && m.Target != "" {
			volumes[m.Source] = m.Target
		}
	}
	if len(volumes) > 0 {
		rec.Volumes = volumes
	}

	if len(spec.Labels) > 0 {
		rec.Labels = spec.Labels
	}
	if spec.MemoryBytes > 0 {
		rec.Memory = fmt.Sprintf("%db", spec.MemoryBytes)
	}
	if spec.NanoCPUs > 0 {
		rec.CPU = strconv.FormatFloat(float64(spec.NanoCPUs)/1e9, 'f', -1, 64)
	}
	if len(spec.Cmd) > 0 {
		rec.Cmd = strings.Join(spec.Cmd, " ")
	}
	if proc, ok := processes["web"]; ok && proc == "" {
		processes["web"] = rec.Cmd
	}

	if err := Write(ctx, exec, rec); err != nil {
		return rec, fmt.Errorf("persisting the backfilled record: %w", err)
	}
	return rec, nil
}
