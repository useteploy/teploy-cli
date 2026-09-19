package caddy

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// fakeStatefulExecutor is a minimal ssh.Executor with a REAL file map, so
// `test -f` reflects prior uploads the way the stateless MockExecutor
// cannot (TCL-59: prefix-response doubles cannot model filesystem
// semantics). Only the commands caddy.Client issues are implemented.
type fakeStatefulExecutor struct {
	mu       sync.Mutex
	files    map[string][]byte
	adaptErr error // when set, the server-side adapt gate refuses
}

func newFakeStatefulExecutor(initial map[string]string) *fakeStatefulExecutor {
	f := &fakeStatefulExecutor{files: map[string][]byte{}}
	for k, v := range initial {
		f.files[k] = []byte(v)
	}
	return f
}

func (f *fakeStatefulExecutor) Run(ctx context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case cmd == "cat "+caddyfilePath:
		data, ok := f.files[caddyfilePath]
		if !ok {
			return "", fmt.Errorf("no such file")
		}
		return string(data), nil
	case strings.HasPrefix(cmd, "test -f "):
		path := strings.Trim(strings.TrimPrefix(cmd, "test -f "), "'")
		if _, ok := f.files[path]; !ok {
			return "", fmt.Errorf("not found")
		}
		return "", nil
	case strings.HasPrefix(cmd, "a=$(docker exec caddy md5sum"):
		return deliveredOK, nil
	case strings.HasPrefix(cmd, "docker exec -i caddy caddy adapt"):
		// The pre-write adapt gate: the fake has no caddy; it accepts
		// unless the test stages a refusal.
		if f.adaptErr != nil {
			return "", f.adaptErr
		}
		return "", nil
	case strings.HasPrefix(cmd, "mkdir "+lockDir), strings.HasPrefix(cmd, "rmdir "+lockDir):
		return "", nil
	case cmd == reloadCmd:
		return "", nil
	case strings.HasPrefix(cmd, "rm -f "):
		delete(f.files, strings.Trim(strings.TrimPrefix(cmd, "rm -f "), "'"))
		return "", nil
	case strings.HasPrefix(cmd, "mv -f -- "):
		fields := strings.Fields(strings.TrimPrefix(cmd, "mv -f -- "))
		if len(fields) == 2 {
			src := strings.Trim(fields[0], "'")
			dst := strings.Trim(fields[1], "'")
			if data, ok := f.files[src]; ok {
				delete(f.files, src)
				f.files[dst] = data
			}
		}
		return "", nil
	}
	return "", fmt.Errorf("fake: unexpected command: %s", cmd)
}

func (f *fakeStatefulExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	_, err := f.Run(ctx, cmd)
	return err
}

func (f *fakeStatefulExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	_, err := f.Run(ctx, cmd)
	return err
}

func (f *fakeStatefulExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.files[remotePath] = data
	return nil
}

func (f *fakeStatefulExecutor) Close() error { return nil }
func (f *fakeStatefulExecutor) Host() string { return "9.9.9.9" }
func (f *fakeStatefulExecutor) User() string { return "root" }

func (f *fakeStatefulExecutor) file(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.files[p])
}

// TCL-20: the load-balancer renderer must emit the CONFIGURED health path,
// not a hardcoded /up — an app that passes readiness on its real endpoint
// then had every upstream marked unhealthy by Caddy.
func TestSetLoadBalancerHealth_RendersConfiguredPath(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", lockCmds("{\n\tadmin 127.0.0.1:2019\n}\n")...)
	client := NewClient(mock)
	err := client.SetLoadBalancerHealth(context.Background(), "myapp", "myapp.com",
		[]Upstream{{Dial: "a:80"}, {Dial: "b:80"}}, "/ready?service=web", TLS{}, "", nil, Firewall{}, Access{})
	if err != nil {
		t.Fatalf("SetLoadBalancerHealth: %v", err)
	}
	written := string(mock.Files[caddyfilePath])
	if !strings.Contains(written, "health_uri /ready?service=web") {
		t.Errorf("configured health path not rendered:\n%s", written)
	}
	if strings.Contains(written, "health_uri /up") {
		t.Errorf("hardcoded /up still rendered:\n%s", written)
	}

	if err := client.SetLoadBalancerHealth(context.Background(), "myapp", "myapp.com",
		[]Upstream{{Dial: "a:80"}}, "not-a-uri\ninject", TLS{}, "", nil, Firewall{}, Access{}); err == nil {
		t.Error("invalid health path accepted")
	}
}

// TCL-25: repeated SetMaintenance must not overwrite the stashed original
// route with the maintenance block — maintenance-off would then restore
// maintenance forever. The first stash wins.
func TestSetMaintenance_IdempotentStash(t *testing.T) {
	const existing = "# TEPLOY BEGIN myapp\nmyapp.com {\n\treverse_proxy myapp-web-1:3000\n}\n# TEPLOY END myapp\n"
	exec := newFakeStatefulExecutor(map[string]string{caddyfilePath: existing})
	client := NewClient(exec)

	if err := client.SetMaintenance(context.Background(), "myapp", "myapp.com"); err != nil {
		t.Fatalf("first SetMaintenance: %v", err)
	}
	stash := fmt.Sprintf(maintStashFmt, "myapp")
	first := exec.file(stash)
	if !strings.Contains(first, "reverse_proxy myapp-web-1:3000") {
		t.Fatalf("first stash did not capture the original route:\n%s", first)
	}

	if err := client.SetMaintenance(context.Background(), "myapp", "myapp.com"); err != nil {
		t.Fatalf("second SetMaintenance: %v", err)
	}
	if second := exec.file(stash); second != first {
		t.Errorf("second maintenance-on overwrote the stashed original route\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TCL-23: updating or removing app "web" must never delete the managed
// block of a DIFFERENT app that happens to be named lb-web. The legacy
// lb-<app> block is migrated only when its site address proves it serves
// this app's hosts.
func TestRenderUpdated_LegacyLBBlockCollision(t *testing.T) {
	const prev = "# TEPLOY BEGIN web\nweb.com {\n\trespond \"web\"\n}\n# TEPLOY END web\n" +
		"# TEPLOY BEGIN lb-web\nlb-web.com {\n\trespond \"lb-web\"\n}\n# TEPLOY END lb-web\n"

	updated, err := renderUpdated(prev, "web", []string{"web.com"}, reverseProxyBlock([]string{"web.com"}, "web-1", 3000, TLS{}, "", nil, Firewall{}, Access{}))
	if err != nil {
		t.Fatalf("renderUpdated: %v", err)
	}
	if !strings.Contains(updated, "# TEPLOY BEGIN lb-web") || !strings.Contains(updated, `respond "lb-web"`) {
		t.Errorf("unrelated lb-web app's block was deleted:\n%s", updated)
	}

	// Removal path (hosts == nil): the app's own block determines the
	// reference hosts, so a foreign lb-web block still survives.
	removed, err := renderUpdated(prev, "web", nil, "")
	if err != nil {
		t.Fatalf("renderUpdated (remove): %v", err)
	}
	if !strings.Contains(removed, "# TEPLOY BEGIN lb-web") {
		t.Errorf("removal deleted the foreign lb-web block:\n%s", removed)
	}

	// A genuine legacy block (serving the SAME hosts as the app) is still
	// migrated away.
	const legacy = "# TEPLOY BEGIN web\nweb.com {\n\trespond \"web\"\n}\n# TEPLOY END web\n" +
		"# TEPLOY BEGIN lb-web\nweb.com {\n\trespond \"legacy lb\"\n}\n# TEPLOY END lb-web\n"
	migrated, err := renderUpdated(legacy, "web", []string{"web.com"}, reverseProxyBlock([]string{"web.com"}, "web-1", 3000, TLS{}, "", nil, Firewall{}, Access{}))
	if err != nil {
		t.Fatalf("renderUpdated (legacy): %v", err)
	}
	if strings.Contains(migrated, "legacy lb") {
		t.Errorf("genuine legacy lb block was not migrated:\n%s", migrated)
	}
}

// TCL-21: when delivery verification fails after a successful reload, the
// previous Caddyfile is restored so the on-disk file matches what the
// container actually serves, and the error still surfaces.
func TestSetRoute_VerifyFailureRestoresPreviousConfig(t *testing.T) {
	const prev = "{\n\tadmin 127.0.0.1:2019\n}\n"
	cmds := lockCmds(prev)
	// Delivery verification reports STALE for every check.
	for i := range cmds {
		if strings.HasPrefix(cmds[i].Match, "a=$(docker exec caddy md5sum") {
			cmds[i].Output = deliveredStale
		}
	}
	mock := ssh.NewMockExecutor("1.2.3.4", cmds...)
	client := NewClient(mock)

	err := client.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-1", 3000, TLS{}, "", nil, Firewall{}, Access{})
	if err == nil {
		t.Fatal("stale delivery reported as success")
	}
	final := string(mock.Files[caddyfilePath])
	if final != prev {
		t.Errorf("on-disk Caddyfile not restored to previous content after verify failure:\ngot:  %q\nwant: %q", final, prev)
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Errorf("error does not surface the verification failure: %v", err)
	}
}
