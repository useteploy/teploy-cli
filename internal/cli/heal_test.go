package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

const healConfJSON = `{"processes":["web"],"max_attempts":5,"backoff_seconds":60,"health_path":"/health"}`

func calledRestart(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "docker restart") {
			return true
		}
	}
	return false
}

// unhealthy web container → heal restarts it in place.
func TestRunHealPass_RestartsUnhealthy(t *testing.T) {
	mock := ssh.NewMockExecutor("host",
		ssh.MockCommand{Match: "cat '/deployments/app/heal.conf'", Output: healConfJSON},
		ssh.MockCommand{Match: "cat '/deployments/app/.heal-state'", Output: ""},
		ssh.MockCommand{Match: "docker ps", Output: psLine("app-web-v1", "running", "teploy.app=app,teploy.process=web,teploy.version=v1")},
		ssh.MockCommand{Match: "docker inspect", Output: "18080 "},
		ssh.MockCommand{Match: "curl", Output: "000"}, // unhealthy
		ssh.MockCommand{Match: "mkdir /deployments/app/.lock", Output: ""},
		ssh.MockCommand{Match: "docker restart", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
	)
	n, err := runHealPass(context.Background(), mock, "app", io.Discard)
	if err != nil {
		t.Fatalf("runHealPass: %v", err)
	}
	if n != 1 {
		t.Fatalf("restarted = %d, want 1", n)
	}
	if !calledRestart(mock.Calls) {
		t.Fatal("expected a docker restart of the unhealthy container")
	}
}

// healthy web container → heal leaves it alone.
func TestRunHealPass_LeavesHealthyAlone(t *testing.T) {
	mock := ssh.NewMockExecutor("host",
		ssh.MockCommand{Match: "cat '/deployments/app/heal.conf'", Output: healConfJSON},
		ssh.MockCommand{Match: "cat '/deployments/app/.heal-state'", Output: ""},
		ssh.MockCommand{Match: "docker ps", Output: psLine("app-web-v1", "running", "teploy.app=app,teploy.process=web,teploy.version=v1")},
		ssh.MockCommand{Match: "docker inspect", Output: "18080 "},
		ssh.MockCommand{Match: "curl", Output: "200"}, // healthy
	)
	n, err := runHealPass(context.Background(), mock, "app", io.Discard)
	if err != nil {
		t.Fatalf("runHealPass: %v", err)
	}
	if n != 0 || calledRestart(mock.Calls) {
		t.Fatalf("healthy container must not be restarted (n=%d)", n)
	}
}

// A held deploy lock → heal yields, does NOT restart even though unhealthy.
func TestRunHealPass_YieldsToDeployLock(t *testing.T) {
	mock := ssh.NewMockExecutor("host",
		ssh.MockCommand{Match: "cat '/deployments/app/heal.conf'", Output: healConfJSON},
		ssh.MockCommand{Match: "cat '/deployments/app/.heal-state'", Output: ""},
		ssh.MockCommand{Match: "docker ps", Output: psLine("app-web-v1", "running", "teploy.app=app,teploy.process=web,teploy.version=v1")},
		ssh.MockCommand{Match: "docker inspect", Output: "18080 "},
		ssh.MockCommand{Match: "curl", Output: "000"}, // unhealthy
		// A deploy holds the lock: mkdir fails, and the info says "manual".
		ssh.MockCommand{Match: "mkdir /deployments/app/.lock", Err: fmt.Errorf("exists")},
		ssh.MockCommand{Match: "cat /deployments/app/.lock/info",
			Output: fmt.Sprintf(`{"type":"manual","ts":%q}`, time.Now().UTC().Format(time.RFC3339))},
	)
	n, err := runHealPass(context.Background(), mock, "app", io.Discard)
	if err != nil {
		t.Fatalf("runHealPass: %v", err)
	}
	if n != 0 || calledRestart(mock.Calls) {
		t.Fatal("heal must yield to a deploy lock — no restart")
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "rm -rf") {
			t.Fatal("heal must not break the deploy lock")
		}
	}
}

// accessories and previews (share teploy.app) are never healed.
func TestRunHealPass_SkipsAccessoriesAndPreviews(t *testing.T) {
	mock := ssh.NewMockExecutor("host",
		ssh.MockCommand{Match: "cat '/deployments/app/heal.conf'", Output: healConfJSON},
		ssh.MockCommand{Match: "cat '/deployments/app/.heal-state'", Output: ""},
		ssh.MockCommand{Match: "docker ps", Output: psLine("app-db", "running", "teploy.app=app,teploy.role=accessory") +
			"\n" + psLine("app-preview-x", "running", "teploy.app=app,teploy.process=preview-x")},
		ssh.MockCommand{Match: "curl", Output: "000"},
	)
	n, err := runHealPass(context.Background(), mock, "app", io.Discard)
	if err != nil {
		t.Fatalf("runHealPass: %v", err)
	}
	if n != 0 || calledRestart(mock.Calls) {
		t.Fatal("heal must not touch accessories or previews")
	}
}
