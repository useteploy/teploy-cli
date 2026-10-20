// Package gitsource implements the D4 git code source for Teploy-home
// apps: per-app credentials stored age-encrypted outside /deployments, a
// server-side bare mirror, and the one admitter (`trigger poll`) that
// decides which branch head deploys. Everything runs through the
// server-side python helper over the app's executor, so private keys and
// tokens never leave the server.
package gitsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// ErrAbsent reports a missing credential, ref or commit (helper exit 3).
var ErrAbsent = errors.New("not found")

// ErrRefused is a refusal with an operator remedy (host-key pin mismatch).
type ErrRefused struct{ Reason string }

func (e *ErrRefused) Error() string { return e.Reason }

// KeyInfo is the public half of a generated deploy key or stored token.
type KeyInfo struct {
	Type        string `json:"type"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	Username    string `json:"username,omitempty"`
	Created     bool   `json:"created"`
}

// HostKeyEntry is one pinned host key line with its fingerprint.
type HostKeyEntry struct {
	Line        string `json:"line"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
}

// ShowState is `config source show`: public facts only, never a secret.
type ShowState struct {
	Type        string         `json:"type"`
	CreatedAt   int64          `json:"created_at,omitempty"`
	PublicKey   string         `json:"public_key,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Username    string         `json:"username,omitempty"`
	KnownHosts  []HostKeyEntry `json:"known_hosts,omitempty"`
}

// GenerateKey creates (or returns) the app's ed25519 deploy key,
// age-encrypted outside /deployments, and prints only its public half.
func GenerateKey(ctx context.Context, exec ssh.Executor, app string, rotate bool) (KeyInfo, error) {
	arg := "keep"
	if rotate {
		arg = "rotate"
	}
	var out KeyInfo
	if err := runJSON(ctx, exec, &out, nil, "keygen", app, arg); err != nil {
		return KeyInfo{}, err
	}
	return out, nil
}

// SetToken stores an HTTPS token read from stdin (never in argv).
func SetToken(ctx context.Context, exec ssh.Executor, app, username string, token []byte) (KeyInfo, error) {
	var out KeyInfo
	if err := runJSON(ctx, exec, &out, token, "token", app, username); err != nil {
		return KeyInfo{}, err
	}
	return out, nil
}

// PinHostKey scans the repository host's SSH keys, verifies an expected
// fingerprint when given, and stores them as the app's known_hosts.
func PinHostKey(ctx context.Context, exec ssh.Executor, app, repository, expectedFingerprint string) ([]HostKeyEntry, error) {
	host, port, err := RepositorySSHHost(repository)
	if err != nil {
		return nil, err
	}
	var out struct {
		Entries []HostKeyEntry `json:"entries"`
		Matched bool           `json:"matched"`
	}
	res := sshRun(ctx, exec, nil, "hostkey", app, host, strconv.Itoa(port), expectedFingerprint)
	if res.Err != nil {
		return nil, fmt.Errorf("scanning %s:%d: %w", host, port, res.Err)
	}
	switch res.ExitCode {
	case 0, 5:
		if err := jsonUnmarshal(res.Stdout, &out); err != nil {
			return nil, fmt.Errorf("scanning %s:%d: unreadable reply", host, port)
		}
	case 3:
		return nil, fmt.Errorf("ssh-keyscan found no keys for %s:%d — is the forge's SSH endpoint up?", host, port)
	default:
		return nil, helperError("scanning host keys", res)
	}
	if expectedFingerprint != "" && !out.Matched {
		return out.Entries, &ErrRefused{Reason: fmt.Sprintf("host %s presented %s, expected %s — if the host really changed, verify the new fingerprint out of band and re-run without --expected-fingerprint", host, fingerprintOf(out.Entries), expectedFingerprint)}
	}
	return out.Entries, nil
}

func fingerprintOf(entries []HostKeyEntry) string {
	for _, e := range entries {
		if e.Fingerprint != "" {
			return e.Fingerprint
		}
	}
	return "no keys"
}

// Show returns the app's public source state (no secrets).
func Show(ctx context.Context, exec ssh.Executor, app string) (ShowState, error) {
	var out ShowState
	err := runJSON(ctx, exec, &out, nil, "show", app)
	if errors.Is(err, ErrAbsent) {
		return ShowState{Type: "none"}, ErrAbsent
	}
	return out, err
}

// Remove deletes the app's credential directory (token, key, host keys).
func Remove(ctx context.Context, exec ssh.Executor, app string) error {
	return runOK(ctx, exec, nil, "remove", app)
}

// LsRemote resolves the remote ref's head with the app credential.
// ErrAbsent means the ref is not there.
func LsRemote(ctx context.Context, exec ssh.Executor, app, repository, branch string) (string, error) {
	out, err := run(ctx, exec, nil, "lsremote", app, repository, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// FetchMirror fetches the branch into the per-app mirror and verifies sha
// arrived. Returns the sha actually fetched.
func FetchMirror(ctx context.Context, exec ssh.Executor, app, repository, branch, sha string) (string, error) {
	out, err := run(ctx, exec, nil, "fetch", app, repository, branch, sha)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IsAncestor reports whether a is an ancestor of b in the mirror.
// ErrAbsent means not an ancestor; exit 4 (unverifiable, object missing)
// is reported as a distinct message wrapped in ErrAbsent-like semantics:
// the caller treats it as a force-push refusal with a reset remedy.
func IsAncestor(ctx context.Context, exec ssh.Executor, app, branch, a, b string) (bool, error) {
	res := sshRun(ctx, exec, nil, "ancestor", app, branch, a, b)
	if res.Err != nil {
		return false, fmt.Errorf("checking ancestry: %w", res.Err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 3:
		return false, nil
	case 4:
		return false, &ErrRefused{Reason: fmt.Sprintf("commit %s is no longer in %s's mirror (history was rewritten and pruned) — run `teploy trigger reset --app %s --ref refs/heads/%s` to restart from the current head", short(a), app, app, branch)}
	}
	return false, helperError("checking ancestry", res)
}

// DiffNames lists files changed between two commits in the mirror.
func DiffNames(ctx context.Context, exec ssh.Executor, app, branch, from, to string) ([]string, error) {
	out, err := run(ctx, exec, nil, "diff", app, branch, from, to)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil, nil
	}
	return fields, nil
}

// ExportArchive extracts the branch's tree into dest and reports the
// build mode plus whether the repository declares submodules.
func ExportArchive(ctx context.Context, exec ssh.Executor, app, dest, branch, contextDir, dockerfile string) (mode string, hasGitmodules bool, err error) {
	var out struct {
		Mode          string `json:"mode"`
		HasGitmodules bool   `json:"has_gitmodules"`
	}
	if err := runJSON(ctx, exec, &out, nil, "export", app, dest, branch, contextDir, dockerfile); err != nil {
		return "", false, err
	}
	return out.Mode, out.HasGitmodules, nil
}

// SealBuildSecret decrypts one age-store secret into dest (0600) on the
// server, for `docker build --secret`. src-dest never carries the value.
func SealBuildSecret(ctx context.Context, exec ssh.Executor, app, key, dest string) error {
	return runOK(ctx, exec, nil, "seal-secret", app, key, dest)
}

// ---------------------------------------------------------------------------
// Admission state

// Commit outcomes (admissions ledger).
const (
	RecordDeployed = "deployed"
	RecordFailed   = "failed"
	RecordSkipped  = "skipped"
	RecordRefused  = "refused"
)

// MaxDeployAttempts is one attempt plus the two retries a failed commit
// gets before only a new commit, `trigger retry` or a redeploy restarts it.
const MaxDeployAttempts = 3

// PollState is the admitter's durable record under
// /deployments/.teploy/apps/<app>/admissions/state.json.
type PollState struct {
	SchemaVersion int                    `json:"schema_version"`
	Repository    string                 `json:"repository,omitempty"`
	Refs          map[string]*RefState   `json:"refs,omitempty"`
	Commits       map[string]*CommitInfo `json:"commits,omitempty"`
	LastPoll      int64                  `json:"last_poll,omitempty"`
}

// RefState is the watermark of one (repository, ref).
type RefState struct {
	Watermark   string `json:"watermark,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`
	RefusedHead string `json:"refused_head,omitempty"`
	RefusedAt   int64  `json:"refused_at,omitempty"`
}

// CommitInfo is the bounded per-commit ledger entry (dedup + retries).
type CommitInfo struct {
	Status      string `json:"status"`
	Attempts    int    `json:"attempts,omitempty"`
	LastAttempt int64  `json:"last_attempt,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	Release     string `json:"release,omitempty"`
}

// LoadState reads the admissions state ({} when absent).
func LoadState(ctx context.Context, exec ssh.Executor, app string) (*PollState, error) {
	var st PollState
	res := sshRun(ctx, exec, nil, "state-get", app)
	if res.Err != nil {
		return nil, fmt.Errorf("reading %s's admissions: %w", app, res.Err)
	}
	if res.ExitCode != 0 {
		return nil, helperError("reading admissions", res)
	}
	if err := jsonUnmarshal(res.Stdout, &st); err != nil {
		return nil, fmt.Errorf("reading %s's admissions: %w", app, err)
	}
	return &st, nil
}

// SaveState writes the admissions state atomically (flock'd) and keeps
// the watermark commits reachable in the mirror.
func SaveState(ctx context.Context, exec ssh.Executor, app string, st *PollState) error {
	prune(st)
	data, err := jsonMarshal(st)
	if err != nil {
		return err
	}
	return runOK(ctx, exec, data, "state-put", app)
}

// prune bounds the commit ledger to the newest maxLedger entries.
func prune(st *PollState) {
	const maxLedger = 100
	if len(st.Commits) <= maxLedger {
		return
	}
	// Drop the least-recently-attempted entries; watermarks never depend
	// on the ledger (ancestry uses the mirror refs).
	type kv struct {
		k string
		v *CommitInfo
	}
	var entries []kv
	for k, v := range st.Commits {
		entries = append(entries, kv{k, v})
	}
	for _, e := range entries {
		if len(st.Commits) <= maxLedger {
			break
		}
		if !isWatermark(st, e.k) {
			delete(st.Commits, e.k)
		}
	}
}

func isWatermark(st *PollState, ledgerKey string) bool {
	for _, r := range st.Refs {
		if r != nil && ledgerKey == r.Watermark {
			return true
		}
	}
	return false
}

// Ref returns the ref's state, creating it when missing.
func (s *PollState) Ref(ref string) *RefState {
	if s.Refs == nil {
		s.Refs = map[string]*RefState{}
	}
	r, ok := s.Refs[ref]
	if !ok || r == nil {
		r = &RefState{}
		s.Refs[ref] = r
	}
	return r
}

// Commit returns the commit's ledger entry, creating it when missing.
func (s *PollState) Commit(ref, sha string) *CommitInfo {
	if s.Commits == nil {
		s.Commits = map[string]*CommitInfo{}
	}
	k := ledgerKey(ref, sha)
	c, ok := s.Commits[k]
	if !ok || c == nil {
		c = &CommitInfo{}
		s.Commits[k] = c
	}
	return c
}

func ledgerKey(ref, sha string) string { return ref + "/" + short(sha) }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ---------------------------------------------------------------------------
// Repository URL helpers

// RepositorySSHHost extracts host and port for host-key pinning from an
// ssh:// or scp-style repository. https/file repositories need no pin.
func RepositorySSHHost(repository string) (string, int, error) {
	lower := strings.ToLower(repository)
	if strings.HasPrefix(lower, "file://") || strings.HasPrefix(lower, "https://") {
		return "", 0, fmt.Errorf("%s is not an SSH repository — no host key to pin", repository)
	}
	if strings.HasPrefix(lower, "ssh://") {
		u, err := url.Parse(repository)
		if err != nil || u.Host == "" {
			return "", 0, fmt.Errorf("%q is not a valid ssh:// repository", repository)
		}
		host := u.Hostname()
		port := 22
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
		return host, port, nil
	}
	// scp-like user@host:path
	slash := strings.Index(repository, "/")
	colon := strings.Index(repository, ":")
	if colon <= 0 || (slash >= 0 && slash < colon) {
		return "", 0, fmt.Errorf("%q is not an SSH repository — no host key to pin", repository)
	}
	hostpart := repository[:colon]
	if at := strings.LastIndex(hostpart, "@"); at >= 0 {
		hostpart = hostpart[at+1:]
	}
	return hostpart, 22, nil
}

// PathRelevant reports whether any changed file matches the filter:
// empty filter deploys on any change. Entries are directories (trailing
// slash or bare path prefix) or path.Match globs; the build context
// directory always matches its own files.
func PathRelevant(changed []string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, c := range changed {
		for _, p := range filter {
			if p == "" {
				continue
			}
			if strings.HasPrefix(p, "/") {
				p = strings.TrimPrefix(p, "/")
			}
			if c == p || strings.HasPrefix(c, p+"/") || strings.HasPrefix(c, strings.TrimSuffix(p, "/")+"/") {
				return true
			}
			if m, err := path.Match(p, c); err == nil && m {
				return true
			}
			// A bare glob ("*.go") also matches nested files' basenames.
			if !strings.Contains(p, "/") {
				if m, err := path.Match(p, path.Base(c)); err == nil && m {
					return true
				}
			}
			// A bare directory entry ("web") also covers "web/**".
			if strings.HasSuffix(p, "/**") && strings.HasPrefix(c, strings.TrimSuffix(p, "**")) {
				return true
			}
		}
	}
	return false
}

// DefaultPaths is the path filter a config implies: git.paths when set,
// else the context directory, else everything.
func DefaultPaths(appCfg *config.AppConfig) []string {
	if appCfg == nil || appCfg.Git == nil {
		return nil
	}
	if len(appCfg.Git.Paths) > 0 {
		return appCfg.Git.Paths
	}
	if context := contextDirOf(appCfg); context != "" {
		return []string{context}
	}
	return nil
}

// ContextDir returns the config's build context relative to the repo
// root ("" means the root itself).
func contextDirOf(c *config.AppConfig) string {
	if c == nil || c.Context == "" || c.Context == "." {
		return ""
	}
	return c.Context
}

// PollInterval parses the configured poll interval; "" defaults to 5m.
func PollInterval(g *config.GitSource) (time.Duration, bool) {
	if g == nil || g.Poll == "" {
		return 5 * time.Minute, true
	}
	if g.Poll == "off" {
		return 0, false
	}
	d, err := config.ParseGitPoll(g.Poll)
	if err != nil {
		return 5 * time.Minute, true
	}
	return d, true
}

// ---------------------------------------------------------------------------
// helper plumbing

func sshRun(ctx context.Context, exec ssh.Executor, stdin []byte, args ...string) ssh.Result {
	cmd := "python3 -c " + ssh.ShellQuote(helper)
	for _, a := range args {
		cmd += " " + ssh.ShellQuote(a)
	}
	if stdin != nil {
		return ssh.RunInputDetailed(ctx, exec, cmd, bytes.NewReader(stdin))
	}
	return ssh.RunDetailed(ctx, exec, cmd)
}

func helperError(what string, res ssh.Result) error {
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	return fmt.Errorf("%s: exit %d: %s", what, res.ExitCode, msg)
}

func run(ctx context.Context, exec ssh.Executor, stdin []byte, args ...string) (string, error) {
	res := sshRun(ctx, exec, stdin, args...)
	if res.Err != nil {
		return "", res.Err
	}
	if res.ExitCode == 3 {
		return "", ErrAbsent
	}
	if res.ExitCode != 0 {
		return "", helperError(strings.Join(args, " "), res)
	}
	return string(res.Stdout), nil
}

func runOK(ctx context.Context, exec ssh.Executor, stdin []byte, args ...string) error {
	_, err := run(ctx, exec, stdin, args...)
	return err
}

func runJSON(ctx context.Context, exec ssh.Executor, out any, stdin []byte, args ...string) error {
	res := sshRun(ctx, exec, stdin, args...)
	if res.Err != nil {
		return res.Err
	}
	switch res.ExitCode {
	case 0:
		if err := jsonUnmarshal(res.Stdout, out); err != nil {
			return fmt.Errorf("%s: unreadable helper reply", args[0])
		}
		return nil
	case 3:
		return ErrAbsent
	case 5:
		return &ErrRefused{Reason: strings.TrimSpace(string(res.Stderr))}
	}
	return helperError(args[0], res)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(bytes.TrimSpace(data), v)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
