//go:build integration

// Fault-prototype harness (programme workstream C01). NOT run by default:
// this file carries the repo's integration-test build tag (CLAUDE.md
// convention), excluded from `go test ./...`, and drives a REAL SSH+Docker
// host through the implementation handoff's three decisive crash scenarios:
//
//	(a) delayed command completing after owner death — a docker effect
//	    issued by an owner whose process dies lands AFTER a new owner has
//	    broken the stale lock and acquired it; the new owner's
//	    reconciliation (Decide over observed evidence) must detect the late
//	    effect instead of assuming lock acquisition proves quiescence.
//	(b) crash after side effect but before receipt — candidate containers
//	    running, no state.json, no release record: the decision function
//	    must return the reconciliation disposition (INSPECT), never
//	    invented success.
//	(c) abandoned owner (fence break) — a stale holder's late write must
//	    be refused by the EXISTING fence machinery (lock.Guarded /
//	    state.WriteFenced → ErrFenceLost), using no harness-local locking.
//
// Invocation (fixture host required — running it is the NEXT slice; this
// slice proves it compiles and skips cleanly):
//
//	TEPLOY_FAULT_HOST=10.0.0.5 \
//	TEPLOY_FAULT_USER=root \
//	TEPLOY_FAULT_KEY=~/.ssh/id_ed25519 \
//	go test -tags integration -run TestFaultHarness -v ./internal/deploy/recovery
//
// Host prerequisites: Docker reachable by the SSH user (no sudo), the
// fixture image pullable (default alpine:3, override TEPLOY_FAULT_IMAGE),
// and a writable /deployments (the harness creates and removes
// /deployments/<prefix>-{a,b,c} — run against a DISPOSABLE fixture only).
// Caddy is optional: the route evidence classes read Absent when no
// /deployments/caddy/Caddyfile exists.
//
// It is a test (not cmd/) because it is fixture-gated verification, not a
// shipped binary — matching how CLAUDE.md scopes integration tests.
package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

const faultSkipMsg = "fault harness needs a real SSH+Docker fixture: set TEPLOY_FAULT_HOST, TEPLOY_FAULT_USER, TEPLOY_FAULT_KEY (see internal/deploy/recovery/harness_integration_test.go header and docs/C01_RECOVERY_STATE_TABLE.md)"

type faultEnv struct {
	host  string
	user  string
	key   string
	app   string // app-name prefix for the scenario fixtures
	image string
}

func faultEnvFrom(t *testing.T) faultEnv {
	t.Helper()
	host := os.Getenv("TEPLOY_FAULT_HOST")
	user := os.Getenv("TEPLOY_FAULT_USER")
	key := os.Getenv("TEPLOY_FAULT_KEY")
	if host == "" || user == "" || key == "" {
		t.Skip(faultSkipMsg)
	}
	e := faultEnv{
		host:  host,
		user:  user,
		key:   key,
		app:   os.Getenv("TEPLOY_FAULT_APP"),
		image: os.Getenv("TEPLOY_FAULT_IMAGE"),
	}
	if e.app == "" {
		e.app = "faultprobe"
	}
	if e.image == "" {
		e.image = "alpine:3"
	}
	return e
}

func faultConnect(t *testing.T, env faultEnv, label string) *ssh.RemoteExecutor {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: env.host, User: env.user, KeyPath: env.key})
	if err != nil {
		t.Fatalf("connecting %s session to %s@%s: %v", label, env.user, env.host, err)
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

// ageLockStale simulates the stale window elapsing: the lock info keeps
// its ORIGINAL owner token (the dead holder's fencing identity) but its
// timestamp moves past staleLockTTL, so the next AcquireLockFenced walks
// the genuine stale-break path — ReleaseLock, mkdir, fresh owner token.
func ageLockStale(t *testing.T, exec ssh.Executor, app, owner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	old := time.Now().UTC().Add(-31 * time.Minute).Format(time.RFC3339)
	info := fmt.Sprintf("{\"type\":\"auto\",\"owner\":%q,\"ts\":%q}\n", owner, old)
	path := fmt.Sprintf("/deployments/%s/.lock/info", app)
	if err := exec.Upload(ctx, strings.NewReader(info), path, "0644"); err != nil {
		t.Fatalf("aging %s's lock: %v", app, err)
	}
}

func fileExists(t *testing.T, exec ssh.Executor, path string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.Run(ctx, fmt.Sprintf("test -e %s && echo yes || echo no", ssh.ShellQuote(path)))
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return strings.TrimSpace(out) == "yes"
}

func waitRunning(t *testing.T, exec ssh.Executor, name string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(name)+" 2>/dev/null || true")
		cancel()
		if err == nil && strings.TrimSpace(out) == "running" {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// observe collects a recovery.Observation from the live host with exact
// names and receipts (no guessed identities): the docker label inventory,
// state.json, the managed Caddy block, and the per-release record. This is
// the evidence-collection half the C01 helper will productionize; it lives
// in the harness so the decision package stays pure.
func observe(ctx context.Context, exec ssh.Executor, app, attempted, predecessor string) Observation {
	var o Observation

	dk := docker.NewClient(exec)
	containers, err := dk.ListContainers(ctx, app)
	if err != nil {
		o.Candidates, o.CandidateCorpses, o.ForeignCandidates = Unknown, Unknown, Unknown
	} else {
		candPrefix := fmt.Sprintf("%s-web-%s", app, attempted)
		predName := ""
		if predecessor != "" {
			predName = fmt.Sprintf("%s-web-%s", app, predecessor)
		}
		for _, c := range containers {
			isCandidate := strings.HasPrefix(c.Name, candPrefix)
			isPredecessor := predName != "" &&
				(strings.HasPrefix(c.Name, predName) || strings.HasPrefix(c.Name, predName+"_replaced"))
			switch {
			case isCandidate:
				if c.State == "running" {
					o.Candidates = Present
				} else {
					o.CandidateCorpses = Present
				}
			case isPredecessor:
				if strings.HasSuffix(c.Name, "_replaced") || c.State == "running" {
					o.PredecessorServing = Present
				} else {
					o.PredecessorStopped = Present
				}
			default:
				// Neither the attempted release's candidate naming nor the
				// known predecessor's: an unattributable workload — only
				// RUNNING ones consume traffic/jobs.
				if c.State == "running" {
					o.ForeignCandidates = Present
				}
			}
		}
	}

	st, err := state.Read(ctx, exec, app)
	switch {
	case err != nil:
		o.StateToCandidate, o.StateToPredecessor = Unknown, Unknown
	case st == nil:
		o.StateToCandidate, o.StateToPredecessor = Absent, Absent
	case st.CurrentHash == attempted:
		o.StateToCandidate = Present
	default:
		o.StateToPredecessor = Present
	}

	caddyfile, present, err := state.ReadRemoteFile(ctx, exec, "/deployments/caddy/Caddyfile")
	switch {
	case err != nil:
		o.RouteToCandidate, o.RouteToPredecessor = Unknown, Unknown
	case !present:
		o.RouteToCandidate, o.RouteToPredecessor = Absent, Absent
	case strings.Contains(string(caddyfile), fmt.Sprintf("%s-web-%s", app, attempted)):
		o.RouteToCandidate = Present
	default:
		o.RouteToPredecessor = Present
	}

	rec, err := releasemeta.Read(ctx, exec, app, attempted)
	switch {
	case err != nil:
		o.ReleaseRecord = Unknown
	case rec != nil:
		o.ReleaseRecord = Present
	default:
		o.ReleaseRecord = Absent
	}
	return o
}

// TestFaultHarness drives the handoff's decisive scenarios against a real
// host and prints the scenario × observed × decision × correctness result
// table. Each scenario is independent (own app dir); failures are reported
// per row, not silently aggregated.
func TestFaultHarness(t *testing.T) {
	env := faultEnvFrom(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Fixture sanity: docker must answer and the image must be pullable.
	probe := faultConnect(t, env, "probe")
	if out, err := probe.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v) — the harness requires SSH+Docker", err)
	} else {
		t.Logf("fixture docker server %s", strings.TrimSpace(out))
	}
	if _, err := probe.Run(ctx, "docker pull "+ssh.ShellQuote(env.image)); err != nil {
		t.Skipf("cannot pull fixture image %s (%v) — pre-pull it or set TEPLOY_FAULT_IMAGE", env.image, err)
	}

	type row struct {
		scenario, observed, decision string
		pass                         bool
		note                         string
	}
	var rows []row
	mkContainer := func(name string, labels map[string]string) string {
		args := []string{"docker", "run", "--detach", "--restart", "no", "--name", ssh.ShellQuote(name)}
		for k, v := range labels {
			args = append(args, "--label", ssh.ShellQuote(k+"="+v))
		}
		args = append(args, ssh.ShellQuote(env.image), "sleep", "300")
		return strings.Join(args, " ")
	}
	var containers []string
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		for _, n := range containers {
			probe.Run(cctx, "docker rm -f "+ssh.ShellQuote(n))
		}
		for _, suffix := range []string{"a", "b", "c"} {
			probe.Run(cctx, "rm -rf "+ssh.ShellQuote(fmt.Sprintf("/deployments/%s-%s", env.app, suffix)))
		}
	})

	// ------------------------------------------------------------------
	// (a) Delayed effect completing after owner death.
	// ------------------------------------------------------------------
	appA := env.app + "-a"
	ownerA := faultConnect(t, env, "owner A")
	if err := state.EnsureAppDir(ctx, ownerA, appA); err != nil {
		t.Fatalf("(a) creating app dir: %v", err)
	}
	lkA, err := state.AcquireLockFenced(ctx, ownerA, appA)
	if err != nil {
		t.Fatalf("(a) owner A acquiring the lock: %v", err)
	}
	// Owner A issues a long docker effect detached from its session, then
	// DIES (session closed, no renewal, no release). The effect — a
	// candidate-shaped container of release "deadgen" — lands ~4s later.
	lateName := fmt.Sprintf("%s-web-deadgen", appA)
	containers = append(containers, lateName)
	inner := fmt.Sprintf("sleep 4; docker run --detach --restart no --name %s --label %s --label %s --label %s %s sleep 300",
		ssh.ShellQuote(lateName),
		ssh.ShellQuote("teploy.app="+appA),
		ssh.ShellQuote("teploy.process=web"),
		ssh.ShellQuote("teploy.version=deadgen"),
		ssh.ShellQuote(env.image))
	if _, err := ownerA.Run(ctx, "nohup sh -c "+ssh.ShellQuote(inner)+" >/dev/null 2>&1 & echo launched"); err != nil {
		t.Fatalf("(a) launching the delayed effect: %v", err)
	}
	ownerA.Close() // owner death: the session dies; the nohup'd effect survives

	// The stale window elapses (simulated by aging the info file — the
	// token stays owner A's), then owner B breaks and acquires the lock
	// through the REAL stale-break path.
	ownerB := faultConnect(t, env, "owner B")
	ageLockStale(t, ownerB, appA, lkA.Owner())
	lkB, err := state.AcquireLockFenced(ctx, ownerB, appA)
	if err != nil {
		t.Fatalf("(a) owner B acquiring the stale-broken lock: %v", err)
	}
	defer state.ReleaseLockFenced(ownerB, lkB, appA)

	landed := waitRunning(t, ownerB, lateName, 20*time.Second)
	if !landed {
		t.Errorf("(a) the delayed effect never landed — the fixture's nohup detach did not survive the session; fix the fixture before trusting this scenario")
	}
	// B reconciles BEFORE acting: it observes the app, then decides. The
	// quiescence assumption would be the all-absent observation (RETRY for
	// a fresh deploy); the real observation must not collapse to it.
	obsA := observe(ctx, ownerB, appA, "newgen", "")
	d := Decide(Admitted, obsA)
	quiescent := Decide(Admitted, Observation{})
	rows = append(rows, row{
		scenario: "(a) late effect after owner death",
		observed: fmt.Sprintf("lock acquired by new owner; foreign running container %s landed post-acquisition=%v", lateName, landed),
		decision: fmt.Sprintf("%s (quiescence assumption would say %s)", d, quiescent),
		pass:     d == Manual && d != quiescent,
		note:     "acquisition proves nothing; the late effect is unattributable → MANUAL",
	})

	// ------------------------------------------------------------------
	// (b) Crash after side effect, before receipt.
	// ------------------------------------------------------------------
	appB := env.app + "-b"
	ownerB2 := faultConnect(t, env, "owner B2")
	if err := state.EnsureAppDir(ctx, ownerB2, appB); err != nil {
		t.Fatalf("(b) creating app dir: %v", err)
	}
	lkB2, err := state.AcquireLockFenced(ctx, ownerB2, appB)
	if err != nil {
		t.Fatalf("(b) acquiring the lock: %v", err)
	}
	defer state.ReleaseLockFenced(ownerB2, lkB2, appB)
	candB := fmt.Sprintf("%s-web-c0ffee", appB)
	containers = append(containers, candB)
	if _, err := ownerB2.Run(ctx, mkContainer(candB, map[string]string{
		"teploy.app": appB, "teploy.process": "web", "teploy.version": "c0ffee",
	})); err != nil {
		t.Fatalf("(b) starting the candidate side effect: %v", err)
	}
	obsB := observe(ctx, ownerB2, appB, "c0ffee", "")
	dB := Decide(CandidatesRunning, obsB)
	rows = append(rows, row{
		scenario: "(b) side effect without receipt",
		observed: fmt.Sprintf("candidate %s running; state=%s route=%s record=%s", candB, obsB.StateToCandidate, obsB.RouteToCandidate, obsB.ReleaseRecord),
		decision: dB.String(),
		pass:     dB == Inspect,
		note:     "reconciliation disposition, never invented success",
	})

	// ------------------------------------------------------------------
	// (c) Abandoned owner: fence break refuses the stale holder's writes.
	// ------------------------------------------------------------------
	appC := env.app + "-c"
	ownerC1 := faultConnect(t, env, "stale holder C1")
	if err := state.EnsureAppDir(ctx, ownerC1, appC); err != nil {
		t.Fatalf("(c) creating app dir: %v", err)
	}
	lkC1, err := state.AcquireLockFenced(ctx, ownerC1, appC)
	if err != nil {
		t.Fatalf("(c) stale holder acquiring: %v", err)
	}
	ownerC2 := faultConnect(t, env, "successor C2")
	ageLockStale(t, ownerC2, appC, lkC1.Owner())
	lkC2, err := state.AcquireLockFenced(ctx, ownerC2, appC)
	if err != nil {
		t.Fatalf("(c) successor breaking the stale lock: %v", err)
	}
	defer state.ReleaseLockFenced(ownerC2, lkC2, appC)

	marker := fmt.Sprintf("/deployments/%s/late-marker", appC)
	_, gerr := lkC1.Guarded(ctx, ownerC1, "touch "+ssh.ShellQuote(marker))
	refusedEffect := errors.Is(gerr, state.ErrFenceLost)
	markerAbsent := !fileExists(t, ownerC2, marker)

	werr := state.WriteFenced(ctx, ownerC1, appC, state.NewAppliedState(nil, "container", "host", ""), lkC1)
	refusedState := errors.Is(werr, state.ErrFenceLost)
	stateAbsent := !fileExists(t, ownerC2, fmt.Sprintf("/deployments/%s/state.json", appC))
	rows = append(rows, row{
		scenario: "(c) stale holder's late write",
		observed: fmt.Sprintf("guarded effect refused=%v marker-on-disk=%v; fenced state commit refused=%v state.json=%v",
			refusedEffect, markerAbsent, refusedState, stateAbsent),
		decision: "refused (ErrFenceLost)",
		pass:     refusedEffect && markerAbsent && refusedState && stateAbsent,
		note:     "existing fence machinery; no harness-local locking",
	})

	// ------------------------------------------------------------------
	// Result table.
	// ------------------------------------------------------------------
	fmt.Println("\n== C01 fault harness — scenario × observed × decision × correctness ==")
	fmt.Printf("%-36s | %-72s | %-46s | %s\n", "scenario", "observed", "decision", "correctness")
	fmt.Println(strings.Repeat("-", 36) + "-+-" + strings.Repeat("-", 72) + "-+-" + strings.Repeat("-", 46) + "-+-------")
	allPass := true
	for _, r := range rows {
		verdict := "FAIL"
		if r.pass {
			verdict = "PASS"
		} else {
			allPass = false
		}
		fmt.Printf("%-36s | %-72s | %-46s | %s\n", r.scenario, r.observed, r.decision, verdict)
		t.Logf("%s — %s (%s)", r.scenario, r.note, verdict)
	}
	fmt.Println(strings.Repeat("-", 180))
	if !allPass {
		t.Error("fault harness: at least one scenario failed — see the table above")
	}
}
