//go:build integration

// Fixture-gated verification of the C01-8/9 + A12/T05 acceptance against a
// REAL SSH+Docker host: generation labels read at EXECUTION time (a delayed
// stop command landing after the world moved on refuses against the live
// label), the committed-generation sidecar CAS on the real FS, and the
// exact-block route CAS against a real Caddyfile through the production
// caddy.Client (lock, adapt gate, guarded commit, reload, delivery
// verification).
//
// Same fixture contract as the other harnesses:
//
//	TEPLOY_FAULT_HOST=127.0.0.1:50075 \
//	TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -run TestGenerationIntegration -v ./internal/deploy
//
// Needs caddy:2-alpine + alpine:3 pullable; skips when /deployments/caddy
// already exists (a provisioned real caddy would conflict). Disposable
// fixture only: creates and removes genprobe-* containers plus
// /deployments/{caddy,genprobe}.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

const genProbeApp = "genprobe"

func genRun(t *testing.T, exec ssh.Executor, ctx context.Context, cmd string) string {
	t.Helper()
	out, err := exec.Run(ctx, cmd)
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return out
}

// TestGenerationIntegration_DelayedStopRefusesNewerGeneration proves the
// delayed-SSH-effect acceptance on real docker: a container labeled
// generation 9 (a successor's same-hash redeploy) is NOT stoppable by an
// operation prepared against generation 7 — including a stop command whose
// EXECUTION is delayed past the label change (nohup), because the label
// check reads the container's identity at execution time, not at issue
// time. Legacy unlabeled containers keep stopping (compat).
func TestGenerationIntegration_DelayedStopRefusesNewerGeneration(t *testing.T) {
	host, user, key, image := reconcileFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	if out, err := exec.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v)", err)
	} else {
		t.Logf("fixture docker server %s", strings.TrimSpace(out))
	}

	name := genProbeApp + "-web-abc123"
	legacy := genProbeApp + "-web-legacy"
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		exec.Run(cctx, "docker rm -f "+name+" "+legacy)
	})

	dk := docker.NewClient(exec)
	// A newer generation's container under the name a stale rollback
	// resolved (generation 7), and a legacy unlabeled one.
	genRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --name %s --label teploy.app=%s --label teploy.version=abc123 --label teploy.generation=9 %s sleep 300", name, genProbeApp, image))
	genRun(t, exec, ctx, fmt.Sprintf(
		"docker run -d --name %s --label teploy.app=%s --label teploy.version=old %s sleep 300", legacy, genProbeApp, image))

	// The generation check reads the LIVE label: expected 7 refuses 9.
	if err := dk.StopGenerationFenced(ctx, name, 2, 7, ""); err == nil {
		t.Fatal("stopping a generation-9 container prepared against 7 must refuse")
	} else if !state.GenerationFenced(err) || !strings.Contains(err.Error(), "TEPLOY_GENERATION_FENCED 9 7") {
		t.Fatalf("refusal must name both generations, got %v", err)
	}
	if st := genRun(t, exec, ctx, "docker inspect -f '{{.State.Status}}' "+name); strings.TrimSpace(st) != "running" {
		t.Fatalf("the newer generation's container must keep running, state %s", st)
	}

	// The DELAYED effect: the composed stop is issued now, executes in 3s —
	// the world it executes against still refuses it (label at execution).
	if out, err := exec.Run(ctx, fmt.Sprintf(
		"nohup sh -c 'sleep 3; %s' >/dev/null 2>&1 &", generationStopShell(name, 7))); err != nil {
		t.Fatalf("launching the delayed stop: %v (%s)", err, out)
	}
	time.Sleep(5 * time.Second)
	if st := genRun(t, exec, ctx, "docker inspect -f '{{.State.Status}}' "+name); strings.TrimSpace(st) != "running" {
		t.Fatalf("the delayed stale stop must have been refused at execution, state %s", st)
	}

	// An operation prepared against the CURRENT generation stops it; a
	// legacy unlabeled container stops too (compat).
	if err := dk.StopGenerationFenced(ctx, name, 2, 9, ""); err != nil {
		t.Fatalf("stopping at the container's own generation must succeed: %v", err)
	}
	if err := dk.StopGenerationFenced(ctx, legacy, 2, 7, ""); err != nil {
		t.Fatalf("a legacy unlabeled container must keep stopping: %v", err)
	}
}

// generationStopShell renders the composed stop exactly as
// docker.StopGenerationFenced issues it (unexported there), for the
// delayed-effect fixture.
func generationStopShell(ref string, expected uint64) string {
	return fmt.Sprintf(
		`g=$(docker inspect -f '{{index .Config.Labels "teploy.generation"}}' '%s' 2>/dev/null || printf '0'); case "$g" in ''|*[!0-9]*) g=0;; esac; [ "$g" -le %d ] || { printf 'TEPLOY_GENERATION_FENCED %%s %%s\n' "$g" %d >&2; exit 74; }; docker stop -t 2 '%s'`,
		ref, expected, expected, ref)
}

// TestGenerationIntegration_CommitCASAndSidecar proves the authority CAS
// on the real FS: WriteFencedGeneration commits state.json + the
// .generation sidecar together, and a stale prepared-against-7 commit
// refuses over a committed 8 with state.json left to the successor.
func TestGenerationIntegration_CommitCASAndSidecar(t *testing.T) {
	host, user, key, _ := reconcileFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		exec.Run(cctx, "rm -rf /deployments/"+genProbeApp)
	})
	genRun(t, exec, ctx, "rm -rf /deployments/"+genProbeApp+" && mkdir -p /deployments/"+genProbeApp)

	lk, err := state.AcquireLockFenced(ctx, exec, genProbeApp)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	defer state.ReleaseLockFenced(exec, lk, genProbeApp)

	mk := func(hash string, gen uint64) *state.AppState {
		return &state.AppState{
			SchemaVersion: state.SchemaVersionV2, DeploymentType: "container", IngressMode: "caddy",
			Domain: "gen.example.com", UpdatedAt: time.Now().UTC(), CurrentHash: hash, Generation: gen,
		}
	}
	if err := state.WriteFencedGeneration(ctx, exec, genProbeApp, mk("v3", 8), lk, 7); err != nil {
		t.Fatalf("clean commit over generation 7 must land: %v", err)
	}
	gen := genRun(t, exec, ctx, "cat /deployments/"+genProbeApp+"/.generation")
	if strings.TrimSpace(gen) != "8" {
		t.Fatalf("the sidecar must carry the committed generation, got %q", gen)
	}

	// The stale rollback: prepared against 7, commits after the successor.
	err = state.WriteFencedGeneration(ctx, exec, genProbeApp, mk("v1", 8), lk, 7)
	if !state.GenerationFenced(err) {
		t.Fatalf("a commit prepared against 7 must refuse over committed 8, got %v", err)
	}
	cur := genRun(t, exec, ctx, "cat /deployments/"+genProbeApp+"/state.json")
	if !strings.Contains(cur, `"current_hash":"v3"`) {
		t.Fatalf("a refused commit must leave the successor's state, got %s", cur)
	}
}

// TestGenerationIntegration_RouteCASAgainstRealCaddy proves the exact-block
// CAS through the production caddy.Client against a real Caddyfile + real
// caddy reload: a switch over the exact predecessor lands stamped with its
// generation; the same expectation replayed after a successor switched
// refuses with BOTH generations named and nothing lands.
func TestGenerationIntegration_RouteCASAgainstRealCaddy(t *testing.T) {
	host, user, key, _ := reconcileFixtureEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	exec, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	if _, err := exec.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v)", err)
	}
	if out, _ := exec.Run(ctx, "test -e /deployments/caddy/Caddyfile && echo yes || echo no"); strings.TrimSpace(out) == "yes" {
		t.Skip("fixture has a provisioned /deployments/caddy — remove it or use a disposable host for this fixture")
	}
	for _, image := range []string{"caddy:2-alpine"} {
		if _, err := exec.Run(ctx, "docker pull "+image); err != nil {
			t.Skipf("cannot pull %s (%v) — pre-pull it on the fixture", image, err)
		}
	}

	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		exec.Run(cctx, "docker rm -f caddy")
		exec.Run(cctx, "rm -rf /deployments/caddy")
	})
	genRun(t, exec, ctx, "docker network create teploy 2>/dev/null || true")
	genRun(t, exec, ctx, "mkdir -p /deployments/caddy")
	if err := exec.Upload(ctx, strings.NewReader("{\n\tadmin 127.0.0.1:2019\n}\n"), "/deployments/caddy/Caddyfile", "0644"); err != nil {
		t.Fatalf("seeding Caddyfile: %v", err)
	}
	genRun(t, exec, ctx, "docker run -d --name caddy --network teploy -v /deployments/caddy:/etc/caddy -p 127.0.0.1:0:80 caddy:2-alpine")

	cd := caddy.NewClient(exec)
	setRoute := func(gen uint64, upstream string) error {
		return cd.WithGeneration(gen).SetRoute(ctx, genProbeApp, "gen.example.com", upstream, 3000, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{})
	}

	// Generation 7's route lands.
	if err := setRoute(7, genProbeApp+"-web-v7"); err != nil {
		t.Fatalf("first route: %v", err)
	}
	resolved, present, err := cd.ReadManagedBlock(ctx, genProbeApp)
	if err != nil || !present {
		t.Fatalf("ReadManagedBlock after first route: %q %v", resolved, err)
	}
	if g, ok := caddy.RegionGeneration(resolved); !ok || g != 7 {
		t.Fatalf("the live block must be stamped generation 7, got %d %v", g, ok)
	}

	// The successor (generation 8) switches the route.
	if err := setRoute(8, genProbeApp+"-web-v8"); err != nil {
		t.Fatalf("successor route: %v", err)
	}

	// The stale operation replays ITS switch, CAS'd on the generation-7
	// region: refused, both generations named, nothing lands.
	err = cd.WithGeneration(7).
		WithRouteCAS(genProbeApp, []string{caddy.ManagedRegionHash(resolved)}, 7).
		SetRoute(ctx, genProbeApp, "gen.example.com", genProbeApp+"-web-v7", 3000, caddy.TLS{}, "", nil, caddy.Firewall{}, caddy.Access{})
	var cas *caddy.ErrRouteCAS
	if !errors.As(err, &cas) {
		t.Fatalf("expected *ErrRouteCAS, got %v", err)
	}
	if cas.ExpectedGeneration != 7 || !cas.FoundStamped || cas.FoundGeneration != 8 {
		t.Errorf("the refusal must name both generations: %+v", cas)
	}
	if len(cas.FoundUpstreams) != 1 || !strings.Contains(cas.FoundUpstreams[0], "web-v8") {
		t.Errorf("the refusal must name the successor's upstreams: %+v", cas)
	}
	live := genRun(t, exec, ctx, "cat /deployments/caddy/Caddyfile")
	if !strings.Contains(live, "genprobe-web-v8") || strings.Contains(live, "TEPLOY GENERATION 7") {
		t.Errorf("a refused CAS must leave the successor's block live:\n%s", live)
	}
}
