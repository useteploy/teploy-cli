package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const (
	deploymentsDir  = "/deployments"
	SchemaVersionV2 = 2
)

// staleLockTTL is how long an "auto" deploy lock (see LockInfo.Type) is
// honored before AcquireLock treats it as abandoned and breaks it. Deploys
// that crash or lose their SSH connection mid-flight — before the deferred
// ReleaseLock runs — would otherwise leave the app locked forever, needing
// a human to notice and run `teploy unlock`. 30 minutes is generous enough
// to never plausibly false-positive against a real, still-running deploy
// (remote image builds, long health-check timeouts, migration hooks), but
// short enough to self-heal within one operational cycle. Manual locks
// (AcquireManualLock) are never auto-broken — those are an explicit,
// intentional freeze with no expiry.
const staleLockTTL = 30 * time.Minute

// staleHealLockTTL is how long a "heal" lock (see LockInfo.Type and
// AcquireHealLock) is honored before it's treated as abandoned. Heal restarts
// a single container and releases immediately, so a heal lock should never
// live more than a few seconds; 2 minutes is a generous ceiling that still
// lets a crashed heal's lock be broken quickly — by the next heal run OR by a
// deploy — instead of blocking deploys for the full 30-minute auto TTL. The
// invariant heal preserves: a running DEPLOY's lock (auto/manual) is never
// breakable by heal, at any age; heal only ever breaks its OWN stale lock.
const staleHealLockTTL = 2 * time.Minute

// ReleaseMetadata identifies an applied release. PreviousRelease retains the
// one-level rollback target's identity without creating a second state store.
type ReleaseMetadata struct {
	Hash            string          `json:"hash,omitempty"`
	ManifestSHA256  string          `json:"manifest_sha256,omitempty"`
	SourceRevision  string          `json:"source_revision,omitempty"`
	ImageRef        string          `json:"image_ref,omitempty"`
	ImageDigest     string          `json:"image_digest,omitempty"`
	UpdatedAt       time.Time       `json:"updated_at,omitempty"`
	OperationID     string          `json:"operation_id,omitempty"`
	Generation      uint64          `json:"generation,omitempty"`
	AppliedManifest json.RawMessage `json:"applied_manifest,omitempty"`
}

// MigrationMetadata records the first import from the legacy key=value file.
type MigrationMetadata struct {
	Source string `json:"source"`
}

// AppState represents the authoritative applied state for an app. V2 is
// stored at /deployments/<app>/state.json; the old key=value state file is
// read only when state.json does not yet exist.
type AppState struct {
	SchemaVersion  int       `json:"schema_version"`
	DeploymentType string    `json:"deployment_type"`
	IngressMode    string    `json:"ingress_mode"`
	Domain         string    `json:"domain,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
	SourceRevision string    `json:"source_revision,omitempty"`
	ImageRef       string    `json:"image_ref,omitempty"`
	ImageDigest    string    `json:"image_digest,omitempty"`
	OperationID    string    `json:"operation_id"`
	Generation     uint64    `json:"generation"`

	AppliedManifest json.RawMessage    `json:"applied_manifest,omitempty"`
	PreviousRelease *ReleaseMetadata   `json:"previous_release,omitempty"`
	Migration       *MigrationMetadata `json:"migration,omitempty"`

	CurrentPort  int    `json:"current_port,omitempty"` // primary port (first replica or single instance)
	CurrentHash  string `json:"current_hash"`
	PreviousPort int    `json:"previous_port,omitempty"`
	PreviousHash string `json:"previous_hash,omitempty"`
	// Replica ports. When len > 1, the app has multiple replicas on this server.
	// CurrentPort is always CurrentPorts[0] for backwards compatibility.
	CurrentPorts  []int `json:"current_ports,omitempty"`
	PreviousPorts []int `json:"previous_ports,omitempty"`

	legacyImported bool
}

// LogEntry represents a single entry in /deployments/teploy.log.
type LogEntry struct {
	Timestamp  time.Time `json:"ts"`
	App        string    `json:"app"`
	Type       string    `json:"type"` // deploy, rollback, restart, health_failure
	Hash       string    `json:"hash,omitempty"`
	Success    bool      `json:"success"`
	DurationMs int64     `json:"duration_ms"`
	Message    string    `json:"message,omitempty"`
}

// Read reads canonical v2 state first. A missing v2 file falls back to the
// legacy key=value file; malformed canonical JSON is an error and never falls
// back to potentially stale legacy state.
func Read(ctx context.Context, exec ssh.Executor, app string) (*AppState, error) {
	v2Path := fmt.Sprintf("%s/%s/state.json", deploymentsDir, app)
	output, err := exec.Run(ctx, fmt.Sprintf("cat -- %s 2>/dev/null", v2Path))
	if err == nil && strings.TrimSpace(output) != "" {
		var s AppState
		if err := json.Unmarshal([]byte(output), &s); err != nil {
			return nil, fmt.Errorf("parsing canonical state for %s: %w", app, err)
		}
		if s.SchemaVersion != SchemaVersionV2 {
			return nil, fmt.Errorf("unsupported state schema version %d for %s", s.SchemaVersion, app)
		}
		if err := validateManifestDigest(&s); err != nil {
			return nil, fmt.Errorf("validating canonical state for %s: %w", app, err)
		}
		if err := validateReleaseDigest(s.PreviousRelease); err != nil {
			return nil, fmt.Errorf("validating previous release for %s: %w", app, err)
		}
		normalizePorts(&s)
		return &s, nil
	}

	path := fmt.Sprintf("%s/%s/state", deploymentsDir, app)
	output, err = exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", path))
	if err != nil || strings.TrimSpace(output) == "" {
		return nil, nil
	}

	s := &AppState{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "current_port":
			s.CurrentPort, _ = strconv.Atoi(parts[1])
		case "current_hash":
			s.CurrentHash = parts[1]
		case "previous_port":
			s.PreviousPort, _ = strconv.Atoi(parts[1])
		case "previous_hash":
			s.PreviousHash = parts[1]
		case "domain":
			s.Domain = parts[1]
		case "current_ports":
			s.CurrentPorts = parsePorts(parts[1])
		case "previous_ports":
			s.PreviousPorts = parsePorts(parts[1])
		}
	}
	s.SchemaVersion = 1
	s.legacyImported = true
	normalizePorts(s)
	return s, nil
}

func normalizePorts(s *AppState) {
	// Backwards compat: if replica lists are absent, derive them from the
	// primary ports used by legacy state and early v2 writers.
	if len(s.CurrentPorts) == 0 && s.CurrentPort > 0 {
		s.CurrentPorts = []int{s.CurrentPort}
	}
	if len(s.PreviousPorts) == 0 && s.PreviousPort > 0 {
		s.PreviousPorts = []int{s.PreviousPort}
	}
}

// parsePorts parses a comma-separated list of ports.
func parsePorts(s string) []int {
	var ports []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			ports = append(ports, n)
		}
	}
	return ports
}

// formatPorts formats a port slice as comma-separated string.
func formatPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// NewAppliedState creates the identity envelope for the next successful
// operation. Callers fill workload-specific ports, hashes, and release fields.
func NewAppliedState(current *AppState, deploymentType, ingressMode, domain string) *AppState {
	if deploymentType == "" {
		deploymentType = "container"
	}
	if ingressMode == "" {
		ingressMode = "caddy"
	}

	s := &AppState{
		SchemaVersion:  SchemaVersionV2,
		DeploymentType: deploymentType,
		IngressMode:    ingressMode,
		Domain:         domain,
		UpdatedAt:      time.Now().UTC(),
		OperationID:    newOperationID(),
		Generation:     1,
	}
	if current == nil {
		return s
	}
	s.Generation = current.Generation + 1
	if current.legacyImported {
		s.Migration = &MigrationMetadata{Source: "legacy-key-value"}
	} else if current.Migration != nil {
		migration := *current.Migration
		s.Migration = &migration
	}
	s.PreviousRelease = current.ReleaseMetadata()
	return s
}

// ReleaseMetadata returns a copy of the current release identity.
func (s *AppState) ReleaseMetadata() *ReleaseMetadata {
	if s == nil || s.CurrentHash == "" {
		return nil
	}
	return &ReleaseMetadata{
		Hash:            s.CurrentHash,
		ManifestSHA256:  s.ManifestSHA256,
		SourceRevision:  s.SourceRevision,
		ImageRef:        s.ImageRef,
		ImageDigest:     s.ImageDigest,
		UpdatedAt:       s.UpdatedAt,
		OperationID:     s.OperationID,
		Generation:      s.Generation,
		AppliedManifest: cloneRawMessage(s.AppliedManifest),
	}
}

// ApplyRelease copies retained release identity onto the current state.
func (s *AppState) ApplyRelease(release *ReleaseMetadata) {
	if release == nil {
		return
	}
	s.ManifestSHA256 = release.ManifestSHA256
	s.SourceRevision = release.SourceRevision
	s.ImageRef = release.ImageRef
	s.ImageDigest = release.ImageDigest
	s.AppliedManifest = cloneRawMessage(release.AppliedManifest)
}

func cloneRawMessage(in json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), in...)
}

func newOperationID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	fallback := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(fallback[:16])
}

func validateManifestDigest(s *AppState) error {
	if len(s.AppliedManifest) == 0 {
		return nil
	}
	if !json.Valid(s.AppliedManifest) {
		return fmt.Errorf("applied_manifest is not valid JSON")
	}
	digest := sha256.Sum256(s.AppliedManifest)
	want := hex.EncodeToString(digest[:])
	if s.ManifestSHA256 == "" {
		s.ManifestSHA256 = want
		return nil
	}
	if s.ManifestSHA256 != want {
		return fmt.Errorf("manifest_sha256 does not match applied_manifest")
	}
	return nil
}

func validateReleaseDigest(release *ReleaseMetadata) error {
	if release == nil || len(release.AppliedManifest) == 0 {
		return nil
	}
	if !json.Valid(release.AppliedManifest) {
		return fmt.Errorf("applied_manifest is not valid JSON")
	}
	digest := sha256.Sum256(release.AppliedManifest)
	want := hex.EncodeToString(digest[:])
	if release.ManifestSHA256 == "" {
		release.ManifestSHA256 = want
		return nil
	}
	if release.ManifestSHA256 != want {
		return fmt.Errorf("manifest_sha256 does not match applied_manifest")
	}
	return nil
}

// Write atomically writes canonical v2 JSON. It never updates the legacy
// key=value file, making that file an import-only migration source rather than
// a second writable authority.
func Write(ctx context.Context, exec ssh.Executor, app string, s *AppState) error {
	if s == nil {
		return fmt.Errorf("state is required")
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersionV2
	}
	if s.SchemaVersion != SchemaVersionV2 {
		return fmt.Errorf("cannot write state schema version %d", s.SchemaVersion)
	}
	if s.DeploymentType == "" {
		s.DeploymentType = "container"
	}
	if s.IngressMode == "" {
		s.IngressMode = "caddy"
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now().UTC()
	}
	if s.OperationID == "" {
		s.OperationID = newOperationID()
	}
	if s.Generation == 0 {
		s.Generation = 1
	}
	if err := validateManifestDigest(s); err != nil {
		return err
	}
	if err := validateReleaseDigest(s.PreviousRelease); err != nil {
		return fmt.Errorf("validating previous release: %w", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	data = append(data, '\n')
	path := fmt.Sprintf("%s/%s/state.json", deploymentsDir, app)
	return ssh.UploadAtomic(ctx, exec, bytes.NewReader(data), path, "0644")
}

// LockInfo represents the metadata stored in a .lock directory.
type LockInfo struct {
	Type    string `json:"type"` // "auto" or "manual"
	User    string `json:"user,omitempty"`
	Message string `json:"message,omitempty"`
	TS      string `json:"ts"`
}

// ReadLock reads the lock info for an app. Returns nil if no lock exists.
func ReadLock(ctx context.Context, exec ssh.Executor, app string) (*LockInfo, error) {
	lockPath := fmt.Sprintf("%s/%s/.lock/info", deploymentsDir, app)
	output, err := exec.Run(ctx, fmt.Sprintf("cat %s 2>/dev/null", lockPath))
	if err != nil || strings.TrimSpace(output) == "" {
		return nil, nil
	}
	var info LockInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &info); err != nil {
		return nil, nil
	}
	return &info, nil
}

// AcquireLock acquires the deploy lock for an app using atomic mkdir.
// Returns an error if a lock already exists (another deploy in progress or
// manual freeze) and isn't stale (see staleLockTTL).
func AcquireLock(ctx context.Context, exec ssh.Executor, app string) error {
	lockPath := fmt.Sprintf("%s/%s/.lock", deploymentsDir, app)
	if _, err := tryMkdirLock(ctx, exec, lockPath); err != nil {
		info, _ := ReadLock(ctx, exec, app)
		if info != nil && info.Type == "manual" {
			msg := fmt.Sprintf("Deploy locked by %s", info.User)
			if info.Message != "" {
				msg += fmt.Sprintf(": '%s'", info.Message)
			}
			msg += fmt.Sprintf(". Locked at %s. Use 'teploy unlock' to release.", info.TS)
			return fmt.Errorf("%s", msg)
		}
		if info != nil && info.Type == "auto" && isStale(info.TS) {
			ReleaseLock(ctx, exec, app)
			if _, retryErr := tryMkdirLock(ctx, exec, lockPath); retryErr != nil {
				// Someone else's deploy won the race to re-acquire right
				// after we broke the stale lock — fall through to the
				// normal "in progress" error below.
				return fmt.Errorf("deploy is already in progress for %s", app)
			}
			return writeLockInfo(ctx, exec, lockPath, app)
		}
		// A crashed heal can leave its short-lived "heal" lock behind. A deploy
		// (authoritative) may break a STALE heal lock so it isn't blocked — but
		// a FRESH heal lock (heal mid-restart, seconds) falls through to the
		// "in progress" error below, so the deploy yields for that brief window
		// rather than interrupting a heal restart.
		if info != nil && info.Type == "heal" && isHealStale(info.TS) {
			ReleaseLock(ctx, exec, app)
			if _, retryErr := tryMkdirLock(ctx, exec, lockPath); retryErr != nil {
				return fmt.Errorf("deploy is already in progress for %s", app)
			}
			return writeLockInfo(ctx, exec, lockPath, app)
		}
		return fmt.Errorf("deploy is already in progress for %s", app)
	}
	return writeLockInfo(ctx, exec, lockPath, app)
}

func tryMkdirLock(ctx context.Context, exec ssh.Executor, lockPath string) (string, error) {
	return exec.Run(ctx, fmt.Sprintf("mkdir %s 2>/dev/null", lockPath))
}

func writeLockInfo(ctx context.Context, exec ssh.Executor, lockPath, app string) error {
	info, _ := json.Marshal(LockInfo{
		Type: "auto",
		TS:   time.Now().UTC().Format(time.RFC3339),
	})
	if err := exec.Upload(ctx, bytes.NewReader(info), lockPath+"/info", "0644"); err != nil {
		// Lock directory was created but info file failed — release and return error.
		ReleaseLock(ctx, exec, app)
		return fmt.Errorf("writing lock info: %w", err)
	}
	return nil
}

// isStale reports whether an "auto" lock's timestamp is older than
// staleLockTTL. An unparseable timestamp is treated as stale — a lock file
// too corrupted to read its own age isn't one worth respecting.
func isStale(ts string) bool {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	return time.Since(parsed) > staleLockTTL
}

// AcquireManualLock places a manual deploy freeze on an app.
func AcquireManualLock(ctx context.Context, exec ssh.Executor, app, user, message string) error {
	lockPath := fmt.Sprintf("%s/%s/.lock", deploymentsDir, app)
	if _, err := exec.Run(ctx, fmt.Sprintf("mkdir %s 2>/dev/null", lockPath)); err != nil {
		return fmt.Errorf("app %s is already locked", app)
	}

	info, _ := json.Marshal(LockInfo{
		Type:    "manual",
		User:    user,
		Message: message,
		TS:      time.Now().UTC().Format(time.RFC3339),
	})
	return exec.Upload(ctx, bytes.NewReader(info), lockPath+"/info", "0644")
}

// AcquireHealLock acquires a short-lived "heal" lock for an app so a heal
// restart is mutually exclusive with a deploy. It is deliberately NOT
// symmetric with AcquireLock:
//
//   - It YIELDS to any existing deploy lock (auto/manual) and to a fresh heal
//     lock — returning (false, nil), never breaking it. A running deploy's
//     lock must never be broken by heal, at any age (a >30-min deploy is legit:
//     image builds, migrations).
//   - It only ever breaks its OWN stale heal lock (a crashed prior heal), so
//     heal can't be permanently blocked by its own dead predecessor.
//
// Returns (true, nil) when the lock was acquired and the caller must release it
// (defer ReleaseLock), (false, nil) when heal should skip this app because
// something else holds the lock, or (false, err) on an unexpected failure.
func AcquireHealLock(ctx context.Context, exec ssh.Executor, app string) (bool, error) {
	lockPath := fmt.Sprintf("%s/%s/.lock", deploymentsDir, app)
	if _, err := tryMkdirLock(ctx, exec, lockPath); err != nil {
		info, _ := ReadLock(ctx, exec, app)
		// Break only our own stale heal lock; yield to everything else.
		if info != nil && info.Type == "heal" && isHealStale(info.TS) {
			ReleaseLock(ctx, exec, app)
			if _, retryErr := tryMkdirLock(ctx, exec, lockPath); retryErr != nil {
				return false, nil // lost the race — yield
			}
			if err := writeHealLockInfo(ctx, exec, lockPath, app); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil // deploy/manual/fresh-heal lock present — yield, never break
	}
	if err := writeHealLockInfo(ctx, exec, lockPath, app); err != nil {
		return false, err
	}
	return true, nil
}

func writeHealLockInfo(ctx context.Context, exec ssh.Executor, lockPath, app string) error {
	info, _ := json.Marshal(LockInfo{
		Type: "heal",
		TS:   time.Now().UTC().Format(time.RFC3339),
	})
	if err := exec.Upload(ctx, bytes.NewReader(info), lockPath+"/info", "0644"); err != nil {
		ReleaseLock(ctx, exec, app)
		return fmt.Errorf("writing heal lock info: %w", err)
	}
	return nil
}

// isHealStale reports whether a "heal" lock's timestamp is older than
// staleHealLockTTL. An unparseable timestamp is treated as stale.
func isHealStale(ts string) bool {
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	return time.Since(parsed) > staleHealLockTTL
}

// ReleaseLock releases the deploy lock for an app.
func ReleaseLock(ctx context.Context, exec ssh.Executor, app string) {
	lockPath := fmt.Sprintf("%s/%s/.lock", deploymentsDir, app)
	exec.Run(ctx, fmt.Sprintf("rm -rf %s", lockPath))
}

// ReleaseLockDetached releases the deploy lock using a fresh, short-lived
// context instead of the caller's — for use as `defer state.
// ReleaseLockDetached(...)` cleanup at the end of a deploy. If the caller's
// context was already cancelled (e.g. the operator hit Ctrl+C mid-deploy),
// reusing it here would abort the unlock SSH command before it ever runs:
// RemoteExecutor.RunStream races an already-closed ctx.Done() against
// session.Run() finishing, and the closed channel wins almost every time
// since Run() needs a real network round-trip first. That left the lock
// stuck, requiring a human to notice and run `teploy unlock`. A detached
// context lets the unlock actually reach the server even when the deploy
// itself was interrupted.
func ReleaseLockDetached(exec ssh.Executor, app string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ReleaseLock(ctx, exec, app)
}

// AppendLog appends a deploy log entry to /deployments/teploy.log.
func AppendLog(ctx context.Context, exec ssh.Executor, entry LogEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling log entry: %w", err)
	}
	line = append(line, '\n')

	// Append in a single command rather than staging through a fixed
	// /tmp/teploy_log_entry that concurrent deploys (different apps) would
	// clobber. base64 keeps the JSON shell-safe; a single >> append below
	// PIPE_BUF is atomic on POSIX, so interleaved appends don't corrupt lines.
	encoded := base64.StdEncoding.EncodeToString(line)
	cmd := fmt.Sprintf("printf %%s %s | base64 -d >> %s/teploy.log", ssh.ShellQuote(encoded), deploymentsDir)
	if _, err := exec.Run(ctx, cmd); err != nil {
		return fmt.Errorf("appending log entry: %w", err)
	}
	return nil
}

// EnsureAppDir creates /deployments/<app>/ if it doesn't exist.
func EnsureAppDir(ctx context.Context, exec ssh.Executor, app string) error {
	_, err := exec.Run(ctx, fmt.Sprintf("mkdir -p %s/%s", deploymentsDir, app))
	return err
}
