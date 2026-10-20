// Package confighome is the server half of an app's config home
// (server-authority spec §2.1–2.3). An app whose config lives in "Teploy"
// keeps its teploy.yml in a bare git repository on its own server:
//
//	/deployments/.teploy/apps/<app>/config.git   (branch refs/heads/teploy)
//
// The CLI is the only writer. Every write is built ON THE SERVER by one
// helper (no client git, no transport): it takes an flock, compares the
// branch head with the commit the client started from (compare-and-swap),
// hashes the new teploy.yml, makes a tree and a commit authored by the
// acting client, and moves the ref. A stale head is exit 73 with the
// current commit, so two editors can never silently overwrite each other.
// Pushes are refused by a pre-receive hook; gc never runs on its own.
//
// The repository's existence is the authority for the home: present means
// the app is edited in Teploy, absent means it is edited in its folder.
package confighome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// Home names where an app's config is edited.
const (
	HomeFolder = "folder"
	HomeTeploy = "teploy"
	HomeRepo   = "repo"
)

// MaxConfigBytes bounds one teploy.yml.
const MaxConfigBytes = 1 << 20

// ZeroCommit is the expected head of a repository that has no commit yet.
const ZeroCommit = "0000000000000000000000000000000000000000"

// ErrConflict is returned by Write when the head moved since the client
// read it. Current carries the head the client must merge against.
type ConflictError struct{ Current string }

func (e *ConflictError) Error() string {
	if e.Current == "" {
		return "config conflict: the app has no Teploy config yet"
	}
	return fmt.Sprintf("config conflict: the config changed since you read it (now at %s)", short(e.Current))
}

// ErrAbsent is returned when the app has no server config repository.
var ErrAbsent = errors.New("app has no Teploy config (its settings are edited in its folder)")

// ErrOwnerMismatch: /deployments/.teploy belongs to another non-root user.
var ErrOwnerMismatch = errors.New("teploy server data owned by another user")

// Commit is one entry of a config's history.
type Commit struct {
	OID     string    `json:"commit"`
	Author  string    `json:"author"`
	Email   string    `json:"email"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
}

// Author identifies who a write is recorded as (the acting client).
type Author struct {
	Name  string
	Email string
}

// Dir is the app's server data directory.
func Dir(app string) string { return rootDir() + "/apps/" + app }

// AppsDir is the directory holding every app's server data (the poll
// timer and doctor enumerate git-source apps from it).
func AppsDir() string { return rootDir() + "/apps" }

// RepoPath is the app's config repository.
func RepoPath(app string) string { return Dir(app) + "/config.git" }

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// Head reports whether the app has a config repository and its branch head
// ("" for a repository with no commit). It reads two files instead of
// running git: the repository is created by an atomic rename (HEAD present
// <=> repo exists) and refs are never packed (every gc runs with
// gc.packRefs=false), so the loose ref is the head.
func Head(ctx context.Context, exec ssh.Executor, app string) (exists bool, commit string, err error) {
	root := rootDir()
	_, present, err := state.ReadRemoteFile(ctx, exec, root+"/apps/"+app+"/config.git/HEAD")
	if err != nil || !present {
		return false, "", err
	}
	ref, present, err := state.ReadRemoteFile(ctx, exec, root+"/apps/"+app+"/config.git/refs/heads/teploy")
	if err != nil || !present {
		return true, "", err
	}
	return true, strings.TrimSpace(string(ref)), nil
}

// rootDir is /deployments/.teploy (overridable for tests, like the helper).
func rootDir() string {
	if r := os.Getenv("TEPLOY_CONFIG_ROOT_TEST"); r != "" {
		return r
	}
	return "/deployments/.teploy"
}

// Read returns teploy.yml at rev (empty = branch head) and the commit it
// resolved to.
func Read(ctx context.Context, exec ssh.Executor, app, rev string) ([]byte, string, error) {
	if rev == "" {
		rev = "refs/heads/teploy"
	}
	if err := validRev(rev); err != nil {
		return nil, "", err
	}
	out, err := run(ctx, exec, nil, "read", app, rev)
	if err != nil {
		return nil, "", err
	}
	commit, content, ok := bytes.Cut(out, []byte("\n"))
	if !ok {
		return nil, "", fmt.Errorf("reading config: unrecognized helper output")
	}
	return content, string(commit), nil
}

// Write commits content as the new teploy.yml if the head is still expect
// (ZeroCommit or "" for the first commit). Returns the new commit.
func Write(ctx context.Context, exec ssh.Executor, app string, content []byte, expect string, author Author, message string) (string, error) {
	if len(content) > MaxConfigBytes {
		return "", fmt.Errorf("teploy.yml is %d bytes; the limit is %d", len(content), MaxConfigBytes)
	}
	if expect == "" {
		expect = ZeroCommit
	}
	if err := validRev(expect); err != nil {
		return "", err
	}
	out, err := run(ctx, exec, content, "write", app, expect, clean(author.Name, "teploy"), clean(author.Email, "teploy@localhost"), clean(message, "config set"))
	if err != nil {
		return "", err
	}
	var r struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Commit == "" {
		return "", fmt.Errorf("writing config: unreadable helper reply")
	}
	return r.Commit, nil
}

// History lists up to n commits, newest first.
func History(ctx context.Context, exec ssh.Executor, app string, n int) ([]Commit, error) {
	if n <= 0 {
		n = 50
	}
	out, err := run(ctx, exec, nil, "history", app, strconv.Itoa(n))
	if err != nil {
		return nil, err
	}
	var commits []Commit
	if err := json.Unmarshal(out, &commits); err != nil {
		return nil, fmt.Errorf("reading config history: %w", err)
	}
	return commits, nil
}

// Init creates the empty repository (idempotent). A first Write also
// initializes; Init exists so `config home teploy` can claim the home
// before the first commit lands.
func Init(ctx context.Context, exec ssh.Executor, app string) error {
	_, err := run(ctx, exec, nil, "init", app)
	return err
}

// Archive moves the repository aside (config.git.archived-<unix>), which
// returns the app to folder home while keeping its history recoverable.
func Archive(ctx context.Context, exec ssh.Executor, app string) (string, error) {
	out, err := run(ctx, exec, nil, "archive", app)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PurgeHistory replaces the whole history with one commit holding the
// current teploy.yml (for a value committed by accident), if the head is
// still expect, and drops the old objects.
func PurgeHistory(ctx context.Context, exec ssh.Executor, app, expect string, author Author) (string, error) {
	if err := validRev(expect); err != nil {
		return "", err
	}
	out, err := run(ctx, exec, nil, "purge", app, expect, clean(author.Name, "teploy"), clean(author.Email, "teploy@localhost"))
	if err != nil {
		return "", err
	}
	var r struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Commit == "" {
		return "", fmt.Errorf("purging config history: unreadable helper reply")
	}
	return r.Commit, nil
}

func clean(s, def string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return def
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// validRev accepts a full hex object id or the branch ref.
func validRev(rev string) error {
	if rev == "refs/heads/teploy" {
		return nil
	}
	if len(rev) < 7 || len(rev) > 64 {
		return fmt.Errorf("invalid commit %q", rev)
	}
	for _, r := range rev {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("invalid commit %q", rev)
		}
	}
	return nil
}

func run(ctx context.Context, exec ssh.Executor, stdin []byte, args ...string) ([]byte, error) {
	cmd := "python3 -c " + ssh.ShellQuote(helper)
	for _, a := range args {
		cmd += " " + ssh.ShellQuote(a)
	}
	// Config writes carry their own flock (.write.lock) and are not fenced
	// by the app lock; unwrap so stdin reaches the helper (the generic
	// executor fallback has no stdin).
	if f, ok := exec.(*state.FencedExecutor); ok {
		exec = f.Executor
	}
	var res ssh.Result
	if stdin != nil {
		res = ssh.RunInputDetailed(ctx, exec, cmd, bytes.NewReader(stdin))
	} else {
		res = ssh.RunDetailed(ctx, exec, cmd)
	}
	if res.Err != nil {
		return nil, res.Err
	}
	switch res.ExitCode {
	case 0:
		return res.Stdout, nil
	case 73:
		var c struct {
			Current string `json:"current"`
		}
		_ = json.Unmarshal(bytes.TrimSpace(res.Stdout), &c)
		return nil, &ConflictError{Current: c.Current}
	case 3:
		return nil, ErrAbsent
	case 77:
		return nil, fmt.Errorf("%w: %s; connect as that user or run `teploy server adopt-owner`", ErrOwnerMismatch, strings.TrimSpace(string(res.Stderr)))
	}
	msg := strings.TrimSpace(string(res.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(res.Stdout))
	}
	return nil, fmt.Errorf("config store: exit %d: %s", res.ExitCode, msg)
}

// helper is the single writer of config.git. Exit codes: 0 ok, 3 absent,
// 73 conflict ({"current"} on stdout), 77 owner mismatch.
const helper = `
import fcntl, glob, json, os, pwd, subprocess, sys, time
ROOT = os.environ.get("TEPLOY_CONFIG_ROOT_TEST") or "/deployments/.teploy"
ZERO = "0" * 40
REF = "refs/heads/teploy"

def owner_check():
    try:
        st = os.stat(ROOT)
    except FileNotFoundError:
        os.makedirs(ROOT, mode=0o700, exist_ok=True)
        os.chmod(ROOT, 0o700)
        return
    uid = os.getuid()
    if uid != 0 and st.st_uid != uid:
        try:
            name = pwd.getpwuid(st.st_uid).pw_name
        except KeyError:
            name = str(st.st_uid)
        sys.stderr.write("this server's Teploy data is owned by %s\n" % name)
        sys.exit(77)

def paths(app):
    d = os.path.join(ROOT, "apps", app)
    return d, os.path.join(d, "config.git")

def git(repo, *args, data=None, env=None, check=True):
    e = dict(os.environ)
    e["GIT_TERMINAL_PROMPT"] = "0"
    e.pop("GIT_DIR", None)
    if env:
        e.update(env)
    r = subprocess.run(["git", "--git-dir=" + repo] + list(args), input=data, capture_output=True, env=e)
    if check and r.returncode != 0:
        sys.stderr.write(r.stderr.decode(errors="replace"))
        sys.exit(1)
    return r

def git_version():
    r = subprocess.run(["git", "--version"], capture_output=True)
    try:
        parts = r.stdout.decode().split()[2].split(".")
        return int(parts[0]), int(parts[1])
    except Exception:
        return 0, 0

def init_repo(d, repo):
    os.makedirs(os.path.join(ROOT, "apps"), mode=0o700, exist_ok=True)
    os.makedirs(d, mode=0o700, exist_ok=True)
    if os.path.isdir(repo):
        return
    tmp = repo + ".init-%d" % os.getpid()
    r = subprocess.run(["git", "init", "-q", "--bare", tmp], capture_output=True)
    if r.returncode != 0:
        sys.stderr.write(r.stderr.decode(errors="replace"))
        sys.exit(1)
    git(tmp, "symbolic-ref", "HEAD", REF)
    for k, v in [("gc.auto", "0"), ("gc.autoDetach", "false"), ("receive.denyNonFastForwards", "true"), ("core.logAllRefUpdates", "true")]:
        git(tmp, "config", k, v)
    if git_version() >= (2, 36):
        git(tmp, "config", "core.fsync", "all")
    else:
        git(tmp, "config", "core.fsyncObjectFiles", "true")
    hook = os.path.join(tmp, "hooks", "pre-receive")
    with open(hook, "w") as f:
        f.write("#!/bin/sh\necho 'this repository is written only by the teploy CLI (teploy config set/edit)' >&2\nexit 1\n")
    os.chmod(hook, 0o755)
    os.chmod(tmp, 0o700)
    os.rename(tmp, repo)

def head(repo):
    r = git(repo, "rev-parse", "--verify", "-q", REF, check=False)
    return r.stdout.decode().strip() if r.returncode == 0 else ""

def locked(d):
    lk = os.open(os.path.join(d, ".write.lock"), os.O_RDWR | os.O_CREAT, 0o600)
    fcntl.flock(lk, fcntl.LOCK_EX)
    return lk

def clear_stale_locks(repo):
    # Inside our flock no other writer exists (gc is off, pushes are refused),
    # so a leftover *.lock is from a killed write: remove it.
    for p in glob.glob(os.path.join(repo, "*.lock")) + glob.glob(os.path.join(repo, "refs", "heads", "*.lock")):
        try:
            os.unlink(p)
        except FileNotFoundError:
            pass

def ident(name, email):
    now = "%d +0000" % int(time.time())
    return {"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email, "GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email,
            "GIT_AUTHOR_DATE": now, "GIT_COMMITTER_DATE": now}

def commit_tree(repo, content, parent, name, email, message):
    blob = git(repo, "hash-object", "-w", "--stdin", data=content).stdout.decode().strip()
    tree = git(repo, "mktree", data=("100644 blob %s\tteploy.yml\n" % blob).encode()).stdout.decode().strip()
    args = ["commit-tree", tree, "-m", message]
    if parent:
        args += ["-p", parent]
    return git(repo, *args, env=ident(name, email)).stdout.decode().strip()

def conflict(current):
    print(json.dumps({"error": "conflict", "current": current}))
    sys.exit(73)

cmd, app = sys.argv[1], sys.argv[2]
d, repo = paths(app)

if cmd == "read":
    if not os.path.isdir(repo):
        sys.exit(3)
    rev = sys.argv[3]
    c = git(repo, "rev-parse", "--verify", "-q", rev + "^{commit}", check=False)
    if c.returncode != 0:
        sys.stderr.write("unknown config commit %s\n" % rev)
        sys.exit(1)
    oid = c.stdout.decode().strip()
    body = git(repo, "cat-file", "blob", oid + ":teploy.yml").stdout
    sys.stdout.buffer.write(oid.encode() + b"\n" + body)
elif cmd == "init":
    owner_check()
    init_repo(d, repo)
elif cmd == "write":
    expect, name, email, message = sys.argv[3:7]
    content = sys.stdin.buffer.read()
    owner_check()
    os.makedirs(os.path.join(ROOT, "apps"), mode=0o700, exist_ok=True)
    os.makedirs(d, mode=0o700, exist_ok=True)
    lk = locked(d)
    try:
        if not os.path.isdir(repo):
            if expect != ZERO:
                conflict("")
            init_repo(d, repo)
        clear_stale_locks(repo)
        cur = head(repo)
        if (cur or ZERO) != expect and not (cur and expect != ZERO and cur.startswith(expect)):
            conflict(cur)
        new = commit_tree(repo, content, cur, name, email, message)
        git(repo, "update-ref", "-m", message, REF, new, cur or ZERO)
        if int(time.time()) % 50 == 0:
            git(repo, "-c", "gc.packRefs=false", "gc", "--quiet", check=False)
        print(json.dumps({"commit": new}))
    finally:
        fcntl.flock(lk, fcntl.LOCK_UN)
        os.close(lk)
elif cmd == "history":
    if not os.path.isdir(repo):
        sys.exit(3)
    n = int(sys.argv[3])
    if not head(repo):
        print("[]")
        sys.exit(0)
    r = git(repo, "log", "-n", str(n), "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1e", REF)
    out = []
    for rec in r.stdout.decode(errors="replace").split("\x1e"):
        rec = rec.strip("\n")
        if not rec:
            continue
        f = rec.split("\x1f")
        out.append({"commit": f[0], "author": f[1], "email": f[2], "date": f[3], "message": f[4]})
    print(json.dumps(out))
elif cmd == "archive":
    if not os.path.isdir(repo):
        sys.exit(3)
    owner_check()
    lk = locked(d)
    try:
        dst = "%s.archived-%d" % (repo, int(time.time()))
        os.rename(repo, dst)
        print(dst)
    finally:
        fcntl.flock(lk, fcntl.LOCK_UN)
        os.close(lk)
elif cmd == "purge":
    expect, name, email = sys.argv[3:6]
    if not os.path.isdir(repo):
        sys.exit(3)
    owner_check()
    lk = locked(d)
    try:
        clear_stale_locks(repo)
        cur = head(repo)
        if not cur or not cur.startswith(expect):
            conflict(cur)
        content = git(repo, "cat-file", "blob", cur + ":teploy.yml").stdout
        new = commit_tree(repo, content, "", name, email, "history purged by teploy config purge-history")
        git(repo, "update-ref", REF, new, cur)
        git(repo, "reflog", "expire", "--expire=now", "--all")
        git(repo, "-c", "gc.packRefs=false", "gc", "--prune=now", "--quiet")
        print(json.dumps({"commit": new}))
    finally:
        fcntl.flock(lk, fcntl.LOCK_UN)
        os.close(lk)
else:
    sys.stderr.write("unknown command %s\n" % cmd)
    sys.exit(2)
`
