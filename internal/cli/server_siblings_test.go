package cli

// `teploy server siblings` tests. Same discipline as server_audit_test.go:
// one fixture models a server through the ssh MockExecutor — synthetic
// docker output, real parse and detection stages — and the read-only pin at
// the bottom is the command's safety property: nothing it issues may mutate,
// and the whole observation is at most two remote commands.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

var siblingsFixtureNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func siblingsImageID(seed string) string {
	// A deterministic full sha256-shaped id for fixture images (hex only —
	// IsImageID rejects anything else).
	sum := sha256.Sum256([]byte("siblings-fixture:" + seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// siblingsFixture models one server's docker answers: the `docker ps --all`
// rows and the batched inspect output (port binds + image facts in one
// command, exactly as InspectSiblings issues it).
type siblingsFixture struct {
	containers   string // docker ps --all --format rows
	inspectPorts string // docker inspect half: /name\t{ports} lines
	inspectImgs  string // docker image inspect half: id\t[tags]\t{labels} lines

	psErr     error // the container listing fails
	extra     []ssh.MockCommand
	noInspect bool // the second round returns nothing at all
}

func (fx siblingsFixture) inspectOutput() string {
	if fx.noInspect {
		return ""
	}
	out := fx.inspectPorts
	if fx.inspectImgs != "" {
		out += "\n__TEPLOY_SIBLINGS_IMAGES__\n" + fx.inspectImgs
	}
	return out
}

func (fx siblingsFixture) exec() *ssh.MockExecutor {
	cmds := append([]ssh.MockCommand{}, fx.extra...)
	cmds = append(cmds, ssh.MockCommand{Match: "docker ps --all ", Output: fx.containers, Err: fx.psErr})
	if !fx.noInspect {
		cmds = append(cmds, ssh.MockCommand{Match: "docker inspect --format", Output: fx.inspectOutput()})
	}
	return ssh.NewMockExecutor("192.0.2.10", cmds...)
}

func (fx siblingsFixture) collect(t *testing.T) serverSiblingsEnvelope {
	t.Helper()
	return collectServerSiblings(context.Background(), fx.exec(), "infra-home", "192.0.2.10", siblingsFixtureNow)
}

// siblingsKitchenSink: one server carrying one of everything — an observe
// app (two containers) found by its own labels, a dash container found
// through its IMAGE's label, a forgejo found through its OCI labels, an
// unrecognised product passed through verbatim, and ordinary containers
// (caddy, a teploy app that is not a product, a database) that must not
// appear anywhere.
func siblingsKitchenSink() siblingsFixture {
	containers := strings.Join([]string{
		auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil),
		auditContainer("blog-web-v3", "blog:v3", "running", "2026-11-04 10:00:00 +0000 UTC",
			map[string]string{"teploy.app": "blog", "teploy.process": "web", "teploy.version": "v3"}),
		auditContainer("blog-db", "postgres:16", "running", "2026-11-04 10:00:00 +0000 UTC",
			map[string]string{"teploy.app": "blog", "teploy.role": "accessory"}),
		auditContainer("dash-web-1a2b3c4", "ghcr.io/useteploy/dash:v0.3.1", "running", "2026-11-04 09:00:00 +0000 UTC", nil),
		auditContainer("forgejo", "codeberg.org/forgejo/forgejo:11", "running", "2026-08-01 00:00:00 +0000 UTC", nil),
		auditContainer("lullmail-web-9f8e7d6", "ghcr.io/fylun/lullmail:latest", "running", "2026-10-01 00:00:00 +0000 UTC",
			map[string]string{"dev.teploy.product": "lullmail", "teploy.app": "lullmail", "teploy.version": "9f8e7d6"}),
		auditContainer("observe-web-7cf98cae", "ghcr.io/useteploy/observe:7cf98cae", "running", "2026-11-05 08:00:00 +0000 UTC",
			map[string]string{"dev.teploy.product": "observe", "teploy.app": "observe", "teploy.process": "web", "teploy.version": "7cf98cae"}),
		auditContainer("observe-worker-7cf98cae", "ghcr.io/useteploy/observe:7cf98cae", "running", "2026-11-05 08:00:00 +0000 UTC",
			map[string]string{"dev.teploy.product": "observe", "teploy.app": "observe", "teploy.process": "worker", "teploy.version": "7cf98cae"}),
	}, "\n")
	ports := strings.Join([]string{
		`/blog-db	{}`,
		`/blog-web-v3	{}`,
		`/caddy	{"80/tcp":null,"443/tcp":null}`,
		`/dash-web-1a2b3c4	` + `{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49221"},{"HostIp":"::","HostPort":"49221"}]}`,
		`/forgejo	` + `{"2222/tcp":null,"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"3000"}]}`,
		`/lullmail-web-9f8e7d6	` + `{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"8080"}]}`,
		`/observe-web-7cf98cae	` + `{"3000/tcp":[{"HostIp":"100.108.123.49","HostPort":"3000"}]}`,
		`/observe-worker-7cf98cae	{}`,
	}, "\n")
	img := func(seed string, tags string, labels string) string {
		return siblingsImageID(seed) + "\t" + tags + "\t" + labels
	}
	images := strings.Join([]string{
		img("blog", `["blog:v3"]`, `{}`),
		img("caddy", `["caddy:2"]`, `{"org.opencontainers.image.source":"https://github.com/caddyserver/caddy"}`),
		img("forgejo", `["codeberg.org/forgejo/forgejo:11"]`, `{"org.opencontainers.image.source":"https://codeberg.org/forgejo/forgejo","org.opencontainers.image.title":"Forgejo application container","org.opencontainers.image.version":"11.0.1"}`),
		img("lullmail", `["ghcr.io/fylun/lullmail:latest"]`, `{}`),
		img("dash", `["ghcr.io/useteploy/dash:v0.3.1"]`, `{"dev.teploy.product":"dash","org.opencontainers.image.version":"v0.3.1"}`),
		img("observe", `["ghcr.io/useteploy/observe:7cf98cae"]`, `{"dev.teploy.product":"observe","org.opencontainers.image.version":"1.4.0"}`),
		img("postgres", `["postgres:16"]`, `{"org.opencontainers.image.source":"https://github.com/docker-library/postgres"}`),
	}, "\n")
	return siblingsFixture{containers: containers, inspectPorts: ports, inspectImgs: images}
}

func siblingsByContainer(env serverSiblingsEnvelope, name string) (siblingDTO, bool) {
	for _, s := range env.Siblings {
		if s.Container == name {
			return s, true
		}
	}
	return siblingDTO{}, false
}

func TestServerSiblings_DetectionAndSources(t *testing.T) {
	env := siblingsKitchenSink().collect(t)
	if env.State != auditStateOK {
		t.Fatalf("state = %s (%s), want ok", env.State, env.Error)
	}
	if len(env.Siblings) != 4 {
		t.Fatalf("siblings = %+v, want dash, forgejo and the two observe containers", env.Siblings)
	}
	for _, tt := range []struct {
		container, product, source, version, app, image string
		hints                                           []siblingPortHint
	}{
		{
			container: "dash-web-1a2b3c4", product: "dash", source: "image_label", version: "v0.3.1", image: "ghcr.io/useteploy/dash:v0.3.1",
			hints: []siblingPortHint{
				{HostIP: "127.0.0.1", HostPort: 49221, ContainerPort: 3000, Proto: "tcp"},
				{HostIP: "::", HostPort: 49221, ContainerPort: 3000, Proto: "tcp"},
			},
		},
		{
			container: "forgejo", product: "forgejo", source: "oci_label", version: "11.0.1", image: "codeberg.org/forgejo/forgejo:11",
			hints: []siblingPortHint{{HostIP: "127.0.0.1", HostPort: 3000, ContainerPort: 3000, Proto: "tcp"}},
		},
		{
			container: "observe-web-7cf98cae", product: "observe", source: "label", version: "7cf98cae", app: "observe", image: "ghcr.io/useteploy/observe:7cf98cae",
			hints: []siblingPortHint{{HostIP: "100.108.123.49", HostPort: 3000, ContainerPort: 3000, Proto: "tcp"}},
		},
		{
			container: "observe-worker-7cf98cae", product: "observe", source: "label", version: "7cf98cae", app: "observe", image: "ghcr.io/useteploy/observe:7cf98cae",
			hints: []siblingPortHint{},
		},
	} {
		got, ok := siblingsByContainer(env, tt.container)
		if !ok {
			t.Fatalf("%s missing from siblings: %+v", tt.container, env.Siblings)
		}
		want := siblingDTO{Container: tt.container, Product: tt.product, Version: tt.version, App: tt.app,
			State: "running", Image: tt.image, PortHints: tt.hints, Source: tt.source}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s =\n%+v\nwant\n%+v", tt.container, got, want)
		}
	}
	// The unrecognised product passes through verbatim.
	if len(env.UnknownLabeled) != 1 || env.UnknownLabeled[0].Product != "lullmail" || env.UnknownLabeled[0].Container != "lullmail-web-9f8e7d6" {
		t.Fatalf("unknown_labeled = %+v, want the lullmail row passed through verbatim", env.UnknownLabeled)
	}
	if env.UnknownLabeled[0].App != "lullmail" || env.UnknownLabeled[0].Version != "9f8e7d6" || len(env.UnknownLabeled[0].PortHints) != 1 {
		t.Errorf("unknown row lost its hints: %+v", env.UnknownLabeled[0])
	}
	// Ordinary containers never appear.
	for _, name := range []string{"caddy", "blog-web-v3", "blog-db"} {
		if s, ok := siblingsByContainer(env, name); ok {
			t.Errorf("%s is not a sibling: %+v", name, s)
		}
	}
}

func TestServerSiblings_VersionPriority(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		imageLabels string
		wantVersion string
		wantOmitted bool // version field absent from JSON
	}{
		{
			name:        "dev product version wins",
			labels:      map[string]string{"dev.teploy.product": "ship", "dev.teploy.version": "2.0.0", "teploy.app": "ship", "teploy.version": "1f2e3d4"},
			imageLabels: `{"org.opencontainers.image.version":"9.9.9"}`,
			wantVersion: "2.0.0",
		},
		{
			name:        "teploy release version for a teploy app",
			labels:      map[string]string{"dev.teploy.product": "ship", "teploy.app": "ship", "teploy.version": "1f2e3d4"},
			imageLabels: `{"org.opencontainers.image.version":"9.9.9"}`,
			wantVersion: "1f2e3d4",
		},
		{
			name:        "teploy version without a teploy app does not count",
			labels:      map[string]string{"dev.teploy.product": "ship", "teploy.version": "1f2e3d4"},
			imageLabels: `{"org.opencontainers.image.version":"9.9.9"}`,
			wantVersion: "9.9.9",
		},
		{
			name:        "image oci version is the last resort",
			labels:      map[string]string{"dev.teploy.product": "ship"},
			imageLabels: `{"org.opencontainers.image.version":"9.9.9"}`,
			wantVersion: "9.9.9",
		},
		{
			name:        "nothing says",
			labels:      map[string]string{"dev.teploy.product": "ship"},
			imageLabels: `{}`,
			wantVersion: "",
			wantOmitted: true,
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := siblingsFixture{
				containers:   auditContainer("ship-web-1", "example/ship:1", "running", "2026-11-04 10:00:00 +0000 UTC", tt.labels),
				inspectPorts: "/ship-web-1\t{}",
				inspectImgs:  fmt.Sprintf("%s\t%s\t%s", siblingsImageID(fmt.Sprintf("ship%d", i)), `["example/ship:1"]`, tt.imageLabels),
			}
			env := fx.collect(t)
			if len(env.Siblings) != 1 {
				t.Fatalf("siblings = %+v", env.Siblings)
			}
			if env.Siblings[0].Version != tt.wantVersion {
				t.Errorf("version = %q, want %q", env.Siblings[0].Version, tt.wantVersion)
			}
			raw, err := json.Marshal(env.Siblings[0])
			if err != nil {
				t.Fatal(err)
			}
			if hasVersion := strings.Contains(string(raw), `"version"`); hasVersion == tt.wantOmitted {
				t.Errorf("json %s: version present = %v, want omitted %v", raw, hasVersion, tt.wantOmitted)
			}
		})
	}
}

func TestServerSiblings_ContainerLabelBeatsImageAndForgejoFamilies(t *testing.T) {
	// A container carrying dev.teploy.product never falls through to the
	// image's differing label (source stays "label"), and the forgejo
	// family detection covers gitea's source label too.
	fx := siblingsFixture{
		containers: strings.Join([]string{
			auditContainer("pinned", "some/registry/observe-fork:2", "running", "2026-11-04 10:00:00 +0000 UTC",
				map[string]string{"dev.teploy.product": "observe"}),
			auditContainer("gitea", "gitea/gitea:1.22", "running", "2026-08-01 00:00:00 +0000 UTC", nil),
		}, "\n"),
		inspectPorts: "/gitea\t{}\n/pinned\t{}",
		inspectImgs: strings.Join([]string{
			siblingsImageID("fork") + "\t" + `["some/registry/observe-fork:2"]` + "\t" + `{"dev.teploy.product":"dash"}`,
			siblingsImageID("gitea") + "\t" + `["gitea/gitea:1.22"]` + "\t" + `{"org.opencontainers.image.source":"https://github.com/go-gitea/gitea"}`,
		}, "\n"),
	}
	env := fx.collect(t)
	pinned, ok := siblingsByContainer(env, "pinned")
	if !ok || pinned.Product != "observe" || pinned.Source != siblingSourceLabel {
		t.Errorf("the container's own label wins: %+v", pinned)
	}
	gitea, ok := siblingsByContainer(env, "gitea")
	if !ok || gitea.Product != "forgejo" || gitea.Source != siblingSourceOCI {
		t.Errorf("gitea images are the forgejo family: %+v", gitea)
	}
}

func TestServerSiblings_ImageIDRefResolvesToTag(t *testing.T) {
	// teploy deploys run containers from the immutable image ID (A52):
	// the row reports the artifact tag, not docker ps's 12-hex form.
	id := siblingsImageID("tagged")
	short := strings.TrimPrefix(id, "sha256:")[:12]
	fx := siblingsFixture{
		containers:   auditContainer("dash-web-1a2b3c4", short, "running", "2026-11-04 09:00:00 +0000 UTC", nil),
		inspectPorts: `/dash-web-1a2b3c4	{}`,
		inspectImgs:  id + "\t" + `["ghcr.io/useteploy/dash:v0.3.1"]` + "\t" + `{"dev.teploy.product":"dash"}`,
	}
	env := fx.collect(t)
	got, ok := siblingsByContainer(env, "dash-web-1a2b3c4")
	if !ok {
		t.Fatalf("ID-created sibling not detected: %+v", env.Siblings)
	}
	if got.Image != "ghcr.io/useteploy/dash:v0.3.1" {
		t.Errorf("image = %q, want the resolved repo tag", got.Image)
	}
}

func TestServerSiblings_NoneFound(t *testing.T) {
	fx := siblingsFixture{
		containers: strings.Join([]string{
			auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil),
			auditContainer("blog-web-v3", "blog:v3", "running", "2026-11-04 10:00:00 +0000 UTC",
				map[string]string{"teploy.app": "blog", "teploy.version": "v3"}),
		}, "\n"),
		inspectPorts: "/blog-web-v3\t{}\n/caddy\t{}",
		inspectImgs:  siblingsImageID("blog") + "\t" + `["blog:v3"]` + "\t" + `{}`,
	}
	env := fx.collect(t)
	if env.State != auditStateOK {
		t.Fatalf("state = %s (%s)", env.State, env.Error)
	}
	if len(env.Siblings) != 0 || len(env.UnknownLabeled) != 0 {
		t.Fatalf("expected nothing, got %+v / %+v", env.Siblings, env.UnknownLabeled)
	}
	// Empty arrays on the wire, never nulls a consumer must special-case.
	raw, _ := json.Marshal(env)
	for _, key := range []string{`"siblings":[]`, `"unknown_labeled":[]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("envelope missing %s: %s", key, raw)
		}
	}
}

func TestServerSiblings_UnreadableContainersIsPartialNeverClean(t *testing.T) {
	fx := siblingsKitchenSink()
	fx.psErr = errors.New("docker: cannot connect to the docker daemon")
	env := fx.collect(t)
	if env.State != auditStatePartial {
		t.Fatalf("state = %s, want partial", env.State)
	}
	if env.Error == "" || !strings.Contains(env.Error, "cannot connect") {
		t.Errorf("error = %q, want the listing failure named", env.Error)
	}
	if len(env.Siblings) != 0 || len(env.UnknownLabeled) != 0 {
		t.Errorf("an unlisted server must not report candidates: %+v", env.Siblings)
	}
}

func TestServerSiblings_InspectWholesaleFailureIsPartial(t *testing.T) {
	// The second round returned nothing at all (the || true swallowed a
	// daemon outage): the observation is partial, and what trip 1 proved —
	// the label-detected sibling — is still reported, without port hints.
	fx := siblingsKitchenSink()
	fx.noInspect = true
	env := fx.collect(t)
	if env.State != auditStatePartial {
		t.Fatalf("state = %s, want partial", env.State)
	}
	observe, ok := siblingsByContainer(env, "observe-web-7cf98cae")
	if !ok || len(observe.PortHints) != 0 {
		t.Errorf("label-detected sibling must survive without hints: %+v", observe)
	}
	if _, ok := siblingsByContainer(env, "dash-web-1a2b3c4"); ok {
		t.Errorf("image-label detection is impossible without the second round")
	}
}

func TestServerSiblings_RowsSortedAndDeterministic(t *testing.T) {
	first := siblingsKitchenSink().collect(t)
	for i := 0; i < 5; i++ {
		again := siblingsKitchenSink().collect(t)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("observation not deterministic:\n%+v\nvs\n%+v", first, again)
		}
	}
	for i := 1; i < len(first.Siblings); i++ {
		if first.Siblings[i-1].Container > first.Siblings[i].Container {
			t.Errorf("siblings not sorted by container: %s after %s", first.Siblings[i].Container, first.Siblings[i-1].Container)
		}
	}
}

func TestServerSiblings_JSONRoundTrip(t *testing.T) {
	env := siblingsKitchenSink().collect(t)
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serverSiblingsEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	again, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(again) {
		t.Fatalf("round-trip changed the envelope:\n%s\nvs\n%s", raw, again)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"machine_interface", "server", "host", "observed_at", "state", "siblings", "unknown_labeled"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("envelope missing %q: %s", key, raw)
		}
	}
	if doc["machine_interface"] != float64(MachineInterface) {
		t.Errorf("machine_interface = %v", doc["machine_interface"])
	}
	for _, s := range append(append([]siblingDTO{}, decoded.Siblings...), decoded.UnknownLabeled...) {
		if s.Container == "" || s.Product == "" || s.Image == "" || s.Source == "" || s.State == "" {
			t.Errorf("row missing a required field: %+v", s)
		}
		for _, h := range s.PortHints {
			if h.Proto == "" || h.HostPort == 0 || h.ContainerPort == 0 {
				t.Errorf("port hint missing fields: %+v", h)
			}
		}
	}
}

func TestServerSiblings_HumanOutput(t *testing.T) {
	env := siblingsKitchenSink().collect(t)
	var out strings.Builder
	if err := printServerSiblings(&out, env); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"observe-web-7cf98cae", "forgejo", "100.108.123.49:3000->3000/tcp", "lullmail", "unrecognised"} {
		if !strings.Contains(text, want) {
			t.Errorf("human output missing %q:\n%s", want, text)
		}
	}

	empty := siblingsFixture{containers: auditContainer("caddy", "caddy:2", "running", "2026-05-01 00:00:00 +0000 UTC", nil),
		inspectPorts: "/caddy\t{}"}.collect(t)
	out.Reset()
	if err := printServerSiblings(&out, empty); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No sibling products found") {
		t.Errorf("empty server output = %q", out.String())
	}

	unreachable := serverSiblingsEnvelope{State: auditStateUnreachable, Error: "ssh: no route to host"}
	if err := printServerSiblings(&out, unreachable); err == nil {
		t.Error("an unreachable server is an error in the human form")
	}
}

// The read-only pin, after TestServerAudit_EveryCommandOnlyReads: run the
// kitchen-sink observation through a recording wrapper and refuse any
// mutating verb, docker subcommand outside the read set, stdin or upload —
// and pin that the whole observation is at most TWO remote commands.
func TestServerSiblings_EveryCommandOnlyReads(t *testing.T) {
	base := siblingsKitchenSink().exec()
	rec := &recordingExecutor{Executor: base}
	env := collectServerSiblings(context.Background(), rec, "infra-home", "192.0.2.10", siblingsFixtureNow)
	if env.State != auditStateOK {
		t.Fatalf("fixture stopped exercising a read: %+v", env)
	}
	if len(rec.commands) == 0 {
		t.Fatal("no commands recorded")
	}
	if len(rec.commands) > 2 {
		t.Errorf("the observation must be at most two remote commands, ran %d: %v", len(rec.commands), rec.commands)
	}
	if len(rec.uploads) != 0 {
		t.Errorf("siblings uploaded files: %v", rec.uploads)
	}
	if len(rec.inputs) != 0 {
		t.Errorf("siblings streamed stdin to commands: %v", rec.inputs)
	}
	banned := []string{"rm ", "mv ", "mkdir", "rmdir", "cp ", "touch", "truncate", "tee ", "chmod", "chown",
		"kill", "pkill", "reboot", "shutdown", "sed ", "dd ", "mkfs", "mount", "umount", "curl", "wget",
		"apt", "yum", "dnf", "pip", "useradd", "passwd", "systemctl ", ">>", "docker run", "docker stop",
		"docker rm", "docker rmi", "docker pull", "docker create", "docker exec", "docker build", "docker network"}
	for _, cmd := range rec.commands {
		for _, verb := range banned {
			if strings.Contains(cmd, verb) {
				t.Errorf("siblings ran a mutating command (%q): %s", verb, cmd)
			}
		}
		if strings.Contains(cmd, "docker ") {
			fields := strings.Fields(cmd)
			if len(fields) > 1 && fields[1] != "ps" && fields[1] != "inspect" && fields[1] != "image" {
				t.Errorf("siblings ran docker %s: %s", fields[1], cmd)
			}
		}
	}
	// Env and anything secret-adjacent must never ride along.
	for _, cmd := range rec.commands {
		if strings.Contains(cmd, "--env") || strings.Contains(cmd, "secret") {
			t.Errorf("siblings touched env or secrets: %s", cmd)
		}
	}
}

func TestForgejoFromOCILabels(t *testing.T) {
	for _, tt := range []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"forgejo source", map[string]string{"org.opencontainers.image.source": "https://codeberg.org/forgejo/forgejo"}, true},
		{"forgejo title", map[string]string{"org.opencontainers.image.title": "forgejo/forgejo binary"}, true},
		{"gitea source", map[string]string{"org.opencontainers.image.source": "https://github.com/go-gitea/gitea"}, true},
		{"case insensitive", map[string]string{"org.opencontainers.image.source": "https://Codeberg.org/Forgejo/Forgejo"}, true},
		{"unrelated", map[string]string{"org.opencontainers.image.source": "https://github.com/docker-library/postgres"}, false},
		{"no labels", nil, false},
	} {
		if got := forgejoFromOCILabels(tt.labels); got != tt.want {
			t.Errorf("%s: forgejoFromOCILabels = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestServerSiblings_EmptyServerNeedsNoSecondRound(t *testing.T) {
	// With no containers there is nothing to inspect; no registration for
	// the inspect exists, so a second round would fail the mock — proving
	// it never ran.
	fx := siblingsFixture{containers: "", noInspect: true}
	env := collectServerSiblings(context.Background(), fx.exec(), "infra-home", "192.0.2.10", siblingsFixtureNow)
	if env.State != auditStateOK || len(env.Siblings) != 0 || len(env.UnknownLabeled) != 0 {
		t.Fatalf("empty server = %+v", env)
	}
}
