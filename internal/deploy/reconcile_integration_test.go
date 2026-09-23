//go:build integration

// Fixture-gated verification of the replacement-owner reconciliation
// (C01-1) against a REAL SSH+Docker host — the wired version of the
// recovery harness's scenario (a): a dead holder's delayed docker effect
// lands after a replacement owner breaks the stale lock, and the
// PRODUCTION reconciler (deploy.ReconcileAfterTakeover, the code DeployFenced
// runs on takeover) must refuse with the MANUAL disposition instead of
// proceeding on the quiescence assumption.
//
// Same fixture contract as internal/deploy/recovery/harness_integration_test.go:
//
//	TEPLOY_FAULT_HOST=127.0.0.1:50075 \
//	TEPLOY_FAULT_USER=tyler \
//	TEPLOY_FAULT_KEY=~/.colima/_lima/_config/user \
//	go test -tags integration -run TestReconcileIntegration -v ./internal/deploy
//
// Disposable fixture only — the test creates and removes
// /deployments/<prefix>-d and <prefix>-d-shaped containers.
package deploy

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

func reconcileFixtureEnv(t *testing.T) (host, user, key, image string) {
	t.Helper()
	host = os.Getenv("TEPLOY_FAULT_HOST")
	user = os.Getenv("TEPLOY_FAULT_USER")
	key = os.Getenv("TEPLOY_FAULT_KEY")
	if host == "" || user == "" || key == "" {
		t.Skip("reconcile fixture needs TEPLOY_FAULT_HOST, TEPLOY_FAULT_USER, TEPLOY_FAULT_KEY (see internal/deploy/reconcile_integration_test.go)")
	}
	image = os.Getenv("TEPLOY_FAULT_IMAGE")
	if image == "" {
		image = "alpine:3"
	}
	return
}

// TestReconcileIntegration_LateEffectAfterTakeoverIsRefused: owner A takes
// the lock, launches a nohup'd delayed candidate, and dies; the lock ages
// stale; owner B breaks and acquires it through the real path (TookOver
// must report true); the late container lands post-acquisition; the
// production reconciler must REFUSE (MANUAL — unattributable running
// workload), proving DeployFenced's takeover path reconciles observed
// evidence instead of assuming quiescence.
func TestReconcileIntegration_LateEffectAfterTakeoverIsRefused(t *testing.T) {
	host, user, key, image := reconcileFixtureEnv(t)
	app := "faultprobe-d"
	lateName := fmt.Sprintf("%s-web-deadgen", app)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	probe, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting probe session: %v", err)
	}
	defer probe.Close()
	if out, err := probe.Run(ctx, "docker version --format '{{.Server.Version}}'"); err != nil {
		t.Skipf("fixture host has no reachable docker daemon (%v)", err)
	} else {
		t.Logf("fixture docker server %s", strings.TrimSpace(out))
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		probe.Run(cctx, "docker rm -f "+ssh.ShellQuote(lateName))
		probe.Run(cctx, "rm -rf "+ssh.ShellQuote("/deployments/"+app))
	})

	if err := state.EnsureAppDir(ctx, probe, app); err != nil {
		t.Fatalf("creating app dir: %v", err)
	}

	// Owner A: take the lock, launch the delayed effect, die.
	ownerA, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting owner A: %v", err)
	}
	lkA, err := state.AcquireLockFenced(ctx, ownerA, app)
	if err != nil {
		t.Fatalf("owner A acquiring the lock: %v", err)
	}
	inner := fmt.Sprintf("sleep 4; docker run --detach --restart no --name %s --label %s --label %s --label %s %s sleep 300",
		ssh.ShellQuote(lateName),
		ssh.ShellQuote("teploy.app="+app),
		ssh.ShellQuote("teploy.process=web"),
		ssh.ShellQuote("teploy.version=deadgen"),
		ssh.ShellQuote(image))
	if _, err := ownerA.Run(ctx, "nohup sh -c "+ssh.ShellQuote(inner)+" >/dev/null 2>&1 & echo launched"); err != nil {
		t.Fatalf("launching the delayed effect: %v", err)
	}
	ownerA.Close()

	// Age the lock past the stale window (token preserved), then owner B
	// breaks and acquires through the real stale-break path.
	ownerB, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		t.Fatalf("connecting owner B: %v", err)
	}
	defer ownerB.Close()
	old := time.Now().UTC().Add(-31 * time.Minute).Format(time.RFC3339)
	info := fmt.Sprintf("{\"type\":\"auto\",\"owner\":%q,\"ts\":%q}\n", lkA.Owner(), old)
	if err := ownerB.Upload(ctx, strings.NewReader(info), fmt.Sprintf("/deployments/%s/.lock/info", app), "0644"); err != nil {
		t.Fatalf("aging the lock: %v", err)
	}
	lkB, err := state.AcquireLockFenced(ctx, ownerB, app)
	if err != nil {
		t.Fatalf("owner B acquiring the stale-broken lock: %v", err)
	}
	defer state.ReleaseLockFenced(ownerB, lkB, app)
	if !lkB.TookOver() {
		t.Fatal("the stale-break acquisition must report TookOver — the reconciliation keys on it")
	}

	// Wait for the dead holder's late effect to land.
	landed := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := ownerB.Run(ctx, "docker inspect -f '{{.State.Status}}' "+ssh.ShellQuote(lateName)+" 2>/dev/null || true")
		if err == nil && strings.TrimSpace(out) == "running" {
			landed = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !landed {
		t.Fatal("the delayed effect never landed — fix the fixture before trusting this scenario")
	}

	// The production reconciliation over the observed world: the late
	// container is neither the new deploy's candidate naming nor a known
	// predecessor's — MANUAL refusal, never a blind deploy.
	d := NewDeployer(ownerB, os.Stderr)
	err = d.ReconcileAfterTakeover(ctx, Config{App: app, Version: "newgen"}, nil)
	if err == nil {
		t.Fatal("the production reconciler proceeded on a world with an unattributable running container — quiescence assumption, the C01-1 defect")
	}
	if !strings.Contains(err.Error(), "MANUAL") {
		t.Fatalf("expected MANUAL disposition, got: %v", err)
	}
	t.Logf("refused as expected: %v", err)
}
