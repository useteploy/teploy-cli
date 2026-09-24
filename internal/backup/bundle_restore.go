package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Isolated restore + explicit cutover — C07's core safety property: live
// overwrite is NEVER the default. `RestoreBundleIsolated` lands a bundle in
// a staging area under /var/tmp, boots throwaway scratch engines/app
// containers against the STAGED data, validates them, and writes a receipt
// with measured RPO/RTO. Nothing under /deployments is touched until
// `CutoverBundle` — a separate, explicit command — promotes the staged data
// with two-phase recovery discipline.

// DRStagingRoot is where isolated restores stage. /var/tmp (disk-backed),
// not /tmp (often tmpfs — see verify.go's 28 GB lesson).
const DRStagingRoot = "/var/tmp/teploy-dr"

// RestoreReceiptSchemaVersion versions the restore receipt.
const RestoreReceiptSchemaVersion = 1

// DRStagingPath is the deterministic staging directory for one bundle of
// one app — deterministic so a later `teploy dr cutover` finds it without
// state of its own.
func DRStagingPath(app, id string) string {
	return fmt.Sprintf("%s/%s/%s", DRStagingRoot, app, id)
}

// drStagingRE pins the exact shape of staging paths: the recursive wipe on
// re-restore must only ever match this pattern, never an operator path.
var drStagingRE = regexp.MustCompile(`^/var/tmp/teploy-dr/[A-Za-z0-9][A-Za-z0-9._-]*/[0-9]{8}-[0-9]{6}(-[0-9a-f]{16})?$`)

// DRCheckResult is one validation check in the receipt.
type DRCheckResult struct {
	Name   string `json:"name"`             // e.g. "accessory:db (postgres)", "app"
	Kind   string `json:"kind"`             // "data" | "app"
	Status string `json:"status"`           // "pass" | "fail" | "skipped"
	Metric string `json:"metric,omitempty"` // e.g. "tables=12"
	Detail string `json:"detail,omitempty"` // skip/fail reason
}

// RestoreReceipt is the measured outcome of an isolated restore. RPO =
// data age at restore time (restore start − bundle creation): the window of
// writes the bundle cannot contain. RTO = measured wall time from restore
// start to validated staging (download + extract + validation) — the
// recovery-time floor before cutover.
type RestoreReceipt struct {
	SchemaVersion int             `json:"schema_version"`
	BundleID      string          `json:"bundle_id"`
	App           string          `json:"app"`
	StartedAt     time.Time       `json:"started_at"`
	ValidatedAt   time.Time       `json:"validated_at"`
	RPOSeconds    int64           `json:"rpo_seconds"`
	RTOSeconds    int64           `json:"rto_seconds"`
	StagingPath   string          `json:"staging_path"`
	Checks        []DRCheckResult `json:"checks"`
	OK            bool            `json:"ok"`
	NextStep      string          `json:"next_step"`
}

// CutoverReceipt records what the explicit cutover changed and where the
// pre-cutover originals were preserved.
type CutoverReceipt struct {
	SchemaVersion int       `json:"schema_version"`
	BundleID      string    `json:"bundle_id"`
	App           string    `json:"app"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Promoted      []string  `json:"promoted"`      // live paths replaced from staging
	RecoveryDirs  []string  `json:"recovery_dirs"` // where originals were preserved
	Stopped       []string  `json:"stopped"`       // containers stopped for the cutover
	NextStep      string    `json:"next_step"`
}

// BundleRestoreOptions configures an isolated restore or cutover.
type BundleRestoreOptions struct {
	App    string
	ID     string
	Config config.AppConfig // the restore-time local teploy.yml
	// Now overrides the clock (tests).
	Now func() time.Time
}

// ErrValidationRequired is returned by cutover when no validated staging
// exists for the bundle — the explicit rejection of cutting over unproven
// data.
var ErrValidationRequired = errors.New("no validated staged restore for this bundle — run `teploy dr restore` first")

// RestoreBundleIsolated restores a DR bundle into an isolated staging area,
// validates it against scratch containers, and writes the RPO/RTO receipt.
// Live state under /deployments is never touched.
func (c *Client) RestoreBundleIsolated(ctx context.Context, opts BundleRestoreOptions, store BundleStore) (*RestoreReceipt, error) {
	started := timeNow(opts.Now)

	// ---- Preflight: everything here is read-only. Missing keys, wrong
	// schema, wrong app, corrupt manifest — all fail BEFORE any mutation.
	manifest, err := c.fetchAndParseManifest(ctx, store, opts.App, opts.ID)
	if err != nil {
		return nil, err
	}
	if err := c.secretsPreflight(ctx, manifest); err != nil {
		return nil, err
	}

	staging := DRStagingPath(opts.App, manifest.ID)
	if !drStagingRE.MatchString(staging) {
		return nil, fmt.Errorf("internal error: staging path %q fails the safety pattern", staging)
	}

	// ---- Staging (isolated: /var/tmp/teploy-dr/..., never /deployments).
	// A re-restore of the same bundle wipes only this bundle's staging tree.
	if _, err := c.exec.Run(ctx, "rm -rf "+ssh.ShellQuote(staging)); err != nil {
		return nil, fmt.Errorf("clearing previous staging: %w", err)
	}
	if _, err := c.exec.Run(ctx, "umask 077; mkdir -p "+ssh.ShellQuote(staging)); err != nil {
		return nil, fmt.Errorf("creating staging dir: %w", err)
	}

	// ---- Download every artifact named by the manifest (+ secret members).
	fmt.Fprintf(c.out, "Downloading bundle %s into %s...\n", manifest.ID, staging)
	if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(staging+"/volumes")+" "+ssh.ShellQuote(staging+"/accessories")); err != nil {
		return nil, fmt.Errorf("preparing staging layout: %w", err)
	}
	for _, snap := range manifest.Snapshots {
		if err := store.Download(ctx, c.exec, opts.App, manifest.ID, snap.Artifact, staging+"/"+snap.Artifact); err != nil {
			return nil, err
		}
	}
	for _, member := range manifest.Secrets.Included {
		if err := store.Download(ctx, c.exec, opts.App, manifest.ID, member, staging+"/"+member); err != nil {
			return nil, err
		}
	}
	if manifest.Secrets.AgeKeyIncluded {
		if err := store.Download(ctx, c.exec, opts.App, manifest.ID, "age-key", staging+"/age-key"); err != nil {
			return nil, err
		}
	}
	// Keep the manifest itself in staging: cutover re-reads it without the
	// store, and the receipt records which bundle produced this tree.
	if manifestBytes, err := json.MarshalIndent(manifest, "", "  "); err == nil {
		_ = c.exec.Upload(context.WithoutCancel(ctx), strings.NewReader(string(manifestBytes)+"\n"), staging+"/manifest.json", "0600")
	}

	// ---- Integrity: every gzip artifact must prove itself BEFORE any
	// engine boots off it. A corrupt bundle fails here, originals
	// untouched (they were never touched at all — this is staging).
	for _, snap := range manifest.Snapshots {
		if strings.HasSuffix(snap.Artifact, ".gz") {
			if _, err := c.exec.Run(ctx, "gzip -t "+ssh.ShellQuote(staging+"/"+snap.Artifact)); err != nil {
				return nil, fmt.Errorf("bundle member %s is corrupt (gzip -t failed): %w — refusing to restore from this bundle", snap.Artifact, err)
			}
		}
	}

	// ---- Encrypted-material decrypt proof. When the bundle carries
	// ciphertexts without the age key, the TARGET's age key must decrypt
	// them — proven on one member before anything else runs.
	if err := c.decryptProof(ctx, manifest, staging); err != nil {
		return nil, err
	}

	receipt := &RestoreReceipt{
		SchemaVersion: RestoreReceiptSchemaVersion,
		BundleID:      manifest.ID,
		App:           opts.App,
		StartedAt:     started.UTC(),
		StagingPath:   staging,
		NextStep:      fmt.Sprintf("teploy dr cutover %s", manifest.ID),
	}

	// ---- Extract app volumes + generic accessory tars into staging.
	for _, snap := range manifest.Snapshots {
		switch snap.Role {
		case "app-volume":
			dest := staging + "/volumes/" + snap.Name
			if _, err := c.exec.Run(ctx, fmt.Sprintf("mkdir -p %s && tar -xzf %s -C %s",
				ssh.ShellQuote(dest), ssh.ShellQuote(staging+"/"+snap.Artifact), ssh.ShellQuote(dest))); err != nil {
				return nil, c.failReceipt(ctx, receipt, fmt.Errorf("extracting volume %s: %w", snap.Name, err))
			}
		case "accessory":
			if snap.Method == "tar" {
				dest := staging + "/accessories/" + snap.Name
				if _, err := c.exec.Run(ctx, fmt.Sprintf("mkdir -p %s && tar -xzf %s -C %s",
					ssh.ShellQuote(dest), ssh.ShellQuote(staging+"/"+snap.Artifact), ssh.ShellQuote(dest))); err != nil {
					return nil, c.failReceipt(ctx, receipt, fmt.Errorf("extracting accessory %s: %w", snap.Name, err))
				}
			}
		}
	}

	// ---- Engine validation: scratch containers, never live ones.
	for _, snap := range manifest.Snapshots {
		if snap.Role != "accessory" || snap.Method == "tar" {
			continue
		}
		check := c.validateEngineSnapshot(ctx, manifest, snap, staging)
		receipt.Checks = append(receipt.Checks, check)
	}

	// ---- App check: boot the recorded image on the STAGED volumes.
	receipt.Checks = append(receipt.Checks, c.appCheck(ctx, manifest, opts, staging))

	receipt.ValidatedAt = timeNow(opts.Now).UTC()
	receipt.RPOSeconds = int64(receipt.StartedAt.Sub(manifest.CreatedAt).Seconds())
	receipt.RTOSeconds = int64(receipt.ValidatedAt.Sub(receipt.StartedAt).Seconds())
	receipt.OK = allPassed(receipt.Checks)

	if receipt.OK {
		fmt.Fprintf(c.out, "Isolated restore validated.\n")
	} else {
		fmt.Fprintf(c.out, "Isolated restore FAILED validation — staging kept at %s for inspection; nothing live was touched\n", staging)
	}
	writeReceipt(ctx, c.exec, staging+"/receipt.json", receipt)
	return receipt, nil
}

// fetchAndParseManifest reads and validates the manifest without side
// effects. App mismatch, bad schema, foreign kind, and malformed JSON all
// die here — before any mutation anywhere.
func (c *Client) fetchAndParseManifest(ctx context.Context, store BundleStore, app, id string) (*BundleManifest, error) {
	data, err := store.FetchManifest(ctx, c.exec, app, id)
	if err != nil {
		return nil, err
	}
	m, err := ParseBundleManifest(data)
	if err != nil {
		return nil, err
	}
	if m.App != app {
		return nil, fmt.Errorf("bundle %s belongs to app %q, not %q — refusing to restore onto the wrong app", m.ID, m.App, app)
	}
	if m.ID != id {
		return nil, fmt.Errorf("manifest id %q does not match the requested bundle %q", m.ID, id)
	}
	return m, nil
}

// secretsPreflight enforces the missing-keys-fail-first rule: when the
// bundle carries secret REFERENCES only, every referenced key must already
// exist on the target, and the missing ones are listed BEFORE any staging,
// download, or container runs. (In references mode the restored app cannot
// boot without them, so discovering that after mutating anything would be
// the classic restore-time surprise.)
func (c *Client) secretsPreflight(ctx context.Context, manifest *BundleManifest) error {
	if manifest.Secrets.Mode != "references" || len(manifest.Secrets.Keys) == 0 {
		return nil
	}
	secrets := secret.NewManager(c.exec)
	present, err := secrets.List(ctx, manifest.App)
	if err != nil {
		return fmt.Errorf("listing target secrets for preflight: %w", err)
	}
	have := make(map[string]bool, len(present))
	for _, k := range present {
		have[k] = true
	}
	var missing []string
	for _, k := range manifest.Secrets.Keys {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("bundle references secret(s) missing on this target: %s — set them first (`teploy secret set KEY=...`) or restore a bundle created with --include-secrets; nothing has been touched", strings.Join(missing, ", "))
	}
	return nil
}

// decryptProof: in encrypted mode the ciphertexts must actually decrypt with
// the key that will be available at cutover — the bundled age-key when
// present, else the target's own /deployments/.age-key. Proven on one member
// before any engine work; failure aborts with staging-only footprint.
func (c *Client) decryptProof(ctx context.Context, manifest *BundleManifest, staging string) error {
	if manifest.Secrets.Mode != "encrypted" || len(manifest.Secrets.Included) == 0 {
		return nil
	}
	var member string
	for _, inc := range manifest.Secrets.Included {
		if strings.HasSuffix(inc, ".age") {
			member = inc
			break
		}
	}
	if member == "" {
		return nil // only .env/credentials included; no ciphertext to prove
	}
	keyPath := deploymentsDir + "/.age-key"
	if manifest.Secrets.AgeKeyIncluded {
		keyPath = staging + "/age-key"
	}
	// age -d reads the file; output goes to the shell's trash — this is a
	// decryptability proof, not an extraction.
	cmd := fmt.Sprintf("age -d -i %s %s >/dev/null 2>&1", ssh.ShellQuote(keyPath), ssh.ShellQuote(staging+"/"+member))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		if manifest.Secrets.AgeKeyIncluded {
			return fmt.Errorf("the bundled age key cannot decrypt the bundled ciphertext %s — the bundle is internally inconsistent; nothing has been touched", member)
		}
		return fmt.Errorf("this target's age key cannot decrypt the bundle's secret material (ciphertext %s) — the bundle was encrypted on %s; supply that host's /deployments/.age-key on this target, or restore a references-only bundle and set the secrets by hand; nothing has been touched", member, manifest.Server)
	}
	return nil
}

// validateEngineSnapshot boots a scratch engine, restores the dump into it,
// and checks the restored data is real. The scratch container is always torn
// down. The LIVE accessory (if any) is never contacted.
func (c *Client) validateEngineSnapshot(ctx context.Context, manifest *BundleManifest, snap SnapshotRecord, staging string) DRCheckResult {
	check := DRCheckResult{
		Name: fmt.Sprintf("accessory:%s (%s)", snap.Name, snap.Engine),
		Kind: "data",
	}
	scratch := manifest.App + "-" + snap.Name + "-drcheck"
	teardown := func() {
		c.exec.Run(context.WithoutCancel(ctx), "docker rm -f "+ssh.ShellQuote(scratch)+" >/dev/null 2>&1 || true")
	}
	// Remove stale scratch from an interrupted earlier run.
	c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(scratch)+" >/dev/null 2>&1 || true")
	defer teardown()

	dump := staging + "/" + snap.Artifact
	image := snap.Image
	if image == "" {
		check.Status = "skipped"
		check.Detail = "manifest records no image for this snapshot (older bundle?) — data check skipped; the artifact remains staged"
		return check
	}

	switch snap.Engine {
	case "postgres":
		db := snap.EngineParams["db"]
		user := snap.EngineParams["user"]
		if user == "" {
			user = "postgres"
		}
		if _, err := c.exec.Run(ctx, fmt.Sprintf(
			"docker run -d --name %s -e POSTGRES_DB=%s -e POSTGRES_USER=%s -e POSTGRES_HOST_AUTH_METHOD=trust %s >/dev/null",
			ssh.ShellQuote(scratch), ssh.ShellQuote(db), ssh.ShellQuote(user), ssh.ShellQuote(image))); err != nil {
			return checkFailed(check, fmt.Errorf("starting scratch postgres: %w", err))
		}
		if err := c.waitReady(ctx, scratch, fmt.Sprintf("pg_isready -U %s", ssh.ShellQuote(user)), 30); err != nil {
			return checkFailed(check, err)
		}
		if _, err := c.exec.Run(ctx, postgresRestoreCmd(scratch, user, db, dump, staging+"/"+snap.Name+".sql")); err != nil {
			return checkFailed(check, fmt.Errorf("restoring dump into scratch: %w", err))
		}
		out, err := c.exec.Run(ctx, fmt.Sprintf(
			"docker exec %s psql -tA -U %s %s -c \"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public'\"",
			ssh.ShellQuote(scratch), ssh.ShellQuote(user), ssh.ShellQuote(db)))
		if err != nil {
			return checkFailed(check, fmt.Errorf("verify query: %w", err))
		}
		n := atoiSafe(strings.TrimSpace(out))
		check.Metric = fmt.Sprintf("tables=%d", n)
		if n == 0 {
			check.Status = "fail"
			check.Detail = "restored database has zero tables"
			return check
		}
		check.Status = "pass"
		return check

	case "mysql", "mariadb":
		db := snap.EngineParams["db"]
		if _, err := c.exec.Run(ctx, fmt.Sprintf(
			"docker run -d --name %s -e MYSQL_DATABASE=%s -e MYSQL_ALLOW_EMPTY_PASSWORD=yes %s >/dev/null",
			ssh.ShellQuote(scratch), ssh.ShellQuote(db), ssh.ShellQuote(image))); err != nil {
			return checkFailed(check, fmt.Errorf("starting scratch mysql: %w", err))
		}
		if err := c.waitReady(ctx, scratch, "mysqladmin ping -u root --silent", 40); err != nil {
			return checkFailed(check, err)
		}
		if _, err := c.exec.Run(ctx, mysqlRestoreCmd(scratch, db, "", dump, staging+"/"+snap.Name+".sql")); err != nil {
			return checkFailed(check, fmt.Errorf("restoring dump into scratch: %w", err))
		}
		out, err := c.exec.Run(ctx, fmt.Sprintf("docker exec %s sh -c %s", ssh.ShellQuote(scratch),
			ssh.ShellQuote(fmt.Sprintf("mysql -u root -N -e \"SHOW TABLES\" %s | wc -l", db))))
		if err != nil {
			return checkFailed(check, fmt.Errorf("verify query: %w", err))
		}
		n := atoiSafe(strings.TrimSpace(out))
		check.Metric = fmt.Sprintf("tables=%d", n)
		if n == 0 {
			check.Status = "fail"
			check.Detail = "restored database has zero tables"
			return check
		}
		check.Status = "pass"
		return check

	case "mongo":
		if _, err := c.exec.Run(ctx, fmt.Sprintf("docker run -d --name %s %s >/dev/null",
			ssh.ShellQuote(scratch), ssh.ShellQuote(image))); err != nil {
			return checkFailed(check, fmt.Errorf("starting scratch mongo: %w", err))
		}
		probe := "sh -c 'mongosh --quiet --eval 1 || mongo --quiet --eval 1'"
		if err := c.waitReady(ctx, scratch, probe, 30); err != nil {
			return checkFailed(check, err)
		}
		if _, err := c.exec.Run(ctx, mongoRestoreCmd(scratch, dump)); err != nil {
			return checkFailed(check, fmt.Errorf("restoring dump into scratch: %w", err))
		}
		out, err := c.exec.Run(ctx, fmt.Sprintf(
			"docker exec %s sh -c 'mongosh --quiet --eval \"db.getMongo().getDBNames().length\" || mongo --quiet --eval \"db.getMongo().getDBNames().length\"'",
			ssh.ShellQuote(scratch)))
		if err != nil {
			return checkFailed(check, fmt.Errorf("verify query: %w", err))
		}
		check.Metric = "dbs=" + strings.TrimSpace(out)
		check.Status = "pass"
		return check

	case "redis":
		// Seed a created (not running) container with the restored rdb, then
		// start it: redis boots FROM the backup, and a corrupt rdb fails the
		// readiness probe (same shape as verify.go).
		rdb := staging + "/" + snap.Name + ".rdb"
		if _, err := c.exec.Run(ctx, fmt.Sprintf(
			"gunzip -c %s > %s && docker create --name %s %s >/dev/null && docker cp %s %s:/data/dump.rdb && docker start %s >/dev/null",
			ssh.ShellQuote(dump), ssh.ShellQuote(rdb), ssh.ShellQuote(scratch), ssh.ShellQuote(image),
			ssh.ShellQuote(rdb), ssh.ShellQuote(scratch), ssh.ShellQuote(scratch))); err != nil {
			return checkFailed(check, fmt.Errorf("seeding scratch redis: %w", err))
		}
		if err := c.waitReady(ctx, scratch, "redis-cli ping", 20); err != nil {
			return checkFailed(check, err)
		}
		out, err := c.exec.Run(ctx, fmt.Sprintf("docker exec %s redis-cli dbsize", ssh.ShellQuote(scratch)))
		if err != nil {
			return checkFailed(check, fmt.Errorf("verify query: %w", err))
		}
		check.Metric = "keys=" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "(integer)"))
		check.Status = "pass"
		return check

	default:
		check.Status = "skipped"
		check.Detail = fmt.Sprintf("engine %q has no scratch validator (yet) — artifact staged, cutover restores it verbatim", snap.Engine)
		return check
	}
}

// appCheck boots the bundle's recorded image on the STAGED volumes with the
// bundled env (when the operator included it) and reports whether it reaches
// running state. It is isolated: no network alias, no published port, no
// Caddy involvement — it can never receive production traffic.
func (c *Client) appCheck(ctx context.Context, manifest *BundleManifest, opts BundleRestoreOptions, staging string) DRCheckResult {
	check := DRCheckResult{Name: "app", Kind: "app"}

	var appState state.AppState
	if err := json.Unmarshal(manifest.State, &appState); err != nil {
		check.Status = "skipped"
		check.Detail = "bundle state unparseable — application check skipped (data checks still ran)"
		return check
	}
	image := manifest.AppRun.Image
	if image == "" {
		image = appState.ImageRef
	}
	if image == "" {
		check.Status = "skipped"
		check.Detail = "bundle carries no deployed image reference — application check skipped (data checks still ran)"
		return check
	}
	scratch := manifest.App + "-dr-appcheck"
	c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(scratch)+" >/dev/null 2>&1 || true")
	defer c.exec.Run(context.WithoutCancel(ctx), "docker rm -f "+ssh.ShellQuote(scratch)+" >/dev/null 2>&1 || true")

	args := []string{"docker", "run", "-d", "--name", ssh.ShellQuote(scratch),
		"--label", "teploy.app=" + manifest.App, "--label", "teploy.role=dr-appcheck"}
	if envFile := stagedEnvFile(manifest, staging); envFile != "" {
		args = append(args, "--env-file", ssh.ShellQuote(envFile))
	}
	for _, vol := range sortedVolumeMounts(opts.Config) {
		args = append(args, "-v", ssh.ShellQuote(staging+"/volumes/"+vol.name+":"+vol.dest))
	}
	args = append(args, ssh.ShellQuote(image))
	// The recorded container command, element-wise quoted (a command with
	// quoted spaces cannot be represented — recorded verbatim by deploy as
	// a single string; splitting on fields matches how teploy.yml's legacy
	// command: is documented).
	for _, w := range strings.Fields(manifest.AppRun.Cmd) {
		args = append(args, ssh.ShellQuote(w))
	}

	if _, err := c.exec.Run(ctx, strings.Join(args, " ")); err != nil {
		// An unavailable image is a SKIP with the reason, not a silent
		// pass: the operator sees exactly what was not proven.
		if strings.Contains(err.Error(), "Unable to find image") || strings.Contains(err.Error(), "pull access denied") || strings.Contains(err.Error(), "not found") {
			check.Status = "skipped"
			check.Detail = fmt.Sprintf("image %s unavailable on this host and could not be pulled — application check skipped", appState.ImageRef)
			return check
		}
		return checkFailed(check, fmt.Errorf("starting scratch app container: %w", err))
	}
	// Bounded wait for "running": docker ps filtering by exact name.
	waitCmd := fmt.Sprintf(
		"for i in $(seq 1 15); do s=$(docker inspect -f '{{.State.Status}}' %s 2>/dev/null) && [ \"$s\" = running ] && exit 0; sleep 2; done; docker logs --tail 20 %s >&2; exit 1",
		ssh.ShellQuote(scratch), ssh.ShellQuote(scratch))
	if _, err := c.exec.Run(ctx, waitCmd); err != nil {
		return checkFailed(check, fmt.Errorf("app container never reached running state: %w", err))
	}
	check.Status = "pass"
	check.Metric = fmt.Sprintf("image=%s running", appState.ImageRef)
	return check
}

func stagedEnvFile(manifest *BundleManifest, staging string) string {
	for _, inc := range manifest.Secrets.Included {
		if inc == "env/.env" {
			return staging + "/env/.env"
		}
	}
	return ""
}

type volumeMount struct{ name, dest string }

func sortedVolumeMounts(cfg config.AppConfig) []volumeMount {
	out := make([]volumeMount, 0, len(cfg.Volumes))
	for name, dest := range cfg.Volumes {
		out = append(out, volumeMount{name, dest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// CutoverBundle is the EXPLICIT promotion step. It requires a validated
// staged restore (its receipt), re-runs the secrets preflight, and only then
// — under the app lock, with two-phase promotion — replaces live data.
// Originals are preserved in recovery directories named in the receipt.
func (c *Client) CutoverBundle(ctx context.Context, opts BundleRestoreOptions) (*CutoverReceipt, error) {
	startedAt := timeNow(opts.Now)
	staging := DRStagingPath(opts.App, opts.ID)
	if !drStagingRE.MatchString(staging) {
		return nil, fmt.Errorf("internal error: staging path %q fails the safety pattern", staging)
	}

	// The staged restore must exist AND have passed validation.
	receiptData, present, err := state.ReadRemoteFile(ctx, c.exec, staging+"/receipt.json")
	if err != nil {
		return nil, fmt.Errorf("reading the staged restore receipt: %w", err)
	}
	if !present {
		return nil, fmt.Errorf("%w: %s", ErrValidationRequired, staging)
	}
	var receipt RestoreReceipt
	if err := json.Unmarshal(receiptData, &receipt); err != nil {
		return nil, fmt.Errorf("parsing the staged restore receipt: %w", err)
	}
	if !receipt.OK {
		return nil, fmt.Errorf("the staged restore for bundle %s FAILED validation — cutover refused; inspect %s or re-run teploy dr restore", opts.ID, staging)
	}

	manifestData, present, err := state.ReadRemoteFile(ctx, c.exec, staging+"/manifest.json")
	if err != nil || !present {
		return nil, fmt.Errorf("reading the staged bundle manifest from %s: %v", staging, err)
	}
	manifest, err := ParseBundleManifest(manifestData)
	if err != nil {
		return nil, err
	}
	if manifest.App != opts.App {
		return nil, fmt.Errorf("staged bundle belongs to app %q, not %q", manifest.App, opts.App)
	}
	if err := c.secretsPreflight(ctx, manifest); err != nil {
		return nil, err
	}

	// Cross-check: every accessory/volume the bundle captured must exist in
	// the restore-time teploy.yml, or cutover would silently drop data.
	for _, snap := range manifest.Snapshots {
		switch snap.Role {
		case "accessory":
			if _, ok := opts.Config.Accessories[snap.Name]; !ok {
				return nil, fmt.Errorf("bundle captured accessory %q but the restore-time teploy.yml does not define it — add it (or accept losing its data by removing it from the bundle) before cutover", snap.Name)
			}
		case "app-volume":
			if _, ok := opts.Config.Volumes[snap.Name]; !ok {
				return nil, fmt.Errorf("bundle captured volume %q but the restore-time teploy.yml does not define it — map it in teploy.yml before cutover", snap.Name)
			}
		}
	}

	out := &CutoverReceipt{
		SchemaVersion: RestoreReceiptSchemaVersion,
		BundleID:      manifest.ID,
		App:           opts.App,
		StartedAt:     startedAt.UTC(),
		NextStep:      "teploy deploy — bring the app container and routing live from the restored state",
	}

	// App lock: cutover is a mutating critical section like a deploy (C01).
	lk, err := state.AcquireLockFenced(ctx, c.exec, opts.App)
	if err != nil {
		return nil, fmt.Errorf("acquiring the app lock for cutover: %w", err)
	}
	defer state.ReleaseLockFenced(c.exec, lk, opts.App)

	// Stop live containers (web + accessories). They are STOPPED, not
	// removed — an aborted cutover restarts them.
	liveOut, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker ps --filter label=teploy.app=%s --format '{{.Names}}'", ssh.ShellQuote(opts.App)))
	if err != nil {
		return nil, fmt.Errorf("listing live containers: %w", err)
	}
	var stopped []string
	for _, n := range strings.Fields(strings.TrimSpace(liveOut)) {
		if n == "" {
			continue
		}
		fmt.Fprintf(c.out, "Stopping %s...\n", n)
		if _, err := c.exec.Run(ctx, "docker stop "+ssh.ShellQuote(n)); err != nil {
			c.restartContainers(context.WithoutCancel(ctx), stopped)
			return nil, fmt.Errorf("stopping %s for cutover (it was left as-is): %w", n, err)
		}
		stopped = append(stopped, n)
	}
	out.Stopped = stopped

	fail := func(err error) (*CutoverReceipt, error) {
		// Best-effort restart of the stopped containers so the pre-cutover
		// state is not left dark; staged data and recovery dirs are KEPT
		// and named. If a promotion already happened for some path, the
		// receipt lists it and the error says exactly where originals are.
		c.restartContainers(context.WithoutCancel(ctx), stopped)
		writeReceipt(context.WithoutCancel(ctx), c.exec, staging+"/cutover-receipt.json", out)
		return out, fmt.Errorf("%w — staged data kept at %s, pre-cutover originals kept in %v; stopped containers were restarted", err, staging, out.RecoveryDirs)
	}

	// Engine-dump accessories first: their data lands through the engine,
	// and a failure here leaves the volume/generic promotions untouched.
	for _, snap := range manifest.Snapshots {
		if snap.Role != "accessory" || snap.Method == "tar" {
			continue
		}
		accCfg := opts.Config.Accessories[snap.Name]
		accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, opts.App, snap.Name)

		// Preserve any pre-existing engine data dir: the dump must land in
		// a FRESH engine (pg_dump has no DROP unless asked; restoring over
		// populated tables errors mid-way). Moved aside, kept, named.
		nonEmpty, err := c.exec.Run(ctx, fmt.Sprintf("if [ -z \"$(ls -A %s 2>/dev/null)\" ]; then printf 'empty\\n'; else printf 'nonempty\\n'; fi", ssh.ShellQuote(accDir)))
		if err != nil {
			return fail(fmt.Errorf("inspecting accessory dir %s: %w", accDir, err))
		}
		if strings.TrimSpace(nonEmpty) == "nonempty" {
			// Only keys with data ON DISK are preserved — a configured key
			// that never materialized (volume added to teploy.yml after
			// the bundle, a re-run after a partial failure) has nothing
			// to move, and an accDir holding only env/credential files
			// must not abort the cutover.
			var toMove []string
			for _, volKey := range sortedVolumeKeys(accCfg) {
				existsOut, err := c.exec.Run(ctx, fmt.Sprintf("if [ -e %s ]; then printf 'present\\n'; else printf 'absent\\n'; fi",
					ssh.ShellQuote(accDir+"/"+volKey)))
				if err != nil {
					return fail(fmt.Errorf("inspecting %s volume dir %s: %w", snap.Name, volKey, err))
				}
				if strings.TrimSpace(existsOut) == "present" {
					toMove = append(toMove, volKey)
				}
			}
			if len(toMove) > 0 {
				recOut, err := c.exec.Run(ctx, "mktemp -d "+ssh.ShellQuote(accDir+".pre-cutover.XXXXXX"))
				if err != nil {
					return fail(fmt.Errorf("creating pre-cutover copy dir for %s: %w", accDir, err))
				}
				recDir := strings.TrimSpace(recOut)
				// Move each VOLUME directory (the engine's data) aside; env
				// files and credentials in accDir stay for the fresh accessory.
				accParent := fmt.Sprintf("%s/%s/accessories", deploymentsDir, opts.App)
				for _, volKey := range toMove {
					move := fmt.Sprintf("mv -f %s %s", ssh.ShellQuote(accDir+"/"+volKey), ssh.ShellQuote(recDir+"/"+volKey))
					if _, err := c.exec.Run(ctx, move); err != nil {
						// Engine images chown their data dir to their own uid
						// with mode 700 (postgres = uid 70), and a non-root
						// deploy user may not be able to rename it. Same
						// reality reconcileDataOwnership solves: retry
						// through a throwaway root container (the accessory
						// image itself provides the shell).
						if mvErr := c.rootMoveAside(ctx, accCfg.Image, accParent, snap.Name, pathBase(recDir), volKey); mvErr != nil {
							return fail(fmt.Errorf("moving aside pre-cutover data for %s volume %s (plain mv: %v; root move: %v)", snap.Name, volKey, err, mvErr))
						}
					}
				}
				out.RecoveryDirs = append(out.RecoveryDirs, recDir)
				fmt.Fprintf(c.out, "Pre-cutover data for %s preserved at %s\n", snap.Name, recDir)
			}
		}

		// Start the accessory from config (fresh dirs), then pipe the dump.
		mgr := accessories.NewManager(c.exec, c.out)
		env, err := mgr.EnsureRunning(ctx, opts.App, snap.Name, accCfg)
		if err != nil {
			return fail(fmt.Errorf("starting accessory %s for cutover: %w", snap.Name, err))
		}
		container := accessories.ContainerName(opts.App, snap.Name)
		dump := staging + "/" + snap.Artifact
		var restoreErr error
		switch snap.Engine {
		case "postgres":
			db, user := postgresDBAndUser(opts.App, engineEnv(snap, env, accCfg))
			if err := c.waitReady(ctx, container, fmt.Sprintf("pg_isready -U %s", ssh.ShellQuote(user)), 30); err != nil {
				restoreErr = err
			} else {
				_, restoreErr = c.exec.Run(ctx, postgresRestoreCmd(container, user, db, dump, staging+"/"+snap.Name+".cutover.sql"))
			}
		case "mysql", "mariadb":
			db := mysqlDB(opts.App, engineEnv(snap, env, accCfg))
			if err := c.waitReady(ctx, container, "mysqladmin ping -u root --silent", 40); err != nil {
				restoreErr = err
			} else {
				_, restoreErr = c.exec.Run(ctx, mysqlRestoreCmd(container, db, mysqlRootPassword(engineEnv(snap, env, accCfg)), dump, staging+"/"+snap.Name+".cutover.sql"))
			}
		case "mongo":
			_, restoreErr = c.exec.Run(ctx, mongoRestoreCmd(container, dump))
		case "redis":
			if err := c.redisAOFPreflight(ctx, opts.App, snap.Name); err != nil {
				restoreErr = err
			} else {
				_, restoreErr = c.exec.Run(ctx, redisRestoreScript(container, dump, staging+"/"+snap.Name+".cutover.rdb", staging+"/"+snap.Name+".cutover-old.rdb"))
			}
		default:
			restoreErr = fmt.Errorf("engine %q has no cutover restore path", snap.Engine)
		}
		if restoreErr != nil {
			return fail(fmt.Errorf("restoring %s dump at cutover: %w (its pre-cutover data, if any, is in the recovery dirs)", snap.Name, restoreErr))
		}
		fmt.Fprintf(c.out, "Restored %s dump into %s\n", snap.Name, container)
	}

	// Generic accessory tars + app volumes: two-phase promotion.
	for _, snap := range manifest.Snapshots {
		var stageDir, liveDir string
		switch snap.Role {
		case "accessory":
			if snap.Method != "tar" {
				continue
			}
			stageDir = staging + "/accessories/" + snap.Name
			liveDir = fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, opts.App, snap.Name)
		case "app-volume":
			stageDir = staging + "/volumes/" + snap.Name
			liveDir = fmt.Sprintf("%s/%s/volumes/%s", deploymentsDir, opts.App, snap.Name)
		default:
			continue
		}
		recoveryDir, err := promoteStaged(ctx, c.exec, stageDir, liveDir, c.out, staging)
		if err != nil {
			// promoteStaged already restored the originals on rollback; the
			// recovery-incomplete variant preserves them split across dirs.
			return fail(fmt.Errorf("promoting %s at cutover: %w", snap.Name, err))
		}
		if recoveryDir != "" {
			// Deliberately KEPT (not deleted): recovery is exactly the
			// moment an operator may want the pre-cutover copy back, and
			// the receipt names where it is. Remove manually once confident.
			out.RecoveryDirs = append(out.RecoveryDirs, recoveryDir)
		}
		out.Promoted = append(out.Promoted, liveDir)
	}

	// State, release records, and secret material land LAST — after all
	// data promotions succeeded, so a failed promotion never leaves the
	// target claiming a generation it does not have.
	if err := c.installBundleState(ctx, manifest, staging); err != nil {
		return fail(fmt.Errorf("installing restored state: %w", err))
	}

	out.CompletedAt = timeNow(opts.Now).UTC()
	writeReceipt(ctx, c.exec, staging+"/cutover-receipt.json", out)
	fmt.Fprintf(c.out, "Cutover complete. Pre-cutover originals kept in %v\n", out.RecoveryDirs)
	fmt.Fprintf(c.out, "Next: %s\n", out.NextStep)
	return out, nil
}

// rootMoveAside renames mountDir/srcDir/srcName to mountDir/dstDir/srcName
// through a throwaway root container of the accessory's own image — the
// reconcileDataOwnership pattern for engine data dirs a non-root deploy
// user cannot move (directory renames need write on the directory itself,
// and engines chown their data to their own uid).
func (c *Client) rootMoveAside(ctx context.Context, image, mountDir, srcDir, dstDir, name string) error {
	if !safeName.MatchString(srcDir) || !safeName.MatchString(dstDir) || !safeName.MatchString(name) {
		return fmt.Errorf("refusing root-container move of unsafe path segment (%q, %q, %q)", srcDir, dstDir, name)
	}
	inner := fmt.Sprintf("mv /w/%s/%s /w/%s/%s", srcDir, name, dstDir, name)
	cmd := fmt.Sprintf("docker run --rm --user 0 -v %s:/w --entrypoint sh %s -c %s",
		ssh.ShellQuote(mountDir), ssh.ShellQuote(image), ssh.ShellQuote(inner))
	if _, err := c.exec.Run(ctx, cmd); err != nil {
		return err
	}
	return nil
}

func sortedVolumeKeys(cfg config.AccessoryConfig) []string {
	keys := make([]string, 0, len(cfg.Volumes))
	for k := range cfg.Volumes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// engineEnv merges the manifest's non-secret engine params with the running
// accessory's resolved env so cutover restores land in the same database the
// bundle was dumped from (fresh-host edge: POSTGRES_* env from config).
func engineEnv(snap SnapshotRecord, liveEnv map[string]string, cfg config.AccessoryConfig) map[string]string {
	merged := make(map[string]string)
	for k, v := range cfg.Env {
		merged[k] = v
	}
	for k, v := range liveEnv {
		merged[k] = v // resolved values (auto passwords, DATABASE_URL) win
	}
	if p := snap.EngineParams; p != nil {
		if db := p["db"]; db != "" && merged["POSTGRES_DB"] == "" && merged["MYSQL_DATABASE"] == "" {
			switch snap.Engine {
			case "postgres":
				merged["POSTGRES_DB"] = db
			case "mysql", "mariadb":
				merged["MYSQL_DATABASE"] = db
			}
		}
	}
	return merged
}

// installBundleState writes the bundle's state.json, release records and
// secret material into /deployments/<app> — atomically per file, 0600.
func (c *Client) installBundleState(ctx context.Context, manifest *BundleManifest, staging string) error {
	appDir := fmt.Sprintf("%s/%s", deploymentsDir, manifest.App)
	if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(appDir+"/meta")+" "+ssh.ShellQuote(appDir+"/secrets")); err != nil {
		return fmt.Errorf("preparing state dirs: %w", err)
	}
	if err := c.exec.Upload(ctx, strings.NewReader(string(manifest.State)+"\n"), appDir+"/state.json", "0600"); err != nil {
		return fmt.Errorf("writing restored state.json: %w", err)
	}
	for _, rec := range manifest.ReleaseRecords {
		var r struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(rec, &r); err != nil || r.Hash == "" {
			continue // a record without identity cannot be keyed; skip loudly below
		}
		if err := c.exec.Upload(ctx, strings.NewReader(string(rec)+"\n"), fmt.Sprintf("%s/meta/%s.json", appDir, r.Hash), "0600"); err != nil {
			return fmt.Errorf("writing release record %s: %w", r.Hash, err)
		}
	}
	if manifest.Secrets.Mode == "encrypted" {
		for _, inc := range manifest.Secrets.Included {
			switch {
			case strings.HasPrefix(inc, "secrets/"):
				key := strings.TrimSuffix(strings.TrimPrefix(inc, "secrets/"), ".age")
				if _, err := c.exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(staging+"/"+inc), ssh.ShellQuote(appDir+"/secrets/"+key+".age"))); err != nil {
					return fmt.Errorf("installing secret ciphertext %s: %w", key, err)
				}
			case inc == "env/.env":
				if _, err := c.exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(staging+"/"+inc), ssh.ShellQuote(appDir+"/.env"))); err != nil {
					return fmt.Errorf("installing restored .env: %w", err)
				}
			case strings.HasPrefix(inc, "env/credentials/"):
				name := strings.TrimPrefix(inc, "env/credentials/")
				accDir := fmt.Sprintf("%s/%s/accessories/%s", deploymentsDir, manifest.App, name)
				if _, err := c.exec.Run(ctx, "mkdir -p "+ssh.ShellQuote(accDir)); err != nil {
					return fmt.Errorf("preparing accessory dir for credentials: %w", err)
				}
				if _, err := c.exec.Run(ctx, fmt.Sprintf("cp -p %s %s", ssh.ShellQuote(staging+"/"+inc), ssh.ShellQuote(accDir+"/credentials"))); err != nil {
					return fmt.Errorf("installing credentials for %s: %w", name, err)
				}
			}
		}
		if manifest.Secrets.AgeKeyIncluded {
			// Install the bundled age key as the target's own ONLY when the
			// target has none — never silently replace an existing key.
			hasKey, err := c.remoteExists(ctx, deploymentsDir+"/.age-key")
			if err != nil {
				return fmt.Errorf("checking the target age key: %w", err)
			}
			if !hasKey {
				if _, err := c.exec.Run(ctx, fmt.Sprintf("cp -p %s %s && chmod 600 %s",
					ssh.ShellQuote(staging+"/age-key"), ssh.ShellQuote(deploymentsDir+"/.age-key"), ssh.ShellQuote(deploymentsDir+"/.age-key"))); err != nil {
					return fmt.Errorf("installing the bundled age key: %w", err)
				}
			}
		}
	}
	return nil
}

func (c *Client) failReceipt(ctx context.Context, receipt *RestoreReceipt, err error) error {
	receipt.Checks = append(receipt.Checks, DRCheckResult{Name: "restore", Kind: "data", Status: "fail", Detail: err.Error()})
	receipt.ValidatedAt = time.Now().UTC()
	receipt.OK = false
	writeReceipt(context.WithoutCancel(ctx), c.exec, receipt.StagingPath+"/receipt.json", receipt)
	return err
}

func writeReceipt(ctx context.Context, exec ssh.Executor, path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = exec.Upload(context.WithoutCancel(ctx), strings.NewReader(string(data)+"\n"), path, "0600")
}

func allPassed(checks []DRCheckResult) bool {
	for _, ck := range checks {
		if ck.Status == "fail" {
			return false
		}
	}
	return true
}

func checkFailed(check DRCheckResult, err error) DRCheckResult {
	check.Status = "fail"
	check.Detail = err.Error()
	return check
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func timeNow(f func() time.Time) time.Time {
	if f != nil {
		return f()
	}
	return time.Now()
}
