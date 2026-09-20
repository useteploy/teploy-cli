package deploy

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

var errBoom = errors.New("boom")

// deployWithAttemptPruneMocks runs a happy-path deploy of version "newhash"
// with the pin read and retained-version inventory stubbed as specified,
// returning the mock (Calls holds every issued command).
func deployWithAttemptPruneMocks(t *testing.T, pinsStub, inventoryStub ssh.MockCommand) *ssh.MockExecutor {
	t.Helper()
	app := "fency"
	mocks := fenceHappyPathMocks(app)
	mocks = append(mocks,
		pinsStub,
		inventoryStub,
		ssh.MockCommand{Match: "ls -1t /deployments/fency/meta/att", Output: "ancient.0000000000000003"},
		ssh.MockCommand{Match: "ls -1t /deployments/caddy/tls/att/fency", Output: "ancient.0000000000000003"},
		ssh.MockCommand{Match: "rm -rf ", Output: ""},
	)
	mock := ssh.NewMockExecutor("1.2.3.4", mocks...)
	lk, err := state.AcquireLockFenced(context.Background(), mock, app)
	if err != nil {
		t.Fatalf("AcquireLockFenced: %v", err)
	}
	d := NewDeployer(mock, &bytes.Buffer{})
	if err := d.DeployFenced(context.Background(), Config{
		App:     app,
		Domain:  "fency.com",
		Image:   "fency:latest",
		Version: "newhash",
		Health:  HealthConfig{Timeout: 5 * time.Second, Interval: 10 * time.Millisecond},
	}, lk); err != nil {
		t.Fatalf("DeployFenced: %v", err)
	}
	return mock
}

// TestDeployFenced_PinReadFailureSkipsAttemptPrune is the A03 regression:
// an unreadable pin file must fail the attempt-artifact prune closed (like
// version pruning), never prune as though no pins existed.
func TestDeployFenced_PinReadFailureSkipsAttemptPrune(t *testing.T) {
	mock := deployWithAttemptPruneMocks(t,
		// The framing command itself fails (transport/permission), which
		// ReadRemoteFile surfaces as an error.
		ssh.MockCommand{Match: "if [ ! -e '/deployments/fency/pinned' ]", Err: errBoom},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: ""},
	)
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf ") && strings.Contains(c, "/meta/att") {
			t.Errorf("attempt artifacts must not be pruned when pins cannot be read: %s", c)
		}
		if strings.HasPrefix(c, "ls -1t /deployments/fency/meta/att") {
			t.Errorf("the prune sweep must not even run when pins cannot be read: %s", c)
		}
	}
}

// TestDeployFenced_RetainedVersionsProtectTheirAttempts is the A02
// regression: a release that still has containers on the server (e.g. held
// by keep_versions retention or a pin) keeps its attempt artifacts — its
// record and Caddy route reference the attempt-scoped TLS/env files.
func TestDeployFenced_RetainedVersionsProtectTheirAttempts(t *testing.T) {
	mock := deployWithAttemptPruneMocks(t,
		ssh.MockCommand{Match: "if [ ! -e '/deployments/fency/pinned' ]", Output: "absent"},
		// The inventory reports a live container of release "ancient": a
		// retained rollback target outside current+previous.
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Output: `{"ID":"deadbeef","Names":"fency-web-ancient","Image":"fency:latest","State":"running","Status":"up","CreatedAt":"2026-05-28 21:33:29 -0700 PDT","Labels":"teploy.app=fency,teploy.process=web,teploy.version=ancient"}`},
	)
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf ") && strings.Contains(c, "ancient.") {
			t.Errorf("a release with live containers must keep its attempt artifacts: %s", c)
		}
	}
}

// TestDeployFenced_InventoryFailureSkipsAttemptPrune: when the retained-
// version inventory cannot be listed, the protection window cannot be
// computed, so the prune is skipped (fail closed, A02).
func TestDeployFenced_InventoryFailureSkipsAttemptPrune(t *testing.T) {
	mock := deployWithAttemptPruneMocks(t,
		ssh.MockCommand{Match: "if [ ! -e '/deployments/fency/pinned' ]", Output: "absent"},
		ssh.MockCommand{Match: "docker ps --all --filter label=teploy.app='fency'", Err: errBoom},
	)
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "ls -1t /deployments/fency/meta/att") {
			t.Errorf("the prune sweep must not run when the version inventory is unreadable: %s", c)
		}
	}
}
