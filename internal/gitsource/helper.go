package gitsource

// helper is the single server-side actor for git sources (D4). It runs
// over the app's executor (SSH from a client, or locally for the server
// timer) exactly like the confighome and events helpers, so no git
// credential, plaintext key or token ever crosses to a client.
//
// Layout on the server:
//
//	credentials: /etc/teploy/apps/<app>          (root) or
//	             ~/.config/teploy/apps/<app>     (non-root)
//	             key.age, key.pub, token.age, known_hosts, source.json
//	mirror:       /deployments/.teploy/apps/<app>/mirror.git   (code only)
//	admissions:   /deployments/.teploy/apps/<app>/admissions/
//	             state.json (+ .state.lock, flock'd; watermark refs live
//	             in the mirror under refs/teploy/wm/<branch>)
//
// Secrets never appear in argv: tokens arrive on stdin, the deploy key is
// decrypted into a 0600 file under a private tmpfs directory for exactly
// one git invocation, and GIT_SSH_COMMAND / GIT_ASKPASS point at files.
// The age identity is tried at /etc/teploy/age-key then
// /deployments/.age-key (W2 moves it; both are accepted), and a missing
// identity is created at the historical /deployments/.age-key path — the
// same place the secret store creates it.
//
// The script deliberately avoids backslash escapes so it can live in a Go
// raw string: newlines are chr(10), stderr goes through print().
//
// Exit codes: 0 ok; 3 "absent" (credential / ref / commit missing);
// 4 unverifiable ancestry; 5 host-key pin mismatch; 1 other errors.
const helper = `
import fcntl, json, os, pwd, shutil, subprocess, sys, tempfile, time

CRED_ROOT = os.environ.get("TEPLOY_GIT_CRED_ROOT_TEST")
CONFIG_ROOT = os.environ.get("TEPLOY_CONFIG_ROOT_TEST") or "/deployments/.teploy"
DEPLOYMENTS = os.environ.get("TEPLOY_DEPLOYMENTS_TEST") or "/deployments"
IDENTITIES = [p for p in os.environ.get("TEPLOY_AGE_IDENTITIES", "/etc/teploy/age-key:/deployments/.age-key").split(":") if p]
NL = chr(10)

def fail(msg, code=1):
    print(msg, file=sys.stderr)
    sys.exit(code)

def run(argv, data=None, env=None, check=True):
    e = dict(os.environ)
    e["GIT_TERMINAL_PROMPT"] = "0"
    e.pop("GIT_DIR", None)
    e.pop("SSH_AUTH_SOCK", None)
    if env:
        e.update(env)
    r = subprocess.run(argv, input=data, capture_output=True, env=e)
    if check and r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    return r

def cred_dir(app):
    if "/" in app or app.startswith("."):
        fail("invalid app name")
    if CRED_ROOT:
        base = CRED_ROOT
    else:
        uid = os.getuid()
        if uid == 0:
            base = "/etc/teploy/apps"
        else:
            home = os.environ.get("HOME") or pwd.getpwuid(uid).pw_dir
            base = os.path.join(home, ".config/teploy/apps")
    return os.path.join(base, app)

def meta_path(app):
    return os.path.join(cred_dir(app), "source.json")

def read_meta(app):
    try:
        with open(meta_path(app)) as f:
            return json.load(f)
    except FileNotFoundError:
        return None

def write_meta(app, meta):
    d = cred_dir(app)
    os.makedirs(d, mode=0o700, exist_ok=True)
    meta["updated_at"] = int(time.time())
    tmp = meta_path(app) + ".tmp"
    with open(tmp, "w") as f:
        json.dump(meta, f)
    os.chmod(tmp, 0o600)
    os.rename(tmp, meta_path(app))

def ensure_identity():
    have = [p for p in IDENTITIES if os.path.exists(p)]
    if have:
        return have
    target = "/deployments/.age-key"
    os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
    r = subprocess.run(["age-keygen", "-o", target], capture_output=True)
    if r.returncode != 0:
        fail("no age identity and age-keygen failed: " + r.stderr.decode(errors="replace"))
    os.chmod(target, 0o600)
    return [target]

def recipient():
    for p in ensure_identity():
        r = subprocess.run(["grep", "public key:", p], capture_output=True)
        out = r.stdout.decode().strip()
        if out:
            return out.split()[-1]
    fail("age identity carries no public key")

def encrypt_to(data_bytes, dest):
    os.makedirs(os.path.dirname(dest), mode=0o700, exist_ok=True)
    r = run(["age", "-r", recipient()], data=data_bytes, check=False)
    if r.returncode != 0:
        fail("encrypting: " + r.stderr.decode(errors="replace"))
    tmp = dest + ".tmp"
    with open(tmp, "wb") as f:
        f.write(r.stdout)
    os.chmod(tmp, 0o600)
    os.rename(tmp, dest)

def decrypt(path):
    args = ["age", "-d"]
    for p in ensure_identity():
        args += ["-i", p]
    r = run(args + [path], check=False)
    if r.returncode != 0:
        fail("decrypting " + os.path.basename(path) + ": " + r.stderr.decode(errors="replace"))
    return r.stdout

def fingerprint(pub):
    r = subprocess.run(["ssh-keygen", "-lf", "-"], input=pub.encode(), capture_output=True)
    return r.stdout.decode().strip().splitlines()[0] if r.stdout else ""

def mirror_dir(app):
    return os.path.join(CONFIG_ROOT, "apps", app, "mirror.git")

def ensure_mirror(app):
    d = mirror_dir(app)
    if os.path.isdir(d):
        return d
    os.makedirs(os.path.dirname(d), mode=0o700, exist_ok=True)
    tmp = d + ".init-%d" % os.getpid()
    r = subprocess.run(["git", "init", "-q", "--bare", tmp], capture_output=True)
    if r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    for k, v in [("gc.auto", "0"), ("gc.autoDetach", "false"), ("fetch.prune", "false")]:
        run(["git", "--git-dir=" + tmp, "config", k, v])
    os.chmod(tmp, 0o700)
    os.rename(tmp, d)
    return d

def git_env(app):
    meta = read_meta(app) or {"type": "none"}
    t = meta.get("type", "none")
    d = cred_dir(app)
    tmpdir = "/dev/shm" if os.path.isdir("/dev/shm") else None
    tmp = tempfile.mkdtemp(prefix="teploy-git-", dir=tmpdir)
    os.chmod(tmp, 0o700)
    env = {}
    if t == "ssh":
        kh = os.path.join(d, "known_hosts")
        if not os.path.exists(kh):
            shutil.rmtree(tmp, ignore_errors=True)
            fail("no pinned host key for %s - run: teploy config source host-key --app %s --host <server>" % (app, app), 3)
        key = os.path.join(tmp, "key")
        with open(key, "wb") as f:
            f.write(decrypt(os.path.join(d, "key.age")))
        os.chmod(key, 0o600)
        env["GIT_SSH_COMMAND"] = " ".join([
            "ssh", "-i", key, "-o", "IdentitiesOnly=yes", "-o", "UserKnownHostsFile=" + kh,
            "-o", "StrictHostKeyChecking=yes", "-o", "BatchMode=yes",
            "-o", "PasswordAuthentication=no", "-o", "ControlPath=none"])
    elif t == "token":
        env["TEPLOY_GIT_TOKEN"] = decrypt(os.path.join(d, "token.age")).decode(errors="replace").strip(NL)
        env["TEPLOY_GIT_USERNAME"] = meta.get("username") or "x-access-token"
        ap = os.path.join(tmp, "askpass")
        script = NL.join([
            "#!/bin/sh",
            'case "$1" in',
            '  *Username*|*username*) echo "$TEPLOY_GIT_USERNAME";;',
            '  *) echo "$TEPLOY_GIT_TOKEN";;',
            "esac"])
        with open(ap, "w") as f:
            f.write(script)
        os.chmod(ap, 0o700)
        env["GIT_ASKPASS"] = ap
        env["GIT_CONFIG_COUNT"] = "1"
        env["GIT_CONFIG_KEY_0"] = "credential.helper"
        env["GIT_CONFIG_VALUE_0"] = ""
    return env, tmp

def with_credential(app, argv, data=None):
    env, tmp = git_env(app)
    try:
        return run(["git", "-c", "protocol.ext.allow=never", "-c", "protocol.file.allow=user"] + argv,
                   data=data, env=env, check=False)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)

def state_path(app):
    return os.path.join(CONFIG_ROOT, "apps", app, "admissions", "state.json")

def load_state(app):
    try:
        with open(state_path(app)) as f:
            return json.load(f)
    except FileNotFoundError:
        return {}

def save_state(app, state):
    d = os.path.dirname(state_path(app))
    os.makedirs(d, mode=0o700, exist_ok=True)
    lk = os.open(os.path.join(d, ".state.lock"), os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(lk, fcntl.LOCK_EX)
        tmp = state_path(app) + ".tmp"
        with open(tmp, "w") as f:
            json.dump(state, f)
        os.chmod(tmp, 0o600)
        os.rename(tmp, state_path(app))
        m = ensure_mirror(app)
        for branch, sha in (state.get("watermarks") or {}).items():
            if sha:
                run(["git", "--git-dir=" + m, "update-ref", "refs/teploy/wm/" + branch, sha], check=False)
    finally:
        fcntl.flock(lk, fcntl.LOCK_UN)
        os.close(lk)

def out_json(v):
    print(json.dumps(v))

def host_fingerprints(lines):
    entries = []
    for l in lines:
        parts = l.split()
        if len(parts) < 3:
            continue
        kf = subprocess.run(["ssh-keygen", "-lf", "-"], input=(parts[1] + " " + parts[2] + NL).encode(), capture_output=True)
        entries.append({"line": l, "type": parts[1], "fingerprint": kf.stdout.decode().strip()})
    return entries

cmd, app = sys.argv[1], sys.argv[2]

if cmd == "keygen":
    rotate = len(sys.argv) > 3 and sys.argv[3] == "rotate"
    d = cred_dir(app)
    created = not os.path.exists(os.path.join(d, "key.age"))
    if created or rotate:
        os.makedirs(d, mode=0o700, exist_ok=True)
        tmp = tempfile.mkdtemp(prefix="teploy-keygen-")
        try:
            r = subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "teploy-deploy-" + app, "-f", os.path.join(tmp, "k")], capture_output=True)
            if r.returncode != 0:
                fail(r.stderr.decode(errors="replace"))
            with open(os.path.join(tmp, "k"), "rb") as f:
                encrypt_to(f.read(), os.path.join(d, "key.age"))
            shutil.copy(os.path.join(tmp, "k.pub"), os.path.join(d, "key.pub"))
            os.chmod(os.path.join(d, "key.pub"), 0o644)
        finally:
            shutil.rmtree(tmp, ignore_errors=True)
        write_meta(app, {"type": "ssh", "created_at": int(time.time())})
    elif read_meta(app) is None:
        write_meta(app, {"type": "ssh", "created_at": int(time.time())})
    pub = open(os.path.join(cred_dir(app), "key.pub")).read()
    out_json({"type": "ssh", "public_key": pub.strip(), "fingerprint": fingerprint(pub), "created": created or rotate})

elif cmd == "token":
    username = sys.argv[3] if len(sys.argv) > 3 else ""
    token = sys.stdin.buffer.read()
    if not token.strip():
        fail("empty token on stdin")
    encrypt_to(token, os.path.join(cred_dir(app), "token.age"))
    prev = read_meta(app) or {}
    meta = {"type": "token", "username": username or "x-access-token", "created_at": prev.get("created_at") or int(time.time())}
    d = cred_dir(app)
    for stale in ("key.age", "key.pub"):
        try:
            os.unlink(os.path.join(d, stale))
        except FileNotFoundError:
            pass
    write_meta(app, meta)
    out_json({"type": "token", "username": meta["username"], "created": True})

elif cmd == "hostkey":
    host, port = sys.argv[3], sys.argv[4]
    expect = sys.argv[5] if len(sys.argv) > 5 else ""
    r = subprocess.run(["ssh-keyscan", "-p", port, "-t", "ed25519,ecdsa,rsa", host], capture_output=True)
    lines = [l for l in r.stdout.decode(errors="replace").splitlines() if l.strip() and not l.startswith("#")]
    if not lines:
        fail("ssh-keyscan found no keys for %s:%s" % (host, port), 3)
    entries = host_fingerprints(lines)
    if expect:
        fps = [e["fingerprint"].split()[1] for e in entries if len(e["fingerprint"].split()) > 1]
        if expect not in fps:
            out_json({"entries": entries, "matched": False})
            sys.exit(5)
    d = cred_dir(app)
    os.makedirs(d, mode=0o700, exist_ok=True)
    tmp = os.path.join(d, "known_hosts.tmp")
    with open(tmp, "w") as f:
        f.write(NL.join(e["line"] for e in entries) + NL)
    os.chmod(tmp, 0o600)
    os.rename(tmp, os.path.join(d, "known_hosts"))
    out_json({"entries": entries, "matched": True})

elif cmd == "show":
    meta = read_meta(app)
    if not meta:
        out_json({"type": "none"})
        sys.exit(3)
    out = {"type": meta.get("type", "none"), "created_at": meta.get("created_at")}
    d = cred_dir(app)
    if meta.get("type") == "ssh" and os.path.exists(os.path.join(d, "key.pub")):
        pub = open(os.path.join(d, "key.pub")).read()
        out["public_key"] = pub.strip()
        out["fingerprint"] = fingerprint(pub)
    if meta.get("type") == "token":
        out["username"] = meta.get("username")
    if os.path.exists(os.path.join(d, "known_hosts")):
        out["known_hosts"] = host_fingerprints([l.strip() for l in open(os.path.join(d, "known_hosts")) if l.strip()])
    out_json(out)

elif cmd == "remove":
    d = cred_dir(app)
    removed = os.path.isdir(d)
    if removed:
        shutil.rmtree(d, ignore_errors=True)
    print("removed" if removed else "absent")

elif cmd == "lsremote":
    url, ref = sys.argv[3], sys.argv[4]
    r = with_credential(app, ["ls-remote", url, ref])
    if r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    line = r.stdout.decode().strip().splitlines()[0] if r.stdout.strip() else ""
    if not line:
        sys.exit(3)
    print(line.split()[0])

elif cmd == "fetch":
    url, branch, sha = sys.argv[3], sys.argv[4], sys.argv[5]
    m = ensure_mirror(app)
    r = with_credential(app, ["--git-dir=" + m, "fetch", "--no-tags", "--force", url, "+refs/heads/%s:refs/heads/%s" % (branch, branch)])
    if r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    v = run(["git", "--git-dir=" + m, "rev-parse", "--verify", "-q", sha + "^{commit}"], check=False)
    if v.returncode != 0:
        fail("fetched branch does not contain %s (branch moved?)" % sha, 3)
    print(v.stdout.decode().strip())

elif cmd == "ancestor":
    branch, a, b = sys.argv[3], sys.argv[4], sys.argv[5]
    m = mirror_dir(app)
    for oid in (a, b):
        if run(["git", "--git-dir=" + m, "cat-file", "-e", oid + "^{commit}"], check=False).returncode != 0:
            fail("commit %s not in the mirror" % oid, 4)
    r = run(["git", "--git-dir=" + m, "merge-base", "--is-ancestor", a, b], check=False)
    sys.exit(0 if r.returncode == 0 else 3)

elif cmd == "diff":
    branch, a, b = sys.argv[3], sys.argv[4], sys.argv[5]
    m = mirror_dir(app)
    r = run(["git", "--git-dir=" + m, "diff", "--name-only", a, b], check=False)
    if r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    sys.stdout.write(r.stdout.decode(errors="replace"))

elif cmd == "export":
    dest, branch, context, dockerfile = sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6]
    m = mirror_dir(app)
    os.makedirs(dest, mode=0o700, exist_ok=True)
    r = run(["git", "--git-dir=" + m, "archive", "--format=tar", branch], check=False)
    if r.returncode != 0:
        fail(r.stderr.decode(errors="replace"))
    t = subprocess.run(["tar", "-x", "-C", dest], input=r.stdout, capture_output=True)
    if t.returncode != 0:
        fail(t.stderr.decode(errors="replace"))
    root = dest if context in ("", ".") else os.path.join(dest, context)
    name = dockerfile or "Dockerfile"
    mode = "dockerfile" if os.path.exists(os.path.join(root, name)) else "nixpacks"
    if mode == "nixpacks" and dockerfile:
        fail("dockerfile %s not found in the repository (context %s)" % (dockerfile, context or "."), 1)
    gm = run(["git", "--git-dir=" + m, "cat-file", "-e", branch + ":.gitmodules"], check=False)
    out_json({"mode": mode, "has_gitmodules": gm.returncode == 0})

elif cmd == "state-get":
    out_json(load_state(app))

elif cmd == "state-put":
    state = json.load(sys.stdin)
    save_state(app, state)
    print("ok")

elif cmd == "seal-secret":
    key, dest = sys.argv[3], sys.argv[4]
    src = os.path.join(DEPLOYMENTS, app, "secrets", key + ".age")
    if not os.path.exists(src):
        fail("secret %s is not set (teploy secret set %s)" % (key, key), 3)
    data = decrypt(src)
    os.makedirs(os.path.dirname(dest), mode=0o700, exist_ok=True)
    tmp = dest + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.chmod(tmp, 0o600)
    os.rename(tmp, dest)

else:
    fail("unknown command %s" % cmd, 2)
`
