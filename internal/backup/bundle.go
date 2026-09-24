package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// DR bundle — C07. A bundle is a COMPLETE recoverable image of one
// application: state + release records, the applied app manifest, secret
// references (or encrypted material the operator explicitly selected), the
// routing/TLS references, and engine-aware data snapshots — each snapshot
// labeled with the consistency level it actually achieved. Existing
// `teploy backup create` volume archives are DATA-ONLY backups and stay
// clearly named as such; they are not bundles and must never be presented as
// whole-app recovery.
//
// Layout (mirrored by both stores — S3 keys and local directory trees):
//
//	<app>/dr/<bundle-id>/manifest.json         written LAST (completeness marker)
//	<app>/dr/<bundle-id>/volumes/<name>.tar.gz      one archive per app volume
//	<app>/dr/<bundle-id>/accessories/<name><ext>    engine dump or tar per accessory
//	<app>/dr/<bundle-id>/secrets/<KEY>.age          only with --include-secrets
//	<app>/dr/<bundle-id>/env/.env                   only with --include-secrets
//	<app>/dr/<bundle-id>/env/credentials/<name>     only with --include-secrets
//	<app>/dr/<bundle-id>/age-key                    only with --include-age-key

// BundleSchemaVersion is the DR bundle manifest schema this CLI writes and
// accepts. Readers refuse manifests from other versions; forward
// compatibility within a version is additive fields only (X04 discipline).
const BundleSchemaVersion = 1

// Bundle kinds. Data-only volume archives (the `teploy backup` family) are
// not bundles at all; Kind exists so a bundle manifest can never be confused
// with one.
const BundleKindDR = "dr"

// BundleManifest is the schema-versioned inventory of a DR bundle: what the
// app was, what was captured, at what consistency, and what recovery
// requires. Everything needed to REBUILD the app on a fresh host — except
// the data artifacts themselves, which the manifest names.
type BundleManifest struct {
	SchemaVersion int       `json:"schema_version"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	App           string    `json:"app"`
	Server        string    `json:"server"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by,omitempty"`

	// State is the app's /deployments/<app>/state.json VERBATIM (generation,
	// release identity, ports, previous release). ReleaseRecords are the
	// /deployments/<app>/meta/<hash>.json per-release execution records.
	// AppManifest is state's applied effective-config manifest.
	State          json.RawMessage   `json:"state,omitempty"`
	ReleaseRecords []json.RawMessage `json:"release_records,omitempty"`
	AppManifest    json.RawMessage   `json:"app_manifest,omitempty"`
	// AppRun is what an isolated restore boots for the application check:
	// the deployed image plus the recorded container command (from the
	// newest release record). Without the command many images exit
	// instantly and prove nothing.
	AppRun AppRunSpec `json:"app_run"`

	Secrets   SecretsRecord    `json:"secrets"`
	Routing   RoutingRecord    `json:"routing"`
	Snapshots []SnapshotRecord `json:"snapshots"`
	Recovery  RecoveryPlan     `json:"recovery"`
}

// SecretsRecord carries what the bundle knows about the app's secrets.
// Default mode is references-only: the KEY NAMES, so a restoring operator
// knows what must exist on the target before cutover — never the material.
// Encrypted material (age ciphertexts, the resolved app .env, accessory
// credential files) is included ONLY when the operator passed
// --include-secrets, and the manifest says so explicitly.
type SecretsRecord struct {
	Mode string   `json:"mode"` // "references" | "encrypted"
	Keys []string `json:"keys,omitempty"`
	// AgeKeyIncluded reports whether the bundle carries the originating
	// host's /deployments/.age-key. Without it, encrypted secrets material
	// can only be decrypted with the ORIGINAL host's age key — Recovery
	// states that requirement in operator language.
	AgeKeyIncluded bool `json:"age_key_included"`
	// Included lists the secret-material members inside the bundle
	// (populated only in "encrypted" mode).
	Included []string `json:"included,omitempty"`
	// Recovery is the human requirement statement for getting secrets back.
	Recovery string `json:"recovery,omitempty"`
}

// RoutingRecord captures the routing/TLS IDENTITY the app had: domain,
// ingress mode, ports, and the TLS mode with cert-file REFERENCES (the cert
// files themselves live on the operator's machine; the bundle names the
// teploy.yml references so a fresh host knows what must be re-supplied).
type RoutingRecord struct {
	Domain      string     `json:"domain,omitempty"`
	IngressMode string     `json:"ingress_mode,omitempty"` // caddy | external | host
	Bind        string     `json:"bind,omitempty"`
	Port        int        `json:"port,omitempty"`
	Publish     []string   `json:"publish,omitempty"`
	TLS         *TLSRecord `json:"tls,omitempty"`
}

type TLSRecord struct {
	Mode string `json:"mode"` // acme | internal | custom-cert
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
}

// AppRunSpec is the minimum needed to BOOT the app image for validation.
type AppRunSpec struct {
	Image string `json:"image,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
}

// deriveAppRun reads the deployed image from state and the container
// command from the NEWEST release record (records carry created_at + cmd;
// state.json does not). Missing records leave Cmd empty — the app check
// then boots the bare image and reports skip/fail honestly.
func deriveAppRun(stateBytes []byte, records []json.RawMessage) AppRunSpec {
	var s struct {
		ImageRef string `json:"image_ref"`
	}
	_ = json.Unmarshal(stateBytes, &s)
	spec := AppRunSpec{Image: s.ImageRef}

	var best struct {
		CreatedAt time.Time `json:"created_at"`
		Cmd       string    `json:"cmd"`
		ImageRef  string    `json:"image_ref"`
	}
	best.CreatedAt = time.Time{}
	for _, rec := range records {
		var r struct {
			CreatedAt time.Time `json:"created_at"`
			Cmd       string    `json:"cmd"`
			ImageRef  string    `json:"image_ref"`
		}
		if err := json.Unmarshal(rec, &r); err != nil {
			continue
		}
		if r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	if best.Cmd != "" {
		spec.Cmd = best.Cmd
	}
	if spec.Image == "" {
		spec.Image = best.ImageRef
	}
	return spec
}

// SnapshotRecord describes ONE data artifact in the bundle and — honestly —
// what consistency it can claim. A tar archive is not proof of database
// consistency: the Consistency field records what the copy actually is, and
// Detection records how the engine was decided, including when that decision
// was a heuristic the operator should double-check.
type SnapshotRecord struct {
	Name        string `json:"name"`        // volume key or accessory name
	Role        string `json:"role"`        // "app-volume" | "accessory"
	Engine      string `json:"engine"`      // "" for plain volumes; postgres|mysql|mariadb|mongo|redis|nucleus|generic
	Method      string `json:"method"`      // tar | pg_dump | mysqldump | mongodump | redis-bgsave
	Consistency string `json:"consistency"` // engine-consistent | engine-snapshot | crash-consistent | quiesced
	Detection   string `json:"detection"`   // image-pattern | volume-pattern | operator-override | teploy-stop
	Artifact    string `json:"artifact"`    // member path inside the bundle
	// Image is the accessory image the snapshot was taken from (empty for
	// app volumes) — what an isolated restore boots to validate the dump.
	Image string `json:"image,omitempty"`
	// EngineParams carries the NON-SECRET parameters a dump needs to land
	// in a scratch engine (postgres/mysql database + user names). These are
	// identifiers, not credentials; passwords never travel in the manifest.
	EngineParams map[string]string `json:"engine_params,omitempty"`
	Notes        string            `json:"notes,omitempty"`
}

// RecoveryPlan is the documented recovery sequence carried in the bundle:
// what a restoring operator must do beyond running the restore command —
// accessory upgrades, credential rotation, storage-path changes, and the
// explicit cutover. Steps may be manual; the bundle carries what's needed
// and says which steps are manual.
type RecoveryPlan struct {
	Steps []RecoveryStep `json:"steps"`
}

type RecoveryStep struct {
	ID      string `json:"id"`
	Action  string `json:"action"` // restore-isolated | validate | cutover | accessory-upgrade | credential-rotation | storage-path | tls-material | deploy
	Summary string `json:"summary"`
	Command string `json:"command,omitempty"`
	Manual  bool   `json:"manual"`
}

// BundleStore reads and writes bundle members. Two implementations exist:
// S3 (the normal remote target, via the server's aws CLI) and a plain
// directory on a filesystem the server can reach (offline bundles: copy a
// directory tree off a dying host, restore from it). Both use the same
// member layout. A bundle is COMPLETE only when its manifest.json exists —
// creators upload the manifest last, and readers treat a missing manifest
// as "no bundle".
type BundleStore interface {
	// Upload publishes one server-side file as a bundle member. Callers
	// upload manifest.json LAST.
	Upload(ctx context.Context, exec ssh.Executor, app, id, member, serverPath string) error
	// Download fetches a bundle member to a server-side path.
	Download(ctx context.Context, exec ssh.Executor, app, id, member, destPath string) error
	// FetchManifest returns the manifest bytes.
	FetchManifest(ctx context.Context, exec ssh.Executor, app, id string) ([]byte, error)
	// ListIDs returns the bundle IDs present for an app.
	ListIDs(ctx context.Context, exec ssh.Executor, app string) ([]string, error)
}

// S3BundleStore stores bundles at s3://<bucket>/<app>/dr/<id>/<member>.
// (S3 has no directory atomics: a mid-upload failure can leave a prefix
// without a manifest. Such prefixes surface in ListIDs but fail loudly at
// FetchManifest with "bundle absent or incomplete" — an incomplete bundle is
// never silently usable.)
type S3BundleStore struct{ S3 S3Config }

func (s S3BundleStore) key(app, id, member string) string {
	return fmt.Sprintf("s3://%s/%s/dr/%s/%s", s.S3.Bucket, app, id, member)
}

func (s S3BundleStore) Upload(ctx context.Context, exec ssh.Executor, app, id, member, serverPath string) error {
	_, err := exec.Run(ctx, s.S3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(serverPath), ssh.ShellQuote(s.key(app, id, member)))))
	if err != nil {
		return fmt.Errorf("uploading %s: %w", s.key(app, id, member), err)
	}
	return nil
}

func (s S3BundleStore) Download(ctx context.Context, exec ssh.Executor, app, id, member, destPath string) error {
	_, err := exec.Run(ctx, s.S3.AWS(fmt.Sprintf("s3 cp %s %s", ssh.ShellQuote(s.key(app, id, member)), ssh.ShellQuote(destPath))))
	if err != nil {
		return fmt.Errorf("downloading %s: %w", s.key(app, id, member), err)
	}
	return nil
}

func (s S3BundleStore) FetchManifest(ctx context.Context, exec ssh.Executor, app, id string) ([]byte, error) {
	if err := ValidateDate(id); err != nil {
		return nil, err
	}
	out, err := exec.Run(ctx, s.S3.AWS(fmt.Sprintf("s3 cp %s -", ssh.ShellQuote(s.key(app, id, "manifest.json")))))
	if err != nil {
		return nil, fmt.Errorf("fetching bundle manifest (bundle absent or incomplete?): %w", err)
	}
	return []byte(out), nil
}

func (s S3BundleStore) ListIDs(ctx context.Context, exec ssh.Executor, app string) ([]string, error) {
	prefix := fmt.Sprintf("s3://%s/%s/dr/", s.S3.Bucket, app)
	out, err := exec.Run(ctx, s.S3.AWS(fmt.Sprintf("s3 ls %s", ssh.ShellQuote(prefix))))
	if err != nil {
		return nil, fmt.Errorf("listing bundles at %s: %w", prefix, err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[len(fields)-1], "/") {
			continue
		}
		id := strings.TrimSuffix(fields[len(fields)-1], "/")
		if ValidateDate(id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// DirBundleStore stores bundles as a plain directory tree
// <root>/<app>/dr/<id>/<member> on a filesystem the server can reach — the
// offline path: an operator can rsync the tree off a dying host and restore
// from it with no S3 in play.
type DirBundleStore struct{ Root string }

func (d DirBundleStore) dir(app, id string) string {
	return fmt.Sprintf("%s/%s/dr/%s", strings.TrimRight(d.Root, "/"), app, id)
}

func (d DirBundleStore) Upload(ctx context.Context, exec ssh.Executor, app, id, member, serverPath string) error {
	dst := d.dir(app, id) + "/" + member
	if _, err := exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(d.dir(app, id)+"/"+dirOf(member))); err != nil {
		return fmt.Errorf("preparing bundle dir: %w", err)
	}
	if _, err := exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(serverPath), ssh.ShellQuote(dst))); err != nil {
		return fmt.Errorf("copying bundle member %s: %w", member, err)
	}
	return nil
}

func (d DirBundleStore) Download(ctx context.Context, exec ssh.Executor, app, id, member, destPath string) error {
	src := d.dir(app, id) + "/" + member
	if _, err := exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(src), ssh.ShellQuote(destPath))); err != nil {
		return fmt.Errorf("fetching bundle member %s: %w", member, err)
	}
	return nil
}

func (d DirBundleStore) FetchManifest(ctx context.Context, exec ssh.Executor, app, id string) ([]byte, error) {
	if err := ValidateDate(id); err != nil {
		return nil, err
	}
	data, present, err := state.ReadRemoteFile(ctx, exec, d.dir(app, id)+"/manifest.json")
	if err != nil {
		return nil, fmt.Errorf("fetching bundle manifest: %w", err)
	}
	if !present {
		return nil, fmt.Errorf("no manifest at %s — bundle absent or incomplete", d.dir(app, id))
	}
	return data, nil
}

func (d DirBundleStore) ListIDs(ctx context.Context, exec ssh.Executor, app string) ([]string, error) {
	base := fmt.Sprintf("%s/%s/dr", strings.TrimRight(d.Root, "/"), app)
	out, err := exec.Run(ctx, "find "+ssh.ShellQuote(base)+" -mindepth 2 -maxdepth 2 -name manifest.json -printf '%h\\n' 2>/dev/null | sort")
	if err != nil {
		return nil, fmt.Errorf("listing bundles under %s: %w", base, err)
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id := line[strings.LastIndexByte(line, '/')+1:]
		if ValidateDate(id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ParseBundleManifest validates and decodes a bundle manifest. Schema
// mismatches and foreign kinds are refused before any restore step runs.
func ParseBundleManifest(data []byte) (*BundleManifest, error) {
	var m BundleManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing bundle manifest: %w", err)
	}
	if m.SchemaVersion != BundleSchemaVersion {
		return nil, fmt.Errorf("unsupported bundle manifest schema version %d (this CLI understands %d)", m.SchemaVersion, BundleSchemaVersion)
	}
	if m.Kind != BundleKindDR {
		return nil, fmt.Errorf("manifest kind %q is not a disaster-recovery bundle", m.Kind)
	}
	if m.ID == "" || m.App == "" {
		return nil, fmt.Errorf("bundle manifest is missing its id or app")
	}
	if err := ValidateDate(m.ID); err != nil {
		return nil, fmt.Errorf("bundle manifest id: %w", err)
	}
	return &m, nil
}

// BundleOptions configures one CreateBundle run.
type BundleOptions struct {
	App    string
	Config config.AppConfig // the local teploy.yml (volumes, accessories, tls refs)

	// IncludeSecrets opts IN encrypted secret material (age ciphertexts,
	// the resolved app .env, accessory credential files). Never default.
	IncludeSecrets bool
	// IncludeAgeKey also carries /deployments/.age-key so a fresh host can
	// decrypt the ciphertexts. Even more sensitive; never default.
	IncludeAgeKey bool

	// StopApp stops the app's web containers before snapshotting volumes
	// (and restarts them after): the copies are then QUIESCED rather than
	// crash-consistent. Accessories keep running — their snapshots go
	// through engine-native dump tooling and do not need quiescing.
	StopApp bool
	// VolumeQuiesced lists volume names the OPERATOR asserts are already
	// quiesced (writer stopped by hand); recorded as quiesced with
	// detection=operator-override. teploy cannot verify the assertion.
	VolumeQuiesced []string

	// Version stamps CreatedBy (the CLI version string).
	Version string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// engineDataPathPatterns maps container destinations that LOOK like live
// database data directories to the engine they suggest. Heuristic only —
// recorded in the manifest as volume-pattern detection with an explicit
// caveat, never as proof of consistency. Long, distinctive paths (postgres,
// mysql, mongo) match by substring; short ones (redis /data) only EXACTLY —
// "/data" as a substring also matches "/app/data", which is not redis.
var engineDataPathPatterns = []struct {
	engine string
	exact  bool
	hints  []string
}{
	{"postgres", false, []string{"/var/lib/postgresql", "pgdata"}},
	{"mysql", false, []string{"/var/lib/mysql", "/var/lib/mariadb"}},
	{"mongo", false, []string{"/data/db"}},
	{"redis", true, []string{"/data"}},
}

// guessVolumeEngine inspects a volume's CONTAINER destination path for
// known engine data-dir patterns. Returns "" when nothing matches.
func guessVolumeEngine(containerPath string) string {
	p := strings.ToLower(strings.TrimRight(containerPath, "/"))
	if p == "" {
		return ""
	}
	for _, pat := range engineDataPathPatterns {
		for _, hint := range pat.hints {
			if pat.exact {
				if p == hint {
					return pat.engine
				}
				continue
			}
			if strings.Contains(p, hint) {
				return pat.engine
			}
		}
	}
	return ""
}

// CreateBundle assembles a complete DR bundle on the server and publishes it
// through the store. The manifest is uploaded LAST so a partial upload can
// never be mistaken for a bundle.
func (c *Client) CreateBundle(ctx context.Context, opts BundleOptions, store BundleStore) (*BundleManifest, error) {
	if !safeName.MatchString(opts.App) {
		return nil, fmt.Errorf("invalid app name %q", opts.App)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	id, err := newBackupID(now())
	if err != nil {
		return nil, err
	}

	quiesced := make(map[string]bool, len(opts.VolumeQuiesced))
	for _, v := range opts.VolumeQuiesced {
		if _, ok := opts.Config.Volumes[v]; !ok {
			return nil, fmt.Errorf("volume %q (--quiesced-volume) is not a volume of %s (volumes: %v)", v, opts.App, volumeNames(opts.Config))
		}
		quiesced[v] = true
	}

	// State first: an app with no on-server state has nothing to recover —
	// data-only backup (`teploy backup create`) is the right tool, and this
	// error says so instead of minting a bundle that lies about scope.
	stateBytes, present, err := state.ReadRemoteFile(ctx, c.exec, fmt.Sprintf("%s/%s/state.json", deploymentsDir, opts.App))
	if err != nil {
		return nil, fmt.Errorf("reading app state: %w", err)
	}
	if !present {
		return nil, fmt.Errorf("no /deployments/%s/state.json on the server — the app was never deployed here; a DR bundle needs deployed state (for plain volume data use `teploy backup create`, a data-only backup)", opts.App)
	}
	var appState state.AppState
	if err := json.Unmarshal(stateBytes, &appState); err != nil {
		return nil, fmt.Errorf("parsing app state: %w", err)
	}
	if appState.SchemaVersion != state.SchemaVersionV2 {
		return nil, fmt.Errorf("unsupported state schema version %d", appState.SchemaVersion)
	}

	// Workspace: everything assembles on the server (where the data is),
	// inside one private mktemp tree.
	runOut, err := c.exec.Run(ctx, "umask 077; mktemp -d /tmp/teploy-dr.XXXXXXXX")
	if err != nil {
		return nil, fmt.Errorf("creating bundle workspace: %w", err)
	}
	ws := strings.TrimSpace(runOut)
	cleanup := func() {
		c.exec.Run(context.WithoutCancel(ctx), "rm -rf "+ssh.ShellQuote(ws))
	}

	m := &BundleManifest{
		SchemaVersion: BundleSchemaVersion,
		Kind:          BundleKindDR,
		ID:            id,
		App:           opts.App,
		Server:        c.exec.Host(),
		CreatedAt:     now().UTC(),
		CreatedBy:     opts.Version,
		State:         json.RawMessage(stateBytes),
		AppManifest:   appState.AppliedManifest,
	}

	if m.ReleaseRecords, err = c.collectReleaseRecords(ctx, opts.App); err != nil {
		cleanup()
		return nil, err
	}
	m.AppRun = deriveAppRun(stateBytes, m.ReleaseRecords)

	// Optional quiescence: stop the app's web containers so volume copies
	// are quiesced rather than crash-consistent.
	var stoppedContainers []string
	if opts.StopApp {
		names, err := c.exec.Run(ctx, fmt.Sprintf(
			"docker ps --filter label=teploy.app=%s --filter label=teploy.process=web --format '{{.Names}}'",
			ssh.ShellQuote(opts.App)))
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("listing %s containers to quiesce: %w", opts.App, err)
		}
		for _, n := range strings.Fields(strings.TrimSpace(names)) {
			if n == "" {
				continue
			}
			fmt.Fprintf(c.out, "Stopping %s for quiesced snapshot...\n", n)
			if _, err := c.exec.Run(ctx, "docker stop "+ssh.ShellQuote(n)); err != nil {
				// Restart anything already stopped before aborting.
				c.restartContainers(context.WithoutCancel(ctx), stoppedContainers)
				cleanup()
				return nil, fmt.Errorf("stopping %s for quiesced snapshot: %w", n, err)
			}
			stoppedContainers = append(stoppedContainers, n)
		}
	}
	defer c.restartContainers(context.WithoutCancel(ctx), stoppedContainers)

	// Secret material — only on explicit operator selection, with the
	// manifest stating exactly what traveled.
	secrets := secret.NewManager(c.exec)
	keys, err := secrets.List(ctx, opts.App)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("listing secrets for %s: %w", opts.App, err)
	}
	m.Secrets = SecretsRecord{Mode: "references", Keys: keys}
	m.Secrets.Recovery = "secret VALUES are not in this bundle — the keys above must exist on the restore target before cutover (`teploy secret set`), or re-create the bundle with --include-secrets"
	if opts.IncludeSecrets {
		m.Secrets.Mode = "encrypted"
		m.Secrets.Recovery = "encrypted secret material IS inside this bundle (age ciphertexts + resolved .env + accessory credentials); guard it like the live secrets"
		for _, k := range keys {
			member := "secrets/" + k + ".age"
			if err := c.wsCopy(ctx, ws, member, fmt.Sprintf("%s/%s/secrets/%s.age", deploymentsDir, opts.App, k)); err != nil {
				cleanup()
				return nil, err
			}
			m.Secrets.Included = append(m.Secrets.Included, member)
		}
		// The app .env and accessory credential files hold RESOLVED secrets
		// (generated DB passwords, DATABASE_URL) — same opt-in gate.
		included, err := c.wsCopyIfExists(ctx, ws, "env/.env", fmt.Sprintf("%s/%s/.env", deploymentsDir, opts.App))
		if err != nil {
			cleanup()
			return nil, err
		}
		if included {
			m.Secrets.Included = append(m.Secrets.Included, "env/.env")
		}
		for _, accName := range sortedAccessoryNames(opts.Config) {
			member := "env/credentials/" + accName
			included, err := c.wsCopyIfExists(ctx, ws, member, fmt.Sprintf("%s/%s/accessories/%s/credentials", deploymentsDir, opts.App, accName))
			if err != nil {
				cleanup()
				return nil, err
			}
			if included {
				m.Secrets.Included = append(m.Secrets.Included, member)
			}
		}
	}
	if opts.IncludeAgeKey {
		if err := c.wsCopy(ctx, ws, "age-key", deploymentsDir+"/.age-key"); err != nil {
			cleanup()
			return nil, err
		}
		m.Secrets.AgeKeyIncluded = true
		if !opts.IncludeSecrets {
			m.Secrets.Recovery += " — the bundle carries the age key itself (--include-age-key); use it to re-key a fresh target's secret store"
		}
	}

	// App volume snapshots. The member directory must exist before tar
	// writes into it (found live: the mock cannot catch missing paths).
	if len(opts.Config.Volumes) > 0 {
		if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(ws+"/volumes")); err != nil {
			cleanup()
			return nil, fmt.Errorf("preparing volumes in bundle workspace: %w", err)
		}
	}
	for _, volName := range volumeNames(opts.Config) {
		volDir := fmt.Sprintf("%s/%s/volumes/%s", deploymentsDir, opts.App, volName)
		member := "volumes/" + volName + ".tar.gz"
		fmt.Fprintf(c.out, "Snapshotting volume %s...\n", volName)
		if _, err := c.exec.Run(ctx, fmt.Sprintf("tar -czf %s -C %s .",
			ssh.ShellQuote(ws+"/"+member), ssh.ShellQuote(volDir))); err != nil {
			cleanup()
			return nil, fmt.Errorf("archiving volume %s: %w", volName, err)
		}
		rec := SnapshotRecord{
			Name:     volName,
			Role:     "app-volume",
			Method:   "tar",
			Artifact: member,
		}
		switch {
		case quiesced[volName]:
			rec.Consistency = consistencyQuiesced
			rec.Detection = "operator-override"
			rec.Notes = "operator asserted the writer was stopped for this copy; teploy did not verify"
		case opts.StopApp && len(stoppedContainers) > 0:
			rec.Consistency = consistencyQuiesced
			rec.Detection = "teploy-stop"
			rec.Notes = "app containers were stopped by teploy for this snapshot"
		default:
			rec.Consistency = consistencyCrash
			rec.Notes = "live bind-mount copied while the app may have been writing; only crash recovery is guaranteed — use --stop-app or an engine accessory for stronger guarantees"
			if eng := guessVolumeEngine(opts.Config.Volumes[volName]); eng != "" {
				rec.Engine = eng
				rec.Detection = "volume-pattern"
				rec.Notes = fmt.Sprintf("container path %q matches the %s data-directory pattern (heuristic, unverified) — a raw tar is NOT a %s-consistent backup; this snapshot is crash-consistent at best", opts.Config.Volumes[volName], eng, eng)
			}
		}
		m.Snapshots = append(m.Snapshots, rec)
	}

	// Accessory snapshots: engine-aware, through the same planner the
	// single-accessory backup uses (no second backup system).
	for _, accName := range sortedAccessoryNames(opts.Config) {
		accCfg := opts.Config.Accessories[accName]
		image, env, err := c.accessoryImageEnv(ctx, opts.App, accName, accCfg)
		if err != nil {
			cleanup()
			return nil, err
		}
		accWS := ws + "/acc/" + accName
		if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(accWS)); err != nil {
			cleanup()
			return nil, fmt.Errorf("preparing accessory workspace: %w", err)
		}
		fmt.Fprintf(c.out, "Snapshotting accessory %s (%s)...\n", accName, image)
		plan := planAccessoryDump(opts.App, accName, image, env, accWS)
		if err := plan.stage(ctx, c.exec); err != nil {
			return nil, fmt.Errorf("staging credential for accessory %s (no dump was run): %w", accName, err)
		}
		if _, err := c.exec.Run(ctx, plan.cmd); err != nil {
			cleanup()
			return nil, fmt.Errorf("dumping accessory %s: %w", accName, err)
		}
		member := "accessories/" + accName + plan.ext
		if err := c.wsMove(ctx, ws, member, plan.artifactPath); err != nil {
			cleanup()
			return nil, err
		}
		rec := SnapshotRecord{
			Name:        accName,
			Role:        "accessory",
			Engine:      plan.engine,
			Method:      plan.method,
			Consistency: plan.consistency,
			Detection:   "image-pattern",
			Artifact:    member,
			Image:       image,
		}
		switch plan.engine {
		case "postgres":
			db, user := postgresDBAndUser(opts.App, env)
			rec.EngineParams = map[string]string{"db": db, "user": user}
		case "mysql", "mariadb":
			rec.EngineParams = map[string]string{"db": mysqlDB(opts.App, env)}
		}
		if plan.engine == "generic" {
			rec.Notes = "generic accessory: raw tar of the bind-mounted directory while the engine ran; engine-native dump tooling for this image is not known to teploy — the isolated restore validates it by booting a scratch engine"
		}
		if plan.engine == "redis" {
			rec.Notes = "redis BGSAVE-acknowledged dump.rdb copy (appendonly=no proven at snapshot time)"
		}
		m.Snapshots = append(m.Snapshots, rec)
	}

	m.Routing = RoutingRecord{
		Domain:      appState.Domain,
		IngressMode: appState.IngressMode,
		Port:        appState.CurrentPort,
		Publish:     opts.Config.Publish,
	}
	if appState.IngressMode == config.IngressHost {
		m.Routing.Bind = opts.Config.Bind
	}
	if opts.Config.TLS != nil {
		tls := &TLSRecord{Mode: "acme"}
		if opts.Config.TLS.Internal {
			tls.Mode = "internal"
		} else if opts.Config.TLS.Cert != "" {
			tls.Mode = "custom-cert"
			tls.Cert = opts.Config.TLS.Cert
			tls.Key = opts.Config.TLS.Key
		}
		m.Routing.TLS = tls
	}

	m.Recovery = c.buildRecoveryPlan(opts, m)

	// Publish: members first, manifest LAST (completeness marker).
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("encoding bundle manifest: %w", err)
	}
	if err := c.exec.Upload(ctx, strings.NewReader(string(manifestJSON)+"\n"), ws+"/manifest.json", "0600"); err != nil {
		cleanup()
		return nil, fmt.Errorf("staging bundle manifest: %w", err)
	}

	uploadMember := func(member, serverPath string) error {
		if err := store.Upload(ctx, c.exec, opts.App, id, member, serverPath); err != nil {
			return err
		}
		return nil
	}
	for _, snap := range m.Snapshots {
		if err := uploadMember(snap.Artifact, ws+"/"+snap.Artifact); err != nil {
			cleanup()
			return nil, err
		}
	}
	for _, member := range m.Secrets.Included {
		if err := uploadMember(member, ws+"/"+member); err != nil {
			cleanup()
			return nil, err
		}
	}
	if opts.IncludeAgeKey {
		if err := uploadMember("age-key", ws+"/age-key"); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := uploadMember("manifest.json", ws+"/manifest.json"); err != nil {
		cleanup()
		return nil, err
	}

	cleanup()
	fmt.Fprintf(c.out, "DR bundle created: %s\n", id)
	return m, nil
}

func (c *Client) buildRecoveryPlan(opts BundleOptions, m *BundleManifest) RecoveryPlan {
	plan := RecoveryPlan{Steps: []RecoveryStep{
		{
			ID:      "restore-isolated",
			Action:  "restore-isolated",
			Summary: "restore the bundle to an ISOLATED staging target and validate it — live state is never touched",
			Command: fmt.Sprintf("teploy dr restore %s", m.ID),
		},
		{
			ID:      "cutover",
			Action:  "cutover",
			Summary: "after validation passes, explicitly promote the staged restore over the live app (this is the mutation step)",
			Command: fmt.Sprintf("teploy dr cutover %s", m.ID),
		},
		{
			ID:      "deploy",
			Action:  "deploy",
			Summary: "cutover restores data/state only; run a deploy to bring the app container and routing live from the restored state",
			Command: "teploy deploy",
			Manual:  true,
		},
	}}

	if len(m.Snapshots) > 0 {
		var accs, vols []string
		for _, s := range m.Snapshots {
			if s.Role == "accessory" {
				accs = append(accs, s.Name)
			} else {
				vols = append(vols, s.Name)
			}
		}
		if len(accs) > 0 {
			plan.Steps = append(plan.Steps, RecoveryStep{
				ID:      "accessory-upgrade",
				Action:  "accessory-upgrade",
				Summary: fmt.Sprintf("accessory data survives image upgrades on its bind mounts (%s): `teploy accessory upgrade <name> <new-image>` recreates the container in place; consult the engine's own major-version upgrade docs before jumping versions — restore-into-scratch (teploy dr restore) is the pre-flight", strings.Join(accs, ", ")),
				Manual:  true,
			})
		}
		if len(vols) > 0 {
			plan.Steps = append(plan.Steps, RecoveryStep{
				ID:      "storage-path",
				Action:  "storage-path",
				Summary: fmt.Sprintf("bundle artifacts are keyed by volume NAME (%s), not host path: moving /deployments or remapping volumes in teploy.yml re-homes storage without invalidating this bundle", strings.Join(vols, ", ")),
			})
		}
	}

	if m.Secrets.Mode == "encrypted" {
		plan.Steps = append(plan.Steps, RecoveryStep{
			ID:      "credential-rotation",
			Action:  "credential-rotation",
			Summary: "this bundle carries secret material — after a restore, rotate the carried credentials (`teploy secret rotate KEY`, accessory passwords) if the bundle's storage was ever outside your control",
			Manual:  true,
		})
	} else if len(m.Secrets.Keys) > 0 {
		plan.Steps = append(plan.Steps, RecoveryStep{
			ID:      "credential-rotation",
			Action:  "credential-rotation",
			Summary: fmt.Sprintf("referenced secret keys (%s) must exist on the target before cutover: `teploy secret set KEY=...` — restore preflight fails without them", strings.Join(m.Secrets.Keys, ", ")),
			Manual:  true,
		})
	}

	if m.Routing.TLS != nil && m.Routing.TLS.Mode == "custom-cert" {
		plan.Steps = append(plan.Steps, RecoveryStep{
			ID:      "tls-material",
			Action:  "tls-material",
			Summary: fmt.Sprintf("routing used a custom certificate (teploy.yml tls.cert=%s, tls.key=%s) — the PEM files are NOT in the bundle; re-supply them on the fresh host before deploying", m.Routing.TLS.Cert, m.Routing.TLS.Key),
			Manual:  true,
		})
	}
	return plan
}

// accessoryImageEnv resolves the accessory's REAL image + env (the running
// container holds the resolved credentials; teploy.yml may carry only
// `auto`/`secret:` references). Falls back to config only when the
// container is absent AND the config env carries no unresolved references.
func (c *Client) accessoryImageEnv(ctx context.Context, app, name string, cfg config.AccessoryConfig) (string, map[string]string, error) {
	if image, env, err := c.InspectAccessory(ctx, app, name); err == nil {
		return image, env, nil
	}
	for _, v := range cfg.Env {
		if v == "auto" || strings.HasPrefix(v, "secret:") {
			return "", nil, fmt.Errorf("accessory %s is not running and its teploy.yml env carries unresolved references — start it (teploy deploy) or snapshot with --stop-app after a deploy", name)
		}
	}
	return cfg.Image, cfg.Env, nil
}

func (c *Client) restartContainers(ctx context.Context, names []string) {
	for _, n := range names {
		if n == "" {
			continue
		}
		fmt.Fprintf(c.out, "Restarting %s...\n", n)
		if _, err := c.exec.Run(ctx, "docker start "+ssh.ShellQuote(n)); err != nil {
			fmt.Fprintf(c.out, "WARNING: could not restart %s after bundling: %v\n", n, err)
		}
	}
}

// wsCopy copies a server file into the bundle workspace under member,
// creating parent directories. Missing source is an error (callers decide
// which absences are fine and use wsCopyIfExists).
func (c *Client) wsCopy(ctx context.Context, ws, member, src string) error {
	dst := ws + "/" + member
	if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(ws+"/"+dirOf(member))); err != nil {
		return fmt.Errorf("preparing %s in bundle workspace: %w", member, err)
	}
	if _, err := c.exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(src), ssh.ShellQuote(dst))); err != nil {
		return fmt.Errorf("copying %s into the bundle: %w", src, err)
	}
	return nil
}

// wsCopyIfExists copies src into the workspace under member when it exists
// and reports whether it was included; a confirmed-absent source records
// nothing (the manifest's Included list is the source of truth). Transport
// failures are errors.
func (c *Client) wsCopyIfExists(ctx context.Context, ws, member, src string) (bool, error) {
	exists, err := c.remoteExists(ctx, src)
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", src, err)
	}
	if !exists {
		return false, nil
	}
	if err := c.wsCopy(ctx, ws, member, src); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) wsMove(ctx context.Context, ws, member, src string) error {
	dst := ws + "/" + member
	if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(ws+"/"+dirOf(member))); err != nil {
		return fmt.Errorf("preparing %s in bundle workspace: %w", member, err)
	}
	if _, err := c.exec.Run(ctx, fmt.Sprintf("mv -f %s %s", ssh.ShellQuote(src), ssh.ShellQuote(dst))); err != nil {
		return fmt.Errorf("moving %s into the bundle: %w", src, err)
	}
	return nil
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

func volumeNames(cfg config.AppConfig) []string {
	names := make([]string, 0, len(cfg.Volumes))
	for k := range cfg.Volumes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func sortedAccessoryNames(cfg config.AppConfig) []string {
	names := make([]string, 0, len(cfg.Accessories))
	for k := range cfg.Accessories {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func (c *Client) remoteExists(ctx context.Context, path string) (bool, error) {
	out, err := c.exec.Run(ctx, fmt.Sprintf("if [ -e %s ]; then printf 'present\\n'; else printf 'absent\\n'; fi", ssh.ShellQuote(path)))
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "present":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("checking %s: unrecognized output", path)
}

func (c *Client) collectReleaseRecords(ctx context.Context, app string) ([]json.RawMessage, error) {
	metaDir := fmt.Sprintf("%s/%s/meta", deploymentsDir, app)
	out, err := c.exec.Run(ctx, fmt.Sprintf("find %s -maxdepth 1 -name '*.json' -type f 2>/dev/null | sort", ssh.ShellQuote(metaDir)))
	if err != nil {
		return nil, fmt.Errorf("listing release records: %w", err)
	}
	var records []json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		data, present, err := state.ReadRemoteFile(ctx, c.exec, line)
		if err != nil {
			return nil, fmt.Errorf("reading release record %s: %w", line, err)
		}
		if !present {
			continue
		}
		records = append(records, json.RawMessage(data))
	}
	return records, nil
}
