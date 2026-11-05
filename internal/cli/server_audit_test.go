package cli

// `teploy server audit` tests. Every fixture models one server through the
// ssh MockExecutor the way domains_test.go does: the responses are synthetic,
// every parse and judgement stage is the real one. The read-only pin at the
// bottom is the audit's safety property: no command it issues may mutate.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

var auditFixtureNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// auditFixture builds the mock executor for one audited server. Zero values
// mean "the read succeeds and reports nothing" wherever that is a shape the
// real server can produce (no dangling images, no dangling volumes); a read
// that must FAIL is registered explicitly through extra.
type auditFixture struct {
	server, host string
	now          time.Time

	apps  []string          // /deployments/*/state.json listing
	state map[string]string // app -> state JSON; "!" means unreadable
	badRC map[string]bool   // apps whose state read fails (S rc=1)

	caddyfile string // "" = no Caddyfile on the server

	containers string // docker ps --all --format output
	sizes      string // docker ps --all --size output
	resolves   string // getent answers inside caddy, "rc name" lines
	roots      map[string]string
	addrs      string // hostname -I
	usage      string // docker system df rows
	danglingIm string
	danglingVo string
	df         string
	du         string
	scheduler  string

	files map[string][]byte
	extra []ssh.MockCommand // registered first: explicit wins
}

func auditFrames(fx auditFixture) string {
	var b strings.Builder
	for _, app := range fx.apps {
		st := fx.state[app]
		rc := "0"
		if fx.badRC[app] {
			rc = "1"
		}
		fmt.Fprintf(&b, "A %s\nS %s %s\nG absent\nL \nM 0\nR 0\n", app, rc,
			base64.StdEncoding.EncodeToString([]byte(st)))
	}
	return b.String()
}

func (fx auditFixture) exec() *ssh.MockExecutor {
	cmds := append([]ssh.MockCommand{}, fx.extra...)
	if fx.apps != nil {
		cmds = append(cmds, ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: strings.Join(fx.apps, "\n") + "\n"})
		cmds = append(cmds, ssh.MockCommand{Match: "for app in", Output: auditFrames(fx)})
	}
	// Registered before the plain `docker ps --all` listing so the longer
	// (size-carrying) command is matched first; an empty output means "the
	// read succeeded and reported nothing".
	cmds = append(cmds, ssh.MockCommand{Match: "t=; command -v timeout >/dev/null 2>&1 && t='timeout 120'; $t docker ps --all --size", Output: fx.sizes})
	if fx.containers != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "docker ps --all ", Output: fx.containers})
	}
	if fx.resolves != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "printf '%s\\n'", Output: fx.resolves})
	}
	for root, verdict := range fx.roots {
		cmds = append(cmds, ssh.MockCommand{Match: "if [ -e " + ssh.ShellQuote(root) + " ]", Output: verdict})
	}
	if fx.addrs != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "hostname -I", Output: fx.addrs})
	}
	if fx.usage != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "docker system df", Output: fx.usage})
	}
	cmds = append(cmds, ssh.MockCommand{Match: "docker images -q -f dangling=true", Output: fx.danglingIm})
	cmds = append(cmds, ssh.MockCommand{Match: "docker volume ls -q -f dangling=true", Output: fx.danglingVo})
	if fx.df != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "df -B1 -P /", Output: fx.df})
	}
	if fx.du != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "t=; command -v timeout >/dev/null 2>&1 && t='timeout 60'; $t du -sxb", Output: fx.du})
	}
	if fx.scheduler != "" {
		cmds = append(cmds, ssh.MockCommand{Match: "sched_daemon=no; ", Output: fx.scheduler})
	}
	m := ssh.NewMockExecutor(fx.host, cmds...)
	for p, data := range fx.files {
		m.Files[p] = data
	}
	if fx.caddyfile != "" {
		m.Files["/deployments/caddy/Caddyfile"] = []byte(fx.caddyfile)
	}
	return m
}

func (fx auditFixture) audit(t *testing.T) serverAuditEnvelope {
	t.Helper()
	if fx.server == "" {
		fx.server = "edge"
	}
	if fx.host == "" {
		fx.host = "192.0.2.10"
	}
	if fx.now.IsZero() {
		fx.now = auditFixtureNow
	}
	return auditServer(context.Background(), fx.exec(), fx.server, fx.host, fx.now)
}

// auditStateJSON builds a state.json the canonical parser accepts.
func auditStateJSON(deployment, ingress, current, previous string, manifest map[string]any) string {
	m := map[string]any{
		"schema_version":  2,
		"deployment_type": deployment,
		"ingress_mode":    ingress,
		"updated_at":      "2026-11-01T00:00:00Z",
		"current_hash":    current,
		"generation":      2,
	}
	if previous != "" {
		m["previous_hash"] = previous
	}
	if manifest != nil {
		raw, _ := json.Marshal(manifest)
		m["applied_manifest"] = json.RawMessage(raw)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// auditContainer renders one docker ps --format row.
func auditContainer(name, image, state, createdAt string, labels map[string]string) string {
	c := struct {
		ID        string            `json:"ID"`
		Names     string            `json:"Names"`
		Image     string            `json:"Image"`
		State     string            `json:"State"`
		Status    string            `json:"Status"`
		CreatedAt string            `json:"CreatedAt"`
		Labels    map[string]string `json:"Labels"`
	}{"id" + name, name, image, state, "Up", createdAt, labels}
	b, _ := json.Marshal(c)
	return string(b)
}

func auditSizeRow(name, size string) string {
	return fmt.Sprintf(`{"Names":%q,"Size":%q}`, name, size)
}

func findingsByKind(env serverAuditEnvelope, kind string) []auditFinding {
	var out []auditFinding
	for _, f := range env.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func oneFinding(t *testing.T, env serverAuditEnvelope, kind string) auditFinding {
	t.Helper()
	got := findingsByKind(env, kind)
	if len(got) != 1 {
		t.Fatalf("kind %s: expected exactly 1 finding, got %d: %+v", kind, len(got), env.Findings)
	}
	return got[0]
}

// kitchenSink is a server with one of everything: every finding kind, a pin,
// a prune notice, disk pressure (so the du read fires) and an app whose state
// cannot be read (so a check reads unknown). The expected findings are
// asserted piecewise by the tests below and pinned as the contracts fixture.
func auditKitchenSink() auditFixture {
	ghostBlock := "# TEPLOY BEGIN ghost\nghost.example.com {\n\treverse_proxy ghost-web-v1:3000\n}\n# TEPLOY END ghost\n"
	hollowBlock := "# TEPLOY BEGIN hollow\n# TEPLOY END hollow\n"
	blogBlock := "# TEPLOY BEGIN blog\nblog.example.com {\n\treverse_proxy blog-web-v3:3000\n}\n# TEPLOY END blog\n"
	filesBlock := "# TEPLOY BEGIN files\nfiles.example.com {\n\troot * /deployments/files/site\n\tfile_server\n}\n# TEPLOY END files\n"
	preview := "# TEPLOY BEGIN blog-preview-p-08e81639\npreview.example.com {\n\treverse_proxy blog-preview-p-08e81639-abc1234:3000\n}\n# TEPLOY END blog-preview-p-08e81639\n"
	manual := "hand.example.com {\n\treverse_proxy gone-svc:80\n}\n\nhttp://203.0.113.9 {\n\treverse_proxy 127.0.0.1:9000\n}\n"

	keep := func(n int) map[string]any {
		return map[string]any{"container": map[string]any{"keep_versions": n}}
	}
	manyV := func(v, when string, running bool) string {
		state := "exited"
		if running {
			state = "running"
		}
		return auditContainer("many-web-"+v, "many:"+v, state, when,
			map[string]string{"teploy.app": "many", "teploy.process": "web", "teploy.version": v})
	}
	unsetV := func(v, when string, running bool) string {
		state := "exited"
		if running {
			state = "running"
		}
		return auditContainer("unset-web-"+v, "unset:"+v, state, when,
			map[string]string{"teploy.app": "unset", "teploy.process": "web", "teploy.version": v})
	}
	containers := []string{
		auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil),
		auditContainer("blog-web-v2", "blog:v2", "running", "2026-11-04 10:00:00 +0000 UTC",
			map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v2"}),
		manyV("v4", "2026-11-04 09:00:00 +0000 UTC", true),
		manyV("v3", "2026-11-03 09:00:00 +0000 UTC", false),
		manyV("v2", "2026-11-02 09:00:00 +0000 UTC", false),
		manyV("v1", "2026-11-01 09:00:00 +0000 UTC", false),
		unsetV("u7", "2026-11-04 08:00:00 +0000 UTC", true),
		unsetV("u6", "2026-11-03 08:00:00 +0000 UTC", false),
		unsetV("u5", "2026-11-02 08:00:00 +0000 UTC", false),
		unsetV("u4", "2026-11-01 08:00:00 +0000 UTC", false),
		unsetV("u3", "2026-10-31 08:00:00 +0000 UTC", false),
		unsetV("u2", "2026-10-30 08:00:00 +0000 UTC", false),
		unsetV("u1", "2026-10-29 08:00:00 +0000 UTC", false),
		auditContainer("reacher-web-1f2e3d", "reacher:1", "running", "2026-08-01 00:00:00 +0000 UTC",
			map[string]string{"teploy.app": "reacher", "teploy.process": "web", "teploy.version": "1f2e3d"}),
		auditContainer("portainer", "portainer/portainer:2", "running", "2026-05-02 00:00:00 +0000 UTC", nil),
	}
	sizes := []string{
		auditSizeRow("caddy", "100MB (virtual 200MB)"),
		auditSizeRow("blog-web-v2", "50MB (virtual 1GB)"),
		auditSizeRow("many-web-v4", "10MB (virtual 1GB)"),
		auditSizeRow("many-web-v3", "10MB (virtual 1GB)"),
		auditSizeRow("many-web-v2", "10MB (virtual 1GB)"),
		auditSizeRow("many-web-v1", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u7", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u6", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u5", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u4", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u3", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u2", "10MB (virtual 1GB)"),
		auditSizeRow("unset-web-u1", "10MB (virtual 1GB)"),
		auditSizeRow("reacher-web-1f2e3d", "35.7GB (virtual 1.1GB)"),
		auditSizeRow("portainer", "2GB (virtual 500MB)"),
	}
	volumes := make([]string, 21)
	for i := range volumes {
		volumes[i] = fmt.Sprintf("%031dxxxxxxxdeadbeef", i)
	}
	return auditFixture{
		apps: []string{"blog", "many", "unset", "orphan", "static-site", "broken"},
		state: map[string]string{
			"blog":        auditStateJSON("container", "caddy", "v3", "v2", keep(3)),
			"many":        auditStateJSON("container", "caddy", "v4", "v3", keep(2)),
			"unset":       auditStateJSON("container", "caddy", "u7", "u6", map[string]any{"container": map[string]any{"keep_versions": 0}}),
			"orphan":      auditStateJSON("container", "caddy", "o1", "", nil),
			"static-site": auditStateJSON("static", "caddy", "s1", "", nil),
			"broken":      "",
		},
		badRC:      map[string]bool{"broken": true},
		caddyfile:  ghostBlock + "\n" + hollowBlock + "\n" + blogBlock + "\n" + filesBlock + "\n" + preview + "\n" + manual,
		containers: strings.Join(containers, "\n"),
		sizes:      strings.Join(sizes, "\n"),
		resolves: "0 blog-preview-p-08e81639-abc1234\n" +
			"2 ghost-web-v1\n" +
			"2 blog-web-v3\n" +
			"2 gone-svc\n",
		roots:      map[string]string{"/deployments/files/site": "yes"},
		addrs:      "192.0.2.10 172.17.0.1\n",
		usage:      `{"Type":"Images","Size":"20GB","Reclaimable":"1GB (5%)"}` + "\n" + `{"Type":"Containers","Size":"5GB","Reclaimable":"0B (0%)"}` + "\n" + `{"Type":"Local Volumes","Size":"4GB","Reclaimable":"3.15GB (78%)"}` + "\n" + `{"Type":"Build Cache","Size":"2GB","Reclaimable":"2GB (100%)"}`,
		danglingIm: "sha256:1111111111111111111111111111111111111111111111111111111111111111\nsha256:2222222222222222222222222222222222222222222222222222222222222222\nsha256:3333333333333333333333333333333333333333333333333333333333333333\n",
		danglingVo: strings.Join(volumes, "\n") + "\n",
		df:         "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 10000000000 9600000000 800000000 96% /\n",
		du:         "53687091200\t/deployments\n",
		scheduler:  "daemon=no\nenabled=disabled\ncrontab=yes\ncrontab_entries=2\nteploy_entries=1\n",
		files: map[string][]byte{
			"/deployments/many/pinned":         []byte("v1\n"),
			"/deployments/unset/.prune-notice": []byte("2026-11-01T00:00:00Z\n"),
		},
	}
}

func auditCheckStatus(env serverAuditEnvelope, name string) (string, bool) {
	for _, c := range env.Checks {
		if c.Name == name {
			return c.Status, true
		}
	}
	return "", false
}

func TestServerAudit_KitchenSinkFindings(t *testing.T) {
	env := auditKitchenSink().audit(t)

	if env.State != auditStatePartial {
		t.Fatalf("state = %s, want partial (one app state unreadable)", env.State)
	}
	if st, ok := auditCheckStatus(env, "apps"); !ok || st != auditCheckUnknown {
		t.Errorf("apps check = %q %v, want unknown: %+v", st, ok, env.Checks)
	}

	var kinds []string
	for _, f := range env.Findings {
		kinds = append(kinds, f.Kind)
	}
	got := strings.Join(kinds, ",")
	expected := []string{
		"dangling_volumes",
		"disk_pressure",
		"empty_managed_region",
		"large_writable_layer",
		"managed_route_dead_upstream",
		"manual_route_dead_upstream",
		"manual_route_nonlocal_address",
		"orphan_state",
		"reclaimable_build_artifacts",
		"scheduler_missing",
		"stale_route",
		"unmanaged_container",
		"versions_beyond_policy",
	}
	for _, k := range expected {
		if strings.Count(got, k) == 0 {
			t.Errorf("expected at least one %s finding; got: %s", k, got)
		}
	}
	if strings.Count(got, "versions_beyond_policy") != 2 {
		t.Errorf("expected two versions_beyond_policy findings (explicit + default), got: %s", got)
	}
	if strings.Count(got, "stale_route") != 2 {
		t.Errorf("expected two stale_route findings (ghost + files), got: %s", got)
	}
	// Findings are ordered by severity then id; a critical must not sort after an info.
	rank := map[string]int{severityCritical: 0, severityWarning: 1, severityInfo: 2}
	for i := 1; i < len(env.Findings); i++ {
		a, b := env.Findings[i-1], env.Findings[i]
		if rank[a.Severity] > rank[b.Severity] {
			t.Errorf("findings not sorted by severity: %s after %s", b.ID, a.ID)
		}
	}
}

func TestServerAudit_StaleRouteAndEmptyRegion(t *testing.T) {
	env := auditKitchenSink().audit(t)

	var ghost, files *auditFinding
	for _, f := range findingsByKind(env, findingStaleRoute) {
		switch f.Subject {
		case "ghost":
			ghost = &f
		case "files":
			files = &f
		}
	}
	if ghost == nil || files == nil {
		t.Fatalf("missing stale_route findings: %+v", env.Findings)
	}
	if ghost.App != "ghost" || !strings.Contains(ghost.Evidence, "no app ghost") {
		t.Errorf("ghost finding: %+v", ghost)
	}
	wantArgv := []string{"teploy", "route", "remove", "--app", "ghost", "--host", "edge"}
	if fmt.Sprint(ghost.Remedy.Argv) != fmt.Sprint(wantArgv) {
		t.Errorf("ghost remedy argv = %v, want %v", ghost.Remedy.Argv, wantArgv)
	}
	if ghost.Remedy.Destructive || ghost.Remedy.Confirm {
		t.Errorf("a proven-dead route needs no confirmation: %+v", ghost.Remedy)
	}

	// The files app's block still serves files that exist: reported, remedy null.
	if files.Remedy.Argv != nil {
		t.Errorf("a route still serving files must be report-only, got argv %v", files.Remedy.Argv)
	}
	if !strings.Contains(files.Evidence, "not removable") || !strings.Contains(files.Evidence, "/deployments/files/site") {
		t.Errorf("files evidence must name the refusal: %q", files.Evidence)
	}

	hollow := oneFinding(t, env, findingEmptyRegion)
	if hollow.Subject != "hollow" || hollow.Remedy.Argv == nil {
		t.Errorf("empty region should be removable: %+v", hollow)
	}
}

func TestServerAudit_ManagedDeadUpstreamOffersRedeploy(t *testing.T) {
	env := auditKitchenSink().audit(t)
	f := oneFinding(t, env, findingDeadUpstream)
	if f.Subject != "blog" {
		t.Fatalf("dead upstream subject = %s, want blog", f.Subject)
	}
	if f.Severity != severityCritical {
		t.Errorf("a fully dead route is critical, got %s", f.Severity)
	}
	if fmt.Sprint(f.Remedy.Argv) != fmt.Sprint([]string{"teploy", "redeploy", "--app", "blog", "--host", "edge"}) {
		t.Errorf("remedy argv = %v", f.Remedy.Argv)
	}
	if !f.Remedy.Confirm || f.Remedy.Destructive {
		t.Errorf("redeploy needs confirmation but is not destructive: %+v", f.Remedy)
	}
	if !strings.Contains(f.Evidence, "blog-web-v3:3000") {
		t.Errorf("evidence must name the dead upstream: %q", f.Evidence)
	}

	// A dead upstream that is NOT this app's (caddy_extra proxy to another
	// app) cannot be fixed by this app's redeploy: report only.
	fx := auditKitchenSink()
	fx.caddyfile = "# TEPLOY BEGIN blog\nblog.example.com {\n\treverse_proxy other-svc:3000\n}\n# TEPLOY END blog\n"
	fx.resolves = "2 other-svc\n"
	f = oneFinding(t, fx.audit(t), findingDeadUpstream)
	if f.Remedy.Argv != nil {
		t.Errorf("someone else's dead upstream must be report-only: %+v", f.Remedy)
	}
	if !strings.Contains(f.Evidence, "redeploy cannot help") {
		t.Errorf("evidence should say a redeploy cannot help: %q", f.Evidence)
	}
}

func TestServerAudit_OrphanState(t *testing.T) {
	env := auditKitchenSink().audit(t)
	f := oneFinding(t, env, findingOrphanState)
	if f.Subject != "orphan" {
		t.Fatalf("orphan subject = %s", f.Subject)
	}
	if fmt.Sprint(f.Remedy.Argv) != fmt.Sprint([]string{"teploy", "remove", "--app", "orphan", "--host", "edge", "-y"}) {
		t.Errorf("remedy argv = %v", f.Remedy.Argv)
	}
	if !f.Remedy.Destructive || !f.Remedy.Confirm {
		t.Errorf("removing state is destructive and needs confirmation: %+v", f.Remedy)
	}

	// A static app with no containers is normal, not an orphan.
	for _, f := range env.Findings {
		if f.Kind == findingOrphanState && f.Subject == "static-site" {
			t.Errorf("static app reported as orphan: %+v", f)
		}
	}
}

func TestServerAudit_VersionsBeyondPolicy(t *testing.T) {
	env := auditKitchenSink().audit(t)
	var many, unset *auditFinding
	for _, f := range findingsByKind(env, findingVersionsBeyond) {
		switch f.Subject {
		case "many":
			many = &f
		case "unset":
			unset = &f
		}
	}
	if many == nil || unset == nil {
		t.Fatalf("missing versions findings: %+v", env.Findings)
	}

	// many: keep 2, current v4 + previous v3 protected, v1 pinned → only v2.
	if !strings.Contains(many.Evidence, "beyond keep_versions 2: v2") {
		t.Errorf("many evidence = %q", many.Evidence)
	}
	if strings.Contains(many.Evidence, "v1") {
		t.Errorf("pinned v1 must not be listed as beyond policy: %q", many.Evidence)
	}
	if many.Severity != severityInfo || !strings.Contains(many.Evidence, "the next deploy removes them") {
		t.Errorf("an explicit keep_versions is housekeeping, not a warning: %+v", many)
	}
	if fmt.Sprint(many.Remedy.Argv) != fmt.Sprint([]string{"teploy", "releases", "prune", "--app", "many", "--host", "edge", "--keep", "2"}) {
		t.Errorf("many remedy argv = %v", many.Remedy.Argv)
	}
	if !many.Remedy.Destructive || !many.Remedy.Confirm {
		t.Errorf("pruning versions is destructive: %+v", many.Remedy)
	}

	// unset: keep_versions not set → default 5, warning + the notice date.
	if !strings.Contains(unset.Evidence, "beyond keep_versions 5") {
		t.Errorf("unset evidence = %q", unset.Evidence)
	}
	if unset.Severity != severityWarning {
		t.Errorf("an unset keep_versions pruning by default is a warning: %+v", unset)
	}
	if !strings.Contains(unset.Evidence, "2026-11-08") { // notice 2026-11-01 + 7d
		t.Errorf("unset evidence should name when automatic pruning starts: %q", unset.Evidence)
	}

	// No notice file yet: the warning says the next deploy starts the notice.
	fx := auditKitchenSink()
	delete(fx.files, "/deployments/unset/.prune-notice")
	env = fx.audit(t)
	for _, f := range findingsByKind(env, findingVersionsBeyond) {
		if f.Subject == "unset" && !strings.Contains(f.Evidence, "7-day notice") {
			t.Errorf("unset evidence without a notice file = %q", f.Evidence)
		}
	}
}

func TestServerAudit_ManualBlocksAreReportOnly(t *testing.T) {
	env := auditKitchenSink().audit(t)
	dead := oneFinding(t, env, findingManualDead)
	if dead.Remedy.Argv != nil {
		t.Errorf("hand-written blocks must never carry a remedy: %+v", dead)
	}
	if !strings.Contains(dead.Evidence, "gone-svc") || !strings.Contains(dead.Evidence, "hand-written") {
		t.Errorf("dead manual evidence = %q", dead.Evidence)
	}
	nonlocal := oneFinding(t, env, findingManualNonLocal)
	if nonlocal.Remedy.Argv != nil {
		t.Errorf("hand-written blocks must never carry a remedy: %+v", nonlocal)
	}
	if !strings.Contains(nonlocal.Evidence, "203.0.113.9") {
		t.Errorf("nonlocal evidence = %q", nonlocal.Evidence)
	}
}

func TestServerAudit_HygieneFindings(t *testing.T) {
	env := auditKitchenSink().audit(t)

	artifacts := oneFinding(t, env, findingBuildArtifacts)
	if fmt.Sprint(artifacts.Remedy.Argv) != fmt.Sprint([]string{"teploy", "server", "prune", "--host", "edge", "--build-cache", "--dangling"}) {
		t.Errorf("build artifacts remedy = %v", artifacts.Remedy.Argv)
	}
	if artifacts.Remedy.Destructive || artifacts.Remedy.Confirm {
		t.Errorf("rebuildable cache needs no confirmation: %+v", artifacts.Remedy)
	}

	layer := oneFinding(t, env, findingLargeLayer)
	if layer.Subject != "reacher-web-1f2e3d" || layer.Remedy.Argv != nil {
		t.Errorf("large writable layer is report-only: %+v", layer)
	}
	if layer.SizeBytes < 35_000_000_000 {
		t.Errorf("layer size = %d", layer.SizeBytes)
	}

	unmanaged := oneFinding(t, env, findingUnmanagedLarge)
	if unmanaged.Subject != "portainer" || unmanaged.Remedy.Argv != nil {
		t.Errorf("unmanaged container is report-only: %+v", unmanaged)
	}

	volumes := oneFinding(t, env, findingDanglingVolumes)
	if volumes.Remedy.Argv != nil {
		t.Errorf("volumes may hold the only copy — report only: %+v", volumes)
	}
	if !strings.Contains(volumes.Evidence, "21 volume(s)") || !strings.Contains(volumes.Evidence, "3.1 GB") {
		t.Errorf("volumes evidence = %q", volumes.Evidence)
	}

	sched := oneFinding(t, env, findingSchedulerMissing)
	if sched.Severity != severityCritical || sched.Remedy.Argv != nil {
		t.Errorf("teploy jobs with no cron daemon is critical, report-only: %+v", sched)
	}
}

func TestServerAudit_CaddyfileWithoutCaddy(t *testing.T) {
	fx := auditKitchenSink()
	fx.containers = strings.ReplaceAll(fx.containers,
		auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil),
		auditContainer("caddy", "caddy:2", "exited", "2026-05-01 00:00:00 +0000 UTC", nil))
	env := fx.audit(t)
	f := oneFinding(t, env, findingCaddyfileNoCaddy)
	if f.Severity != severityWarning || !strings.Contains(f.Evidence, "the caddy container is exited") {
		t.Errorf("caddyfile without caddy: %+v", f)
	}
	// Without a running caddy nothing can be proven dead: the stale route
	// loses its one-click remedy instead of being reported removable.
	var ghost *auditFinding
	for _, f := range findingsByKind(env, findingStaleRoute) {
		if f.Subject == "ghost" {
			ghost = &f
		}
	}
	if ghost == nil {
		t.Fatalf("no stale_route finding for ghost: %+v", env.Findings)
	}
	if ghost.Remedy.Argv != nil || !strings.Contains(ghost.Evidence, "not removable") {
		t.Errorf("an unprovable route must be report-only: %+v", ghost)
	}
	if st, ok := auditCheckStatus(env, "upstreams"); !ok || st != auditCheckUnknown {
		t.Errorf("upstreams check = %q %v, want unknown without a running caddy", st, ok)
	}
}

func TestServerAudit_DiskPressureAttributesAndLinks(t *testing.T) {
	env := auditKitchenSink().audit(t)
	f := oneFinding(t, env, findingDiskPressure)
	if f.Severity != severityCritical { // 800MB < 2GiB
		t.Errorf("800MB free is critical, got %s", f.Severity)
	}
	if !strings.Contains(f.Evidence, "images 20.0 GB") || !strings.Contains(f.Evidence, "deployments") {
		t.Errorf("disk evidence must attribute the space: %q", f.Evidence)
	}
	var relatedKinds []string
	for _, id := range f.Related {
		for _, x := range env.Findings {
			if x.ID == id {
				relatedKinds = append(relatedKinds, x.Kind)
			}
		}
	}
	for _, want := range []string{findingVersionsBeyond, findingBuildArtifacts, findingDanglingVolumes, findingLargeLayer} {
		found := false
		for _, k := range relatedKinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("disk pressure related findings missing %s: %v", want, f.Related)
		}
	}
	if env.Disk == nil || env.Disk.AvailableBytes != 800000000 || env.Disk.UsedPercent != 96 {
		t.Errorf("disk = %+v", env.Disk)
	}
}

func TestServerAudit_LivePreviewIsNotAFinding(t *testing.T) {
	env := auditKitchenSink().audit(t)
	for _, f := range env.Findings {
		if strings.Contains(f.ID, "preview") {
			t.Errorf("a live preview route is not stale: %+v", f)
		}
	}
}

func TestServerAudit_UnreadableYieldsCheckNotFinding(t *testing.T) {
	fx := auditKitchenSink()
	// The container listing fails: orphan/large-layer/unmanaged findings are
	// impossible to judge, and routes cannot be proven dead.
	fx.extra = append([]ssh.MockCommand{{Match: "docker ps --all ", Err: errors.New("docker: cannot connect to the docker daemon")}}, fx.extra...)
	env := fx.audit(t)
	if env.State != auditStatePartial {
		t.Fatalf("state = %s, want partial", env.State)
	}
	if st, ok := auditCheckStatus(env, "containers"); !ok || st != auditCheckUnknown {
		t.Fatalf("containers check = %q %v, want unknown: %+v", st, ok, env.Checks)
	}
	for _, kind := range []string{findingOrphanState, findingLargeLayer, findingUnmanagedLarge, findingCaddyfileNoCaddy} {
		if got := findingsByKind(env, kind); len(got) != 0 {
			t.Errorf("with containers unreadable there must be no %s finding: %+v", kind, got)
		}
	}
	var ghost *auditFinding
	for _, f := range findingsByKind(env, findingStaleRoute) {
		if f.Subject == "ghost" {
			ghost = &f
		}
	}
	if ghost == nil {
		t.Fatalf("no stale_route finding for ghost: %+v", env.Findings)
	}
	if ghost.Remedy.Argv != nil {
		t.Errorf("removal cannot be proven safe without the container list: %+v", ghost.Remedy)
	}
	// An app whose state could not be read is never judged; the check names it.
	if st, ok := auditCheckStatus(env, "apps"); !ok || st != auditCheckUnknown {
		t.Errorf("apps check = %q %v, want unknown", st, ok)
	}
}

func TestServerAudit_HealthyServerReportsNothing(t *testing.T) {
	fx := auditFixture{
		apps:      []string{"blog"},
		state:     map[string]string{"blog": auditStateJSON("container", "caddy", "v3", "v2", map[string]any{"container": map[string]any{"keep_versions": 3}})},
		caddyfile: "# TEPLOY BEGIN blog\nblog.example.com {\n\treverse_proxy blog-web-v3:3000\n}\n# TEPLOY END blog\n",
		containers: auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil) + "\n" +
			auditContainer("blog-web-v3", "blog:v3", "running", "2026-11-04 10:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v3"}),
		sizes:     auditSizeRow("caddy", "100MB (virtual 200MB)") + "\n" + auditSizeRow("blog-web-v3", "50MB (virtual 1GB)"),
		resolves:  "0 blog-web-v3\n",
		addrs:     "192.0.2.10\n",
		usage:     `{"Type":"Images","Size":"20GB","Reclaimable":"1GB (5%)"}` + "\n" + `{"Type":"Containers","Size":"5GB","Reclaimable":"0B (0%)"}` + "\n" + `{"Type":"Local Volumes","Size":"4GB","Reclaimable":"0B (0%)"}` + "\n" + `{"Type":"Build Cache","Size":"2GB","Reclaimable":"100MB (5%)"}`,
		df:        "Filesystem 1-blocks Used Available Capacity Mounted on\n/dev/vda1 100000000000 30000000000 70000000000 30% /\n",
		scheduler: "daemon=yes\nenabled=enabled\ncrontab=no\n",
	}
	env := fx.audit(t)
	if env.State != auditStateOK {
		t.Fatalf("state = %s, want ok; checks: %+v", env.State, env.Checks)
	}
	if len(env.Findings) != 0 {
		t.Fatalf("a healthy server must report no findings: %+v", env.Findings)
	}
}

func TestServerAudit_JSONEncodesRemedyNull(t *testing.T) {
	env := auditKitchenSink().audit(t)
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	findings, _ := doc["findings"].([]any)
	if len(findings) == 0 {
		t.Fatal("no findings in JSON")
	}
	nullArgv := 0
	for _, f := range findings {
		remedy := f.(map[string]any)["remedy"].(map[string]any)
		argv, _ := remedy["argv"].([]any)
		if remedy["argv"] == nil {
			nullArgv++
		} else if len(argv) == 0 || argv[0] != "teploy" {
			t.Errorf("remedy argv must be null or start with teploy: %v", remedy["argv"])
		}
		if _, ok := remedy["destructive"].(bool); !ok {
			t.Errorf("destructive must always be present: %v", remedy)
		}
		if _, ok := remedy["confirm"].(bool); !ok {
			t.Errorf("confirm must always be present: %v", remedy)
		}
	}
	if nullArgv == 0 {
		t.Error("no finding with remedy argv null in the fixture")
	}
	if _, ok := doc["disk"].(map[string]any); !ok {
		t.Error("disk must be an object on a reachable server")
	}
	if _, ok := doc["checks"].([]any); !ok {
		t.Error("checks must be an array")
	}
}

// The read-only pin: run the full kitchen sink (every read fires, including
// the pressure-only du and the retention reads) through a recording wrapper
// and refuse any mutating verb, docker subcommand outside the read set, or
// file upload.
func TestServerAudit_EveryCommandOnlyReads(t *testing.T) {
	fx := auditKitchenSink()
	base := fx.exec()
	rec := &recordingExecutor{Executor: base}
	env := auditServer(context.Background(), rec, "edge", "192.0.2.10", auditFixtureNow)
	if env.State != auditStatePartial {
		t.Fatalf("fixture stopped exercising a read: %+v", env.Checks)
	}
	sawDU := false
	for _, cmd := range rec.commands {
		if strings.Contains(cmd, "du -sxb") {
			sawDU = true
		}
	}
	if !sawDU {
		t.Errorf("the disk-pressure attribution read never ran; recorded: %v", rec.commands)
	}
	if len(rec.uploads) != 0 {
		t.Errorf("the audit uploaded files: %v", rec.uploads)
	}
	if len(rec.inputs) != 0 {
		t.Errorf("the audit streamed stdin to commands: %v", rec.inputs)
	}
	if len(rec.commands) == 0 {
		t.Fatal("no commands recorded")
	}
	banned := []*regexp.Regexp{
		regexp.MustCompile(`\brm\b`), regexp.MustCompile(`\bmv\b`), regexp.MustCompile(`\bmkdir\b`),
		regexp.MustCompile(`\brmdir\b`), regexp.MustCompile(`\bcp\b`), regexp.MustCompile(`\btouch\b`),
		regexp.MustCompile(`\btruncate\b`), regexp.MustCompile(`\btee\b`), regexp.MustCompile(`\bchmod\b`),
		regexp.MustCompile(`\bchown\b`), regexp.MustCompile(`\bkill\b`), regexp.MustCompile(`\bpkill\b`),
		regexp.MustCompile(`\breboot\b`), regexp.MustCompile(`\bshutdown\b`), regexp.MustCompile(`\bsed\b`),
		regexp.MustCompile(`\bdd\b`), regexp.MustCompile(`\bmkfs\b`),
		regexp.MustCompile(`\bmount\b`), regexp.MustCompile(`\bumount\b`), regexp.MustCompile(`\bcurl\b`),
		regexp.MustCompile(`\bwget\b`), regexp.MustCompile(`\bapt\b`), regexp.MustCompile(`\byum\b`),
		regexp.MustCompile(`\bdnf\b`), regexp.MustCompile(`\bpip\b`), regexp.MustCompile(`\buseradd\b`),
		regexp.MustCompile(`\bpasswd\b`),
		regexp.MustCompile(`\bsystemctl\s+(start|stop|restart|enable|disable|mask)\b`),
		regexp.MustCompile(`>>`),
	}
	allowedDocker := map[string]bool{"ps": true, "images": true, "volume": true, "system": true, "exec": true, "inspect": true, "version": true}
	for _, cmd := range rec.commands {
		for _, re := range banned {
			if re.MatchString(cmd) {
				t.Errorf("audit ran a mutating command (%s): %s", re, cmd)
			}
		}
		if strings.Contains(cmd, "crontab") && !strings.Contains(cmd, "crontab -l") {
			t.Errorf("audit touched crontab beyond -l: %s", cmd)
		}
		for _, tok := range strings.Fields(cmd) {
			if strings.HasPrefix(tok, ">") && tok != ">/dev/null" {
				t.Errorf("audit redirects stdout (%s): %s", tok, cmd)
			}
		}
		if i := strings.Index(cmd, "docker "); i >= 0 {
			fields := strings.Fields(cmd[i:])
			if len(fields) > 1 && !allowedDocker[fields[1]] {
				t.Errorf("audit ran docker %s: %s", fields[1], cmd)
			}
			if len(fields) > 1 && fields[1] == "exec" && !strings.Contains(cmd, "getent") {
				t.Errorf("audit docker exec is not the read-only getent probe: %s", cmd)
			}
		}
	}
}

// recordingExecutor wraps an Executor and records everything asked of it, so
// a test can pin WHAT a read-only code path may do to a server.
type recordingExecutor struct {
	ssh.Executor
	commands []string
	inputs   []string
	uploads  []string
}

func (r *recordingExecutor) Run(ctx context.Context, cmd string) (string, error) {
	r.commands = append(r.commands, cmd)
	return r.Executor.Run(ctx, cmd)
}

func (r *recordingExecutor) RunStream(ctx context.Context, cmd string, stdout, stderr io.Writer) error {
	r.commands = append(r.commands, cmd)
	return r.Executor.RunStream(ctx, cmd, stdout, stderr)
}

func (r *recordingExecutor) RunInput(ctx context.Context, cmd string, stdin io.Reader) error {
	r.inputs = append(r.inputs, cmd)
	return r.Executor.RunInput(ctx, cmd, stdin)
}

func (r *recordingExecutor) Upload(ctx context.Context, content io.Reader, remotePath string, mode string) error {
	r.uploads = append(r.uploads, remotePath)
	return r.Executor.Upload(ctx, content, remotePath, mode)
}

func TestParseDockerSize(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"12B", 12, true},
		{"50MB", 50_000_000, true},
		{"2.5GB", 2_500_000_000, true},
		{"1KiB", 1024, true},
		{"35.7GB", 35_700_000_000, true}, // the "(virtual …)" tail is split off by the caller
		{"", 0, false},
		{"lots", 0, false},
	} {
		got, ok := parseDockerSize(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseDockerSize(%q) = %d,%v want %d,%v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
