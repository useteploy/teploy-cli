# First success: install to rollback

The shortest honest path from nothing to a deployed app you have verified
and can roll back. Every command below was executed against a scratch
Docker host over SSH (a colima VM); where a step needs an environment this
walk-through cannot assume (a public domain, a cloud VPS), it says so.

## 0. Install the CLI

```bash
brew install useteploy/tap/teploy        # macOS/Linux
# or download a release binary / Scoop on Windows / go install — see README
teploy version
```

Verified here with a worktree build (`teploy version` prints the embedded
version; a source build prints `dev`).

## 1. Have a server

Any Linux server you can SSH into with key auth, with Docker installable.
`teploy setup <host>` provisions it: Docker, Caddy, firewall, and by
default host audit hardening (auditd + sudo session recording; skip with
`--no-harden`).

```bash
teploy setup 203.0.113.10
```

Gated here: `setup` against a fresh public VPS was not re-run for this
walk-through (no spare public host); the command's behavior is covered by
its own tests, and the fixture below used an already-provisioned Docker
host. If your host already runs Docker, a deploy works without `setup`
for `ingress: host` — Caddy is only required for domain routing.

Register the server so commands can name it (writes
`~/.teploy/servers.yml`):

```bash
teploy server add box1 203.0.113.10 --user root
```

Non-standard SSH port? Use `host:port` — `teploy server add box1
203.0.113.10:2222 --user root` and `server: box1` in `teploy.yml`.

## 2. Create the app

A project directory with a `Dockerfile` listening on one port:

```dockerfile
FROM nginx:1.27-alpine
COPY index.html /usr/share/nginx/html/index.html
```

Either run `teploy init` (interactive: app name, domain or raw port,
server) or write the three-line config yourself:

```yaml
# teploy.yml
app: demo
domain: demo.example.com   # or: ingress: host + port: 8080 for a raw port
server: box1
```

Check yourself before deploying:

```bash
teploy validate   # config grammar + server reference
teploy doctor     # read-only: git, config, SSH, Docker, disk, registry,
                  # Caddy, version compatibility, repair debt
```

`doctor` never mutates server state and exits 1 (never 2) when any check
fails; every failing check prints a `fix:` line. A `warn` (e.g. missing
local git) does not fail the run.

## 3. Deploy

```bash
teploy deploy
```

What you should see (abridged, from the verified run):

```
Built image: demo-build-<hash>
Deploying demo (version <hash>)...
Publishing on 0.0.0.0:80 (host ingress)...
Starting container demo-web-<hash> (port 80)...
  Readiness: auto — HTTP then TCP fallback (compat, 30s deadline)
  Health check passed
Deployed demo version <hash> in 1.237s
Receipt: image sha256:..., revision <full-hash>, config manifest ...
```

The deploy output names the target, the version, the readiness mode and
deadline, and ends with a receipt (image digest, revision, config
manifest) — the identity of exactly what landed.

Failure modes at this step, with the product's remedy:

| Symptom | Meaning | Remedy |
|---|---|---|
| `could not determine version from git ... (use --version flag)` | Project is not a git repository | `git init && git commit`, or `teploy deploy --version v1` |
| `rsync failed: exit status 127` | Server lacks `rsync` | Install it (`apt-get install rsync`); `teploy setup` includes it |
| `authentication failed for root@...; try --user <name> ...` | SSH user/key mismatch | `--user`, `--key`, or `TEPLOY_USER`/`TEPLOY_SSH_KEY` |
| `'domain' is required` | No domain and no `ingress: host` | Set `domain:`, or `ingress: host` + `port:` |
| Health check never passes | Readiness gate refuses to switch traffic | The deploy fails and the previous version keeps serving. Fix the app's health endpoint or set `health: {mode: tcp}` — see README "Config" |
| Doctor fails on `ssh`/`docker`/`disk` | Target not ready | Follow the per-check `fix:` line; disk fails below 2 GiB free |

## 4. Verify

```bash
teploy status    # containers + version
teploy health    # run the readiness probe on the live app
teploy logs --tail 20   # stream logs (Ctrl-C to exit — it follows)
teploy log        # deploy history: deploys, rollbacks, failures
```

And from any machine that can reach the app: `curl http(s)://<domain or
host:port>/`.

## 5. Change something, deploy again, roll back

Commit a change and deploy; `teploy status` now shows current and previous
hashes. To revert:

```bash
teploy rollback
```

On Caddy ingress this switches traffic back to the still-known predecessor
version and health-gates it. On `ingress: host` the prior container was
removed at deploy, so rollback redeploys the previous version and
health-gates it (seconds, not an instant switch) — verified on the
fixture: `Rolled back demo to version <hash> in 369ms`.

## 6. Next steps

- Secrets and env: `teploy env set`, `teploy secret set` (encrypted at
  rest), SOPS/age `env_files:`.
- Backups: `teploy backup create --bucket ...`, verified restores via
  `teploy accessory verify-backup`.
- Whole-app disaster recovery bundles: [failure-and-recovery.md](failure-and-recovery.md).
- CI: [ci-deploy.md](ci-deploy.md).
- Fleet and surviving server loss: [resilience.md](resilience.md).
