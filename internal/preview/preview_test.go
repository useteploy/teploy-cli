package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestSanitizeBranch(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"main", "main"},
		{"feature/login", "feature-login"},
		{"Feature_Page", "feature-page"},
		{"fix/bug#123", "fix-bug123"},
		{"---leading---", "leading"},
		{"a/b/c/d", "a-b-c-d"},
		{"UPPERCASE", "uppercase"},
	}

	for _, tt := range tests {
		got := SanitizeBranch(tt.input)
		if got != tt.want {
			t.Errorf("SanitizeBranch(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// The canonical-ID derivation is pinned to exact sha256 output so a silent
// change to the identity scheme (which would orphan every deployed preview
// record) cannot land unnoticed. Values are sha256("<app>\x00<branch>")
// truncated to 8 hex chars.
func TestPreviewIDGolden(t *testing.T) {
	tests := []struct {
		app, branch, want string
	}{
		{"myapp", "feature/login", "myapp-p-08e81639"},
		{"myapp", "feature-login", "myapp-p-cb4bdf9a"},
		{"myapp", "main", "myapp-p-563059ce"},
		{"myapp", "old-feature", "myapp-p-491218d3"},
		{"myapp", "active-feature", "myapp-p-9fdbab8f"},
		{"market-eval", "feature/login", "market-eval-p-a575aaf7"},
		{"market-eval", "feature-login", "market-eval-p-84d280a4"},
	}
	for _, tt := range tests {
		if got := PreviewID(tt.app, tt.branch); got != tt.want {
			t.Errorf("PreviewID(%q, %q) = %q, want %q", tt.app, tt.branch, got, tt.want)
		}
		if got := previewIDHex(tt.app, tt.branch); got != strings.TrimPrefix(tt.want, tt.app+"-p-") {
			t.Errorf("previewIDHex(%q, %q) = %q, want %q", tt.app, tt.branch, got, strings.TrimPrefix(tt.want, tt.app+"-p-"))
		}
		// Deterministic: the same identity inputs always derive the same ID.
		if PreviewID(tt.app, tt.branch) != PreviewID(tt.app, tt.branch) {
			t.Errorf("PreviewID(%q, %q) not deterministic", tt.app, tt.branch)
		}
	}
}

// The C06 defect in one test: feature/login and feature/login's slug twin
// feature-login must never share an identifier anywhere — state path,
// container/process name, route key, or DNS label.
func TestPreviewBranchIdentityIsDistinct(t *testing.T) {
	left, right := "feature/login", "feature-login"
	if previewStatePath("myapp", left) == previewStatePath("myapp", right) {
		t.Errorf("state paths collide: %q", previewStatePath("myapp", left))
	}
	if legacyPreviewStatePath("myapp", left) != legacyPreviewStatePath("myapp", right) {
		t.Errorf("legacy slug paths must collide (that is the defect being migrated): %q vs %q",
			legacyPreviewStatePath("myapp", left), legacyPreviewStatePath("myapp", right))
	}
	if previewDomain("myapp", left, "myapp.com") == previewDomain("myapp", right, "myapp.com") {
		t.Errorf("domains collide: %q", previewDomain("myapp", left, "myapp.com"))
	}
	leftProc := "preview-p-" + previewIDHex("myapp", left)
	rightProc := "preview-p-" + previewIDHex("myapp", right)
	if leftProc == rightProc {
		t.Errorf("container process names collide: %q", leftProc)
	}
	if "myapp-"+leftProc == "myapp-"+rightProc {
		t.Errorf("route keys collide")
	}
}

func TestDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/previews", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/myapp/previews/feature-login.json", Output: "", Err: nil},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p", Output: "80/tcp"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		ssh.MockCommand{Match: "rm -f /tmp/teploy_caddy_config.json", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	err := mgr.Deploy(context.Background(), DeployConfig{
		App:     "myapp",
		Domain:  "myapp.com",
		Branch:  "feature/login",
		Image:   "myapp:latest",
		Version: "abc123",
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	output := buf.String()
	if !bytes.Contains([]byte(output), []byte("preview-feature-login-08e81639.myapp.com")) {
		t.Errorf("expected preview domain in output, got: %s", output)
	}
}

func TestList_Empty(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls", Output: "", Err: nil},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	previews, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(previews) != 0 {
		t.Errorf("expected 0 previews, got %d", len(previews))
	}
}

// TestPrune_OnlyDestroysExpired is the regression test for the piggyback
// fix in internal/cli/preview.go's runPreviewDeploy: since teploy has no
// server-side agent/daemon, ExpiresAt was written at deploy time but
// nothing ever checked it. Prune is now called at the start of every
// `teploy preview deploy` so expired previews for the app get torn down
// (container + Caddy route) before a new one is created. This test proves
// Prune destroys an expired preview and leaves a non-expired one alone.
// The fixtures are legacy slug-keyed records, so it doubles as the
// prune-side legacy-adoption proof: Destroy adopts the expired legacy
// record by full-Branch match and tears down exactly its artifacts.
func TestPrune_OnlyDestroysExpired(t *testing.T) {
	expiredJSON := `{"branch":"old-feature","domain":"preview-old-feature.myapp.com","port":49200,"container":"myapp-preview-old-feature-v1","image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":"2020-01-02T00:00:00Z"}`
	freshJSON := fmt.Sprintf(`{"branch":"active-feature","domain":"preview-active-feature.myapp.com","port":49201,"container":"myapp-preview-active-feature-v2","image":"myapp:v2","created_at":"2020-01-01T00:00:00Z","expires_at":%q}`,
		"2099-01-01T00:00:00Z")

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls /deployments/myapp/previews/*.json",
			Output: "/deployments/myapp/previews/old-feature.json\n/deployments/myapp/previews/active-feature.json"},
		ssh.MockCommand{Match: "cat /deployments/myapp/previews/old-feature.json", Output: expiredJSON},
		ssh.MockCommand{Match: "cat /deployments/myapp/previews/active-feature.json", Output: freshJSON},

		// Destroy(old-feature): docker stop/remove, then RemoveRoute's full
		// Caddyfile-editing sequence (see TestDeploy for why this many
		// steps are needed), then rm the state file.
		ssh.MockCommand{Match: "docker stop -t 5 'myapp-preview-old-feature-v1'", Output: ""},
		ssh.MockCommand{Match: "docker rm 'myapp-preview-old-feature-v1'", Output: ""},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "rm -f -- /deployments/myapp/previews/old-feature.json", Output: ""},
	)

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	pruned, err := mgr.Prune(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("expected 1 pruned preview, got %d", pruned)
	}

	// The non-expired preview must never have been touched.
	for _, call := range mock.Calls {
		if strings.Contains(call, "active-feature-v2") {
			t.Errorf("non-expired preview should not be touched, but got call: %s", call)
		}
	}
}

func TestPreviewDomain(t *testing.T) {
	tests := []struct {
		app, branch, domain, want string
	}{
		{"myapp", "feature/login", "myapp.com", "preview-feature-login-08e81639.myapp.com"},
		{"myapp", "main", "example.com", "preview-main-563059ce.example.com"},
		// A 70-char slug must truncate so the whole DNS label stays <= 63:
		// "preview-" (8) + 46 chars + "-" + 8 hex = 63.
		{"myapp", strings.Repeat("a", 70) + "/x", "myapp.com", "preview-" + strings.Repeat("a", 46) + "-54119e3a.myapp.com"},
	}
	for _, tt := range tests {
		got := previewDomain(tt.app, tt.branch, tt.domain)
		if got != tt.want {
			t.Errorf("previewDomain(%q, %q, %q) = %q, want %q", tt.app, tt.branch, tt.domain, got, tt.want)
		}
		label := strings.SplitN(got, ".", 2)[0]
		if len(label) > 63 {
			t.Errorf("DNS label %q exceeds 63 chars (%d)", label, len(label))
		}
	}
}

// previewDeployMocks is the mock bundle for a full Deploy against a bare
// server: port allocation, container start, the candidate health probe
// (blue/green readiness gate), and the Caddyfile edit/reload/verify
// transaction (see TestDeploy for the origins of each entry). State-file
// and Caddyfile writes go through the mock's file state, so successive
// deploys observe each other's records and routes.
func previewDeployMocks() []ssh.MockCommand {
	return []ssh.MockCommand{
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/previews", Output: ""},
		ssh.MockCommand{Match: "ss -tln", Output: ""},
		ssh.MockCommand{Match: "docker run", Output: "abc123"},
		ssh.MockCommand{Match: "docker inspect -f '{{range $p", Output: "80/tcp"},
		ssh.MockCommand{Match: "curl -s -o /dev/null", Output: "200"},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
	}
}

const (
	loginIDHex  = "08e81639" // previewIDHex("myapp", "feature/login")
	dashIDHex   = "cb4bdf9a" // previewIDHex("myapp", "feature-login")
	loginBranch = "feature/login"
	dashBranch  = "feature-login"
)

func deployCfg(branch, version string) DeployConfig {
	return DeployConfig{
		App:     "myapp",
		Domain:  "myapp.com",
		Branch:  branch,
		Image:   "myapp:" + version,
		Version: version,
		Repo:    "github.com/tyler/myapp",
	}
}

func mustDeploy(t *testing.T, mgr *Manager, cfg DeployConfig) {
	t.Helper()
	if err := mgr.Deploy(context.Background(), cfg); err != nil {
		t.Fatalf("Deploy(%q): %v", cfg.Branch, err)
	}
}

func callsContaining(mock *ssh.MockExecutor, needle string) []string {
	var found []string
	for _, c := range mock.Calls {
		if strings.Contains(c, needle) {
			found = append(found, c)
		}
	}
	return found
}

// The C06 coexistence contract: two branches whose sanitized slugs collide
// deploy side by side with distinct state records, containers, routes and
// domains — neither deploy destroys or blocks the other.
func TestDeployCoexistence(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	mustDeploy(t, mgr, deployCfg(dashBranch, "v1"))

	loginPath := previewStatePath("myapp", loginBranch)
	dashPath := previewStatePath("myapp", dashBranch)
	if loginPath == dashPath {
		t.Fatalf("state paths collide: %q", loginPath)
	}
	for _, path := range []string{loginPath, dashPath} {
		if _, ok := mock.Files[path]; !ok {
			t.Errorf("expected state record at %s", path)
		}
	}

	// Distinct containers were started; neither deploy tore anything down
	// (there was nothing to destroy), proving the second deploy did not
	// resolve to the first preview's identity.
	if got := callsContaining(mock, "docker run"); len(got) != 2 {
		t.Fatalf("expected 2 container starts, got %d: %v", len(got), got)
	}
	if len(callsContaining(mock, "--name 'myapp-preview-p-"+loginIDHex+"-v1'")) != 1 {
		t.Errorf("missing container name myapp-preview-p-%s-v1 in: %v", loginIDHex, callsContaining(mock, "docker run"))
	}
	if len(callsContaining(mock, "--name 'myapp-preview-p-"+dashIDHex+"-v1'")) != 1 {
		t.Errorf("missing container name myapp-preview-p-%s-v1 in: %v", dashIDHex, callsContaining(mock, "docker run"))
	}
	if stops := callsContaining(mock, "docker stop"); len(stops) != 0 {
		t.Errorf("fresh deploys must not stop containers, got: %v", stops)
	}

	// Both records carry their full branch identity and distinct routes.
	var loginState, dashState State
	if err := json.Unmarshal(mock.Files[loginPath], &loginState); err != nil {
		t.Fatalf("login record: %v", err)
	}
	if err := json.Unmarshal(mock.Files[dashPath], &dashState); err != nil {
		t.Fatalf("dash record: %v", err)
	}
	if loginState.Branch != loginBranch || dashState.Branch != dashBranch {
		t.Errorf("full branch identity not preserved: %+v / %+v", loginState, dashState)
	}
	if loginState.Route == dashState.Route {
		t.Errorf("route keys collide: %q", loginState.Route)
	}

	// Both routes and domains are distinct and live.
	caddy := string(mock.Files["/deployments/caddy/Caddyfile"])
	for _, key := range []string{"myapp-preview-p-" + loginIDHex, "myapp-preview-p-" + dashIDHex} {
		if !strings.Contains(caddy, key) {
			t.Errorf("Caddyfile missing route key %s", key)
		}
	}
	out := buf.String()
	for _, domain := range []string{
		"preview-feature-login-" + loginIDHex + ".myapp.com",
		"preview-feature-login-" + dashIDHex + ".myapp.com",
	} {
		if !strings.Contains(out, domain) {
			t.Errorf("output missing distinct domain %s", domain)
		}
	}
}

// Updating one preview of a colliding pair must not touch the other's
// record or container.
func TestDeployUpdateOneLeavesOther(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	mustDeploy(t, mgr, deployCfg(dashBranch, "v1"))
	dashPath := previewStatePath("myapp", dashBranch)
	dashBefore := string(mock.Files[dashPath])

	mustDeploy(t, mgr, deployCfg(loginBranch, "v2"))

	// The redeploy replaced only its own generation.
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-p-"+loginIDHex+"-v1'")) != 1 {
		t.Errorf("expected the login v1 container to be stopped, calls: %v", callsContaining(mock, "docker stop"))
	}
	if stops := callsContaining(mock, "docker stop"); len(stops) != 1 {
		t.Errorf("redeploy must stop exactly one container, got: %v", stops)
	}
	if got := string(mock.Files[dashPath]); got != dashBefore {
		t.Errorf("the colliding branch's record was modified:\nbefore: %s\nafter:  %s", dashBefore, got)
	}
}

// Destroying one preview of a colliding pair leaves the other fully
// intact.
func TestDestroyOneLeavesOther(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	mustDeploy(t, mgr, deployCfg(dashBranch, "v1"))
	dashPath := previewStatePath("myapp", dashBranch)
	dashBefore := string(mock.Files[dashPath])

	if err := mgr.Destroy(context.Background(), "myapp", loginBranch); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if _, ok := mock.Files[previewStatePath("myapp", loginBranch)]; ok {
		t.Errorf("destroyed preview's record still present")
	}
	if got := string(mock.Files[dashPath]); got != dashBefore {
		t.Errorf("the colliding branch's record was modified:\nbefore: %s\nafter:  %s", dashBefore, got)
	}
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-p-"+dashIDHex+"-v1'")) != 0 {
		t.Errorf("the colliding branch's container must not be stopped, calls: %v", callsContaining(mock, "docker stop"))
	}
	// Its route survives too: only the login route key was removed.
	caddy := string(mock.Files["/deployments/caddy/Caddyfile"])
	if strings.Contains(caddy, "myapp-preview-p-"+loginIDHex) {
		t.Errorf("destroyed preview's route still present in Caddyfile")
	}
	if !strings.Contains(caddy, "myapp-preview-p-"+dashIDHex) {
		t.Errorf("surviving preview's route missing from Caddyfile")
	}
}

// Prune expires one canonical preview of a colliding pair and leaves the
// other running.
func TestPruneExpiresOneLeavesOther(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{
			ssh.MockCommand{Match: "ls /deployments/myapp/previews/*.json",
				Output: previewStatePath("myapp", loginBranch) + "\n" + previewStatePath("myapp", dashBranch)},
		}, previewDeployMocks()...)...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	mustDeploy(t, mgr, deployCfg(dashBranch, "v1"))
	dashPath := previewStatePath("myapp", dashBranch)
	dashBefore := string(mock.Files[dashPath])

	// Expire the login preview by rewriting its stored record.
	loginPath := previewStatePath("myapp", loginBranch)
	var s State
	if err := json.Unmarshal(mock.Files[loginPath], &s); err != nil {
		t.Fatalf("login record: %v", err)
	}
	s.ExpiresAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	expired, _ := json.Marshal(s)
	mock.Files[loginPath] = expired

	pruned, err := mgr.Prune(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected 1 pruned preview, got %d", pruned)
	}
	if _, ok := mock.Files[loginPath]; ok {
		t.Errorf("expired preview's record still present")
	}
	if got := string(mock.Files[dashPath]); got != dashBefore {
		t.Errorf("the colliding branch's record was modified:\nbefore: %s\nafter:  %s", dashBefore, got)
	}
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-p-"+dashIDHex+"-v1'")) != 0 {
		t.Errorf("the colliding branch's container must not be stopped, calls: %v", callsContaining(mock, "docker stop"))
	}
}

// legacyRecordJSON is a record exactly as the pre-canonical-ID writer
// emitted it: slug-keyed file, full Branch, no ID/Repo/Route fields.
func legacyRecordJSON(branch, container string) string {
	return fmt.Sprintf(`{"branch":%q,"domain":"preview-%s.myapp.com","port":49200,"container":%q,"image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":"2099-01-01T00:00:00Z"}`,
		branch, SanitizeBranch(branch), container)
}

// seedCaddyfileWithRoute seeds the Caddyfile file state with a managed
// block for an app key, mimicking what an earlier teploy version left on
// the server.
func seedCaddyfileWithRoute(mock *ssh.MockExecutor, key, host string) {
	mock.Files["/deployments/caddy/Caddyfile"] = []byte(fmt.Sprintf(
		"{\n\tadmin 0.0.0.0:2019\n}\n\n# TEPLOY BEGIN %s\n%s {\n\treverse_proxy %s:80\n}\n# TEPLOY END %s\n",
		key, host, key, key))
}

// A legacy slug-keyed record whose stored full Branch matches is adopted:
// Deploy migrates it under the canonical key (identity preserved), tears
// down the artifacts the record actually names (the old slug-keyed
// container and route), and deploys fresh canonical-keyed artifacts.
func TestLegacyAdoptionDeploy(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	legacyPath := legacyPreviewStatePath("myapp", loginBranch) // .../feature-login.json
	mock.Files[legacyPath] = []byte(legacyRecordJSON(loginBranch, "myapp-preview-feature-login-v1"))
	seedCaddyfileWithRoute(mock, "myapp-preview-feature-login", "preview-feature-login.myapp.com")

	mustDeploy(t, mgr, deployCfg(loginBranch, "v2"))

	// The legacy file is gone; the canonical record carries the FULL
	// branch identity, the canonical ID, the repo provenance, and the new
	// route key.
	if _, ok := mock.Files[legacyPath]; ok {
		t.Errorf("legacy record was not migrated away from %s", legacyPath)
	}
	canonPath := previewStatePath("myapp", loginBranch)
	data, ok := mock.Files[canonPath]
	if !ok {
		t.Fatalf("canonical record missing at %s", canonPath)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("canonical record: %v", err)
	}
	if s.Branch != loginBranch {
		t.Errorf("full branch identity lost in adoption: %+v", s)
	}
	if s.ID != "myapp-p-"+loginIDHex {
		t.Errorf("canonical ID missing/wrong: %+v", s)
	}
	if s.Repo != "github.com/tyler/myapp" {
		t.Errorf("repo provenance not recorded: %+v", s)
	}
	if s.Route != "myapp-preview-p-"+loginIDHex {
		t.Errorf("route key not recorded: %+v", s)
	}

	// The legacy record's OWN artifacts were torn down: its container and
	// its slug-era route.
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-feature-login-v1'")) != 1 {
		t.Errorf("legacy container not stopped via its stored name, calls: %v", callsContaining(mock, "docker stop"))
	}
	caddy := string(mock.Files["/deployments/caddy/Caddyfile"])
	if strings.Contains(caddy, "myapp-preview-feature-login") {
		t.Errorf("legacy slug-keyed route not removed:\n%s", caddy)
	}
	if !strings.Contains(caddy, "myapp-preview-p-"+loginIDHex) {
		t.Errorf("new canonical route missing:\n%s", caddy)
	}
}

// The collision case: a legacy record at the shared slug path belongs to a
// DIFFERENT branch. Deploy must refuse with an ambiguous-resource error
// naming both branches and the record path, and must not mutate anything.
func TestLegacyCollisionDeployIsAmbiguous(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	legacyPath := legacyPreviewStatePath("myapp", loginBranch) // shared slug: feature-login.json
	legacy := []byte(legacyRecordJSON(dashBranch, "myapp-preview-feature-login-v9"))
	mock.Files[legacyPath] = legacy
	seedCaddyfileWithRoute(mock, "myapp-preview-feature-login", "preview-feature-login.myapp.com")

	err := mgr.Deploy(context.Background(), deployCfg(loginBranch, "v1"))
	var amb *AmbiguousPreviewError
	if !errors.As(err, &amb) {
		t.Fatalf("expected *AmbiguousPreviewError, got %v", err)
	}
	if amb.StoredBranch != dashBranch || amb.RequestedBranch != loginBranch {
		t.Errorf("error must name both branches, got %+v", amb)
	}
	if amb.Path != legacyPath {
		t.Errorf("error must name the record path %s, got %s", legacyPath, amb.Path)
	}
	for _, want := range []string{dashBranch, loginBranch, legacyPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text must mention %q: %v", want, err)
		}
	}

	// Nothing was mutated: record intact, no container touched, no new
	// record, no route edit, not even the preview directory.
	if got := string(mock.Files[legacyPath]); got != string(legacy) {
		t.Errorf("ambiguous legacy record was mutated:\nbefore: %s\nafter:  %s", legacy, got)
	}
	if calls := mock.Calls; len(calls) > 2 { // the two record reads only
		t.Errorf("ambiguous legacy record must abort before any mutation, calls: %v", calls)
	}
}

// Destroy hits the same ambiguity wall: it must refuse, not guess, and
// leave the record intact.
func TestLegacyCollisionDestroyIsAmbiguous(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	legacyPath := legacyPreviewStatePath("myapp", loginBranch)
	legacy := []byte(legacyRecordJSON(dashBranch, "myapp-preview-feature-login-v9"))
	mock.Files[legacyPath] = legacy

	err := mgr.Destroy(context.Background(), "myapp", loginBranch)
	var amb *AmbiguousPreviewError
	if !errors.As(err, &amb) {
		t.Fatalf("expected *AmbiguousPreviewError, got %v", err)
	}
	if got := string(mock.Files[legacyPath]); got != string(legacy) {
		t.Errorf("ambiguous legacy record was mutated:\nbefore: %s\nafter:  %s", legacy, got)
	}
	if stops := callsContaining(mock, "docker"); len(stops) != 0 {
		t.Errorf("no docker command may run against an ambiguous record, calls: %v", stops)
	}
	if rms := callsContaining(mock, "rm -f"); len(rms) != 0 {
		t.Errorf("no file may be removed against an ambiguous record, calls: %v", rms)
	}
}

// Repo provenance participates in legacy adoption when both sides record
// one: same branch, different repo → ambiguous, untouched.
func TestLegacyRepoMismatchIsAmbiguous(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	legacyPath := legacyPreviewStatePath("myapp", loginBranch)
	legacy := []byte(`{"branch":"feature/login","repo":"github.com/someone/clone","domain":"preview-feature-login.myapp.com","port":49200,"container":"myapp-preview-feature-login-v1","image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":"2099-01-01T00:00:00Z"}`)
	mock.Files[legacyPath] = legacy

	cfg := deployCfg(loginBranch, "v1") // Repo github.com/tyler/myapp
	err := mgr.Deploy(context.Background(), cfg)
	var amb *AmbiguousPreviewError
	if !errors.As(err, &amb) {
		t.Fatalf("expected *AmbiguousPreviewError, got %v", err)
	}
	if !strings.Contains(err.Error(), "github.com/someone/clone") {
		t.Errorf("error must name the stored repo: %v", err)
	}
	if got := string(mock.Files[legacyPath]); got != string(legacy) {
		t.Errorf("ambiguous legacy record was mutated")
	}
}

// A legacy record for the OTHER colliding branch must not block work on
// this branch once this branch has its own canonical record: the legacy
// file belongs to that branch and is left in place untouched.
func TestLegacyOtherBranchNotBlocked(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4", previewDeployMocks()...)
	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)

	// This branch's canonical record exists (a modern deploy happened).
	mustDeploy(t, mgr, deployCfg(loginBranch, "v1"))
	// And a legacy-era preview of the colliding branch is still around.
	legacyPath := legacyPreviewStatePath("myapp", loginBranch) // feature-login.json
	legacy := []byte(legacyRecordJSON(dashBranch, "myapp-preview-feature-login-v9"))
	mock.Files[legacyPath] = legacy

	// Redeploying feature/login must succeed and not touch the other
	// branch's legacy record or container.
	mustDeploy(t, mgr, deployCfg(loginBranch, "v2"))
	if got := string(mock.Files[legacyPath]); got != string(legacy) {
		t.Errorf("the other branch's legacy record was mutated:\nbefore: %s\nafter:  %s", legacy, got)
	}
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-feature-login-v9'")) != 0 {
		t.Errorf("the other branch's container must not be stopped: %v", callsContaining(mock, "docker stop"))
	}

	// And destroying THAT branch still finds and removes its legacy record.
	if err := mgr.Destroy(context.Background(), "myapp", dashBranch); err != nil {
		t.Fatalf("Destroy of the legacy branch: %v", err)
	}
	if _, ok := mock.Files[legacyPath]; ok {
		t.Errorf("legacy record for %s not removed by its own destroy", dashBranch)
	}
	if len(callsContaining(mock, "docker stop -t 5 'myapp-preview-feature-login-v9'")) != 1 {
		t.Errorf("legacy branch's container not stopped via its stored name: %v", callsContaining(mock, "docker stop"))
	}
}

// List surfaces records from both eras without mutating anything.
func TestListIncludesLegacyAndCanonical(t *testing.T) {
	loginPath := previewStatePath("myapp", loginBranch)
	legacyPath := legacyPreviewStatePath("myapp", dashBranch)
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "ls /deployments/myapp/previews/*.json",
			Output: loginPath + "\n" + legacyPath},
	)
	mock.Files[loginPath] = []byte(`{"id":"myapp-p-` + loginIDHex + `","branch":"feature/login","route":"myapp-preview-p-` + loginIDHex + `","domain":"preview-feature-login-` + loginIDHex + `.myapp.com","port":49200,"container":"myapp-preview-p-` + loginIDHex + `-v1","image":"myapp:v1","created_at":"2020-01-01T00:00:00Z","expires_at":"2099-01-01T00:00:00Z"}`)
	mock.Files[legacyPath] = []byte(legacyRecordJSON(dashBranch, "myapp-preview-feature-login-v9"))

	var buf bytes.Buffer
	mgr := NewManager(mock, &buf)
	previews, err := mgr.List(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(previews) != 2 {
		t.Fatalf("expected 2 previews, got %d: %+v", len(previews), previews)
	}
	byBranch := map[string]State{}
	for _, p := range previews {
		byBranch[p.Branch] = p
	}
	if byBranch[loginBranch].ID != "myapp-p-"+loginIDHex {
		t.Errorf("canonical record lost its ID: %+v", byBranch[loginBranch])
	}
	if byBranch[dashBranch].ID != "" {
		t.Errorf("legacy record must be listed unmodified (no invented ID): %+v", byBranch[dashBranch])
	}
}
