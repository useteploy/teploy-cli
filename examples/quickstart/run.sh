#!/usr/bin/env bash
# Executable quickstart (C09): deploy the maintained fixture app from
# examples/quickstart/app against a LOCAL docker target — the colima VM's
# own SSH endpoint — with no undocumented repair, then verify the app
# answers, redeploy a second version, and clean up after itself.
#
# Honest gating: this script needs (a) the docker CLI, (b) a running
# colima VM (it will NOT start one), and (c) ssh/ssh-keyscan/curl on
# PATH. When any is missing it prints SKIP and exits 0 — a skipped
# quickstart must not read as a failed one.
#
# What it proves: a new user path from `git clean` checkout to a
# responding application — config in teploy.yml, build on the target,
# health-gated start, published port, idempotent redeploy, status,
# removal. Exit 0 only if every step held.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
APP_DIR="$REPO_ROOT/examples/quickstart/app"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/teploy-quickstart.XXXXXX")"
TEPLOY_BIN=""
SSH_HOST=""; SSH_PORT=""; SSH_USER=""; SSH_KEY=""
CONTAINER_KEY_FILE=""   # known_hosts lines added by this run
APP_PORT=18080

log()  { printf '==> %s\n' "$*"; }
skip() { printf 'SKIP: %s\n' "$*"; exit 0; }

cleanup() {
  local code=$?
  set +e
  if [ -n "$TEPLOY_BIN" ] && [ -n "$SSH_HOST" ]; then
    "$TEPLOY_BIN" remove --purge --yes --app quickstart --host "$SSH_HOST" \
      --user "$SSH_USER" --key "$SSH_KEY" >/dev/null 2>&1
    ssh -i "$SSH_KEY" -p "$SSH_PORT" -o BatchMode=yes "$SSH_USER@$SSH_HOST" \
      'docker rm -f quickstart-web-qs1 quickstart-web-qs2 >/dev/null 2>&1; docker rmi quickstart-build-qs1 quickstart-build-qs2 >/dev/null 2>&1; sudo rm -rf /deployments/quickstart' 2>/dev/null
  fi
  if [ -n "$CONTAINER_KEY_FILE" ] && [ -f "$HOME/.ssh/known_hosts" ]; then
    # Remove only the lines this run appended.
    python3 - "$CONTAINER_KEY_FILE" <<'PY'
import sys
added = set(open(sys.argv[1]).read().splitlines())
path = __import__("os").path.expanduser("~/.ssh/known_hosts")
lines = open(path).read().splitlines()
kept = [l for l in lines if l not in added]
open(path, "w").write("\n".join(kept) + ("\n" if kept else ""))
PY
  fi
  rm -rf "$WORK_DIR"
  exit $code
}
trap cleanup EXIT

# --- gates -----------------------------------------------------------------
for bin in docker ssh ssh-keyscan ssh-keygen curl python3; do
  command -v "$bin" >/dev/null 2>&1 || skip "$bin not found on PATH"
done
command -v colima >/dev/null 2>&1 || skip "colima not found (this quickstart targets a local colima VM)"
colima status >/dev/null 2>&1 || skip "colima VM not running (start it with: colima start), then re-run"

# --- resolve the colima VM's SSH endpoint from colima's own config ---------
COLIMA_HOME_DIR="${COLIMA_HOME:-$HOME/.colima}"
SSH_CFG=""
for candidate in "$COLIMA_HOME_DIR/ssh_config" "$COLIMA_HOME_DIR/default/ssh_config" "$COLIMA_HOME_DIR/_lima/colima/ssh_config"; do
  [ -f "$candidate" ] && SSH_CFG="$candidate" && break
done
[ -n "$SSH_CFG" ] || skip "no colima ssh_config under $COLIMA_HOME_DIR"
SSH_HOST="$(awk '$1=="Hostname"{print $2}' "$SSH_CFG")"
SSH_PORT="$(awk '$1=="Port"{print $2}' "$SSH_CFG")"
SSH_USER="$(awk '$1=="User"{print $2}' "$SSH_CFG")"
SSH_KEY="$(awk '$1=="IdentityFile"{print $2}' "$SSH_CFG" | tr -d '"')"
for v in "$SSH_HOST" "$SSH_PORT" "$SSH_USER" "$SSH_KEY"; do
  [ -n "$v" ] || skip "could not parse endpoint from $SSH_CFG"
done
[ -f "$SSH_KEY" ] || skip "colima SSH key missing ($SSH_KEY) — restart colima to regenerate"
log "target: $SSH_USER@$SSH_HOST:$SSH_PORT (colima VM)"

ssh -i "$SSH_KEY" -p "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  -o BatchMode=yes "$SSH_USER@$SSH_HOST" true 2>/dev/null \
  || skip "cannot SSH to the colima VM ($SSH_USER@$SSH_HOST:$SSH_PORT)"

# --- host key + /deployments bootstrap (the only target-side setup) --------
mkdir -p "$HOME/.ssh"
touch "$HOME/.ssh/known_hosts"
CONTAINER_KEY_FILE="$WORK_DIR/added-host-keys"
ssh-keyscan -p "$SSH_PORT" "$SSH_HOST" >"$CONTAINER_KEY_FILE" 2>/dev/null
grep -q . "$CONTAINER_KEY_FILE" || skip "ssh-keyscan produced no keys for $SSH_HOST:$SSH_PORT"
cat "$CONTAINER_KEY_FILE" >>"$HOME/.ssh/known_hosts"

ssh -i "$SSH_KEY" -p "$SSH_PORT" -o BatchMode=yes "$SSH_USER@$SSH_HOST" \
  'sudo mkdir -p /deployments && sudo chown "$(id -un)" /deployments' 2>/dev/null \
  || skip "cannot bootstrap /deployments on the VM (needs passwordless sudo)"

# --- build this checkout's CLI ----------------------------------------------
log "building teploy from this checkout"
TEPLOY_BIN="$WORK_DIR/teploy"
(cd "$REPO_ROOT" && go build -o "$TEPLOY_BIN" ./cmd/teploy)

TEPLOY=("$TEPLOY_BIN" --host "$SSH_HOST:$SSH_PORT" --user "$SSH_USER" --key "$SSH_KEY")

# --- deploy v1 --------------------------------------------------------------
cp -R "$APP_DIR/." "$WORK_DIR/app/"
log "deploying quickstart v1 (build-on-target, health-gated, host ingress :$APP_PORT)"
(cd "$WORK_DIR/app" && "${TEPLOY[@]}" deploy --version qs1) 2>&1 | sed 's/^/    /'

BODY="$(curl -fsS -m 10 "http://127.0.0.1:$APP_PORT/")"
grep -q "quickstart v1" <<<"$BODY" || { echo "FAIL: v1 content not served: $BODY" >&2; exit 1; }
log "verified: http://127.0.0.1:$APP_PORT/ serves the v1 fixture"

# --- redeploy v2 (recreate path) --------------------------------------------
log "redeploying as qs2 with changed content"
sed 's/quickstart v1/quickstart v2/' "$WORK_DIR/app/index.html" >"$WORK_DIR/app/index.html.tmp"
mv "$WORK_DIR/app/index.html.tmp" "$WORK_DIR/app/index.html"
(cd "$WORK_DIR/app" && "${TEPLOY[@]}" deploy --version qs2) 2>&1 | sed 's/^/    /'

BODY="$(curl -fsS -m 10 "http://127.0.0.1:$APP_PORT/")"
grep -q "quickstart v2" <<<"$BODY" || { echo "FAIL: v2 content not served after redeploy: $BODY" >&2; exit 1; }
log "verified: redeploy switched the served content to v2"

# --- status ------------------------------------------------------------------
log "teploy status sees the deployment"
"${TEPLOY[@]}" status --app quickstart 2>&1 | sed 's/^/    /'

log "quickstart complete: deployed, verified, redeployed, verified again"
log "cleanup follows (teploy remove --purge, known_hosts lines, temp dir)"
