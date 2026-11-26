# Sandboxes (early access, self-hosted)

`teploy sandbox` runs isolated sandboxes on a server you already manage with teploy. It is early access and self-hosted: the engine is the open-source `teploy-sandbox` daemon, installed on your server as a systemd service. Sandboxes hosted on Teploy Cloud are not part of this.

```
teploy sandbox host prod                                   # install and start the engine on prod
teploy sandbox new --host prod --image alpine:3.20         # a sandbox, and its id
teploy sandbox ls --host prod                              # every sandbox on prod
teploy sandbox exec 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod -- echo hello
teploy sandbox cp app.tar.gz 01JCV5ZK3Q7X9B2D4E6F8A0B1C:app.tar.gz --host prod
teploy sandbox cp 01JCV5ZK3Q7X9B2D4E6F8A0B1C:out/report.pdf . --host prod
teploy sandbox stop 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod     # keep its filesystem, free the container
teploy sandbox resume 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod --ttl 2h
teploy sandbox fork 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod     # a new sandbox from this one's filesystem
teploy sandbox rm 01JCV5ZK3Q7X9B2D4E6F8A0B1C --host prod
```

## What a sandbox is

A sandbox is a container the engine starts from a Docker image on the server. It gets its own filesystem and network namespace. It has no network unless you ask for one (`--network allowlist` or `--network open`), and it is destroyed when its TTL passes (30 minutes by default, 24 hours at most) unless you stop it first or asked for `--on-expire stop`. Commands run in it through `sh -c` in `/work`, as the image's `ship` user when it has one and as root otherwise (`--user root` forces root). It may use 1 CPU, 1024 MiB and 256 processes unless you say otherwise (`--cpus`, `--memory-mb`, `--pids`).

Any Docker image works. The engine's own README documents the rest of what a run can be.

**stop** commits the sandbox's filesystem, removes its container and keeps the sandbox, under its own id, as stopped: it costs disk and nothing else until it is resumed, forked or removed, or 14 days pass. **resume** boots it again with a new deadline. **fork** starts a new sandbox from a running or stopped one's filesystem and settings, and leaves the source as it is.

## Reaching the engine

`host` installs the engine as the systemd service `teploy-sandbox`. It runs as root (it drives Docker), listens on the server's loopback at `127.0.0.1:7439` and nowhere else, and is restarted when it fails.

Every other command opens the SSH connection teploy always opens, reads the engine's token on the server, and forwards a connection to that loopback address. That is the only way in: nothing the engine serves is on a public address, the token never leaves the SSH connection, and the engine is spoken to the way teploy-ship speaks to the one it runs its agent sandboxes on (its HTTP API with a bearer token).

The server needs systemd and Docker, as `teploy setup` leaves them. `host` needs root or passwordless sudo. The other commands need the server's sshd to allow TCP forwarding, which is its default; one configured with `AllowTcpForwarding no` is told apart and said so.

A daemon that was set up another way (by hand, or by teploy-ship) can be used without `host`: pass `--addr` with the address it listens on and `--token-file` with the path of its token. `--addr` has to be on the server's loopback, written as an IP (`127.0.0.1:7439`, `[::1]:7439`): the engine speaks plain HTTP, so the bearer token would travel to any other address unencrypted. An engine on another address or a name needs `--allow-remote-addr` as well, and only makes sense on a network you trust.

## Installing: `sandbox host`

```
teploy sandbox host <server> [--yes] [--json]
```

The engine is a pinned release of `teploy-sandbox` (`v0.1.0`), downloaded by the CLI from the repository's GitHub releases: the binary is checked against the SHA-256 the CLI pins for it (and against the release's checksum) before anything is uploaded, then run once on the server to confirm it is the release it claims, and only then published. It is never built on the server. The pinned hashes are in the CLI (`sandboxEngineSHA256` in `internal/cli/sandbox_host.go`) and are filled in when the release is published; until then `host` says it has no pinned hash and installs nothing. If the release has not been published, the command says so and installs nothing.

It leaves on the server:

| What | Where |
|---|---|
| the engine | `/usr/local/bin/teploy-sandbox` |
| the binary an upgrade replaced (after an upgrade) | `/usr/local/bin/teploy-sandbox.prev`, put back automatically if the new engine does not answer |
| the service | `/etc/systemd/system/teploy-sandbox.service`, owned by root and marked as written by this command |
| the token (`0600`), the stopped sandboxes' registry and the warm cache | `/var/lib/teploy-sandbox/` |

Running it again is safe:

- the server has this CLI's release running, with the service file this CLI writes: nothing is done;
- it has an older one: it is upgraded. The binary it replaces is kept as `teploy-sandbox.prev`, and put back (the service restarted) if the new engine does not answer. It asks first, because it restarts the engine;
- the service file is not what this CLI writes (changed by hand, or written by another CLI version): it is rewritten. The engine is restarted if it was running, so it asks first then; a service that was not running is just started. If the engine does not answer with the new file, the one that was there is put back;
- the service is installed and stopped: it is started;
- the binary is gone but the service is still installed: it is installed again;
- the server has a newer release than this CLI's: nothing is changed. Update the CLI (`teploy update`) instead.

**What a restart costs.** It is the engine's own rule, from its README ("Surviving a restart"): the running sandboxes made with `--on-expire stop` are stopped and kept, every other running sandbox is destroyed, the stopped ones stay, and every command still running in a sandbox is cut off. The prompt says so, with the counts.

It never replaces what it did not install. A `teploy-sandbox.service` without its marker, a binary put there by hand, a daemon already answering at `127.0.0.1:7439`, or another service on that port is refused with nothing changed.

`--json` needs `--yes`.

## The commands

Every command but `host` names the server with `--host`: a name from servers.yml or a host. `--user` and `--key` apply as everywhere else.

| Command | Does |
|---|---|
| `new --image IMAGE` | Creates a sandbox and prints its id. `--ttl` (default 30m), `--network none\|allowlist\|open`, `--env KEY=VALUE` (repeatable), `--cpus`, `--memory-mb`, `--pids`, `--user default\|root`, `--label KEY=VALUE` (repeatable, at most 15), `--on-expire destroy\|stop`, `--driver`. |
| `ls` | Lists every sandbox, running and stopped, oldest first. |
| `exec <id> -- <command>` | Runs a command in a running sandbox and streams its output. The `--` ends teploy's own flags: each word after it is passed as the argument your shell made of it, so quoting survives; shell syntax (a pipe, a redirect) goes inside one quoted word, as in `-- 'ls \| wc -l'`. `--timeout` (default 2m), `--env`, `--workdir` (a directory in the sandbox's `/work`; `/work/…` may be written in full, and `/work` is the default). |
| `cp` | Copies one file (at most 64 MiB). The sandbox's side is `<id>:<path>`, relative to its `/work` (`/work/…` may be written in full). A copy out keeps the mode of a file that is already there, as `cp` does; a new file is `0644`. |
| `stop <id>` | Stops it and keeps its filesystem. |
| `resume <id>` | Resumes a stopped one. `--ttl`. |
| `fork <id>` | A new sandbox from this one's filesystem. `--ttl`, `--label`, `--driver`. |
| `rm <id>` | Destroys it. Asks first unless `--yes`. |

`stop` and `resume` of a sandbox already in that state are not errors: they say so.

**Exit status of `exec`.** It is the command's own, as `ssh` passes it on: `0` when the command exited 0, and the status it exited with otherwise (a command its `--timeout` ended exits 124). When teploy itself fails, `exec` exits `255`: the engine could not be reached or refused, the sandbox could not run the command, the stream ended before the exit was known, or the invocation was wrong. So `255` always means teploy, except that a command which itself exits 255 looks the same by status alone; under `--json` the classes tell them apart (`command-failed` is the command). This is the one teploy command that does not exit 1 on failure: a command's status is passed through, `2` included.

## When something is wrong on the server

The commands look at the server before they fail, and say which of these it is:

- **the engine is not installed**: they print `teploy sandbox host <server>`;
- **it is installed and its service is not running**: the systemctl commands that show why and start it, and the last lines of its log when the server lets them be read;
- **the vm driver is not available**: the engine runs the docker driver.

Under `--json` all three are the error envelope with code `unsupported`, and so is a CLI build that has no pinned hash for the engine release it would install.

## Losing an answer

A create and a fork are sent once, never twice: a retry would make a second sandbox. When the engine's answer is lost, the command says the outcome is unknown, and to list the sandboxes before trying again. Every sandbox the CLI makes carries the label `teploy.cli-run=<id>` for exactly that: `ls` shows it.

An `exec` whose stream dies before the command's exit is reported the same way, as an outcome that is not known: closing the stream ends the command on the engine's side, but whether it had finished by then is not known. So is an `exec` whose answer never came. Neither is sent again. Under `--json` all of these are code `uncertain-outcome`.

## The vm driver

The CLI targets the engine's docker driver, which is what `host` installs. The vm driver is experimental (it needs KVM, a ZFS pool and a golden guest image; every sandbox is root inside it and warm caches are unavailable). On a server where it is already set up, `--driver vm` on `new` and `fork` opts into it; without it, a server running the vm driver is refused with the flag named. `host --driver vm` is refused: it installs the docker driver only.

## Machine output

Every `--json` document carries `machine_interface` at its root, like the rest of the CLI (capability `sandbox-v1`; schemas in `contracts/schema/sandbox-*.schema.json`). Field names are the CLI's snake_case.

| Command | Document |
|---|---|
| `new`, `stop`, `resume`, `fork` | `{machine_interface, server, host, run}` |
| `ls` | `{machine_interface, server, host, driver, runs[]}`, oldest first |
| `exec` | `{machine_interface, server, host, run_id, exit_code, timed_out, duration_ms, stdout, stderr, truncated}` |
| `rm` | `{machine_interface, server, host, run_id}` |
| `cp` | `{machine_interface, server, host, run_id, path, direction, bytes}` |
| `host` | `{machine_interface, server, host, ok, action, version, from, addr, token_file}` |

A measure the engine did not take is left out of a run's `usage`, never reported as zero. `exec --json` captures the output (at most 1 MiB a stream, `truncated` says so) instead of streaming it; `exit_code` is the command's, `-1` when the sandbox itself could not run it, and `null` when the stream died before the exit was known. The document is printed only once the engine has taken the command, and for a nonzero exit too: teploy then exits with the command's status. An `exec` the engine refused (a sandbox that is not there, a stopped one, a bad id) prints nothing on stdout, only the error envelope. `host --json` reports `action` as `install`, `upgrade`, `start` or `unchanged`, with `from` on an upgrade only.

Failures are the error envelope on stderr. An engine that cannot serve a request is `unsupported`; the engine's own refusal of a request in the wrong state (a command in a stopped sandbox) is `conflict`; an outcome that is not known is `uncertain-outcome`; a command that exited nonzero, or that its timeout ended, is `command-failed`. Everything else is `internal`.

`host` records a `sandbox.host` event in the server's history when it changes something.

## Not in this release

- **Recoverable execs.** The engine can keep a command running when the connection that started it drops, and report on it afterwards. `exec` does not use that yet: its stream is the only word on how the command ended.
- **Leases, snapshots, warm caches, remote storage, and published ports or SSH into a sandbox.** The engine has routes for all of them; no command here reaches them.
- **Systemd sandboxing on the service.** The unit has none: the engine drives Docker, so it is root-equivalent whatever the unit says. What keeps it in is that it listens on the loopback only.
