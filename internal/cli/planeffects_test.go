package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/state"
)

// manifestFixture builds a deployed manifest view from a config — the
// writer/reader pair is round-trip-tested in internal/config.
func manifestView(t *testing.T, cfg *config.AppConfig, appliedImage string) *config.AppliedManifestView {
	t.Helper()
	data, _, err := config.NormalizeAndDigest(cfg, appliedImage)
	if err != nil {
		t.Fatalf("NormalizeAndDigest: %v", err)
	}
	view, err := config.ParseAppliedManifest(data)
	if err != nil {
		t.Fatalf("ParseAppliedManifest: %v", err)
	}
	return view
}

func findEffect(effects []planEffect, name string) *planEffect {
	for i := range effects {
		if effects[i].Name == name {
			return &effects[i]
		}
	}
	return nil
}

func TestRoutingEffects(t *testing.T) {
	deployed := &config.AppConfig{App: "blog", Domain: "old.example.com", Port: 3000, Publish: []string{"127.0.0.1:9100:9000"}}
	current := &state.AppState{Domain: "old.example.com", IngressMode: "caddy", CurrentPort: 3000}
	view := manifestView(t, deployed, "img:old")

	planned := &config.AppConfig{App: "blog", Domain: "new.example.com,alt.example.com", Port: 8080}
	effects := routingEffects(planned, current, view)

	if e := findEffect(effects, "domain"); e == nil || e.Action != "change" || e.From != "old.example.com" || e.To != "alt.example.com,new.example.com" {
		t.Errorf("domain effect wrong: %+v", e)
	}
	if e := findEffect(effects, "application port"); e == nil || e.Action != "change" || e.From != "3000" || e.To != "8080" {
		t.Errorf("port effect wrong: %+v", e)
	}
	if e := findEffect(effects, "publish 127.0.0.1:9100:9000"); e == nil || e.Action != "remove" {
		t.Errorf("publish removal missing: %+v", e)
	}
	if e := findEffect(effects, "ingress mode"); e != nil {
		t.Errorf("unchanged ingress must not appear: %+v", e)
	}

	// Domain order and case are normalized: not drift.
	same := routingEffects(&config.AppConfig{App: "blog", Domain: "OLD.EXAMPLE.COM", Port: 3000}, current, view)
	if findEffect(same, "domain") != nil {
		t.Errorf("case-folded identical domain reported as drift: %+v", same)
	}

	// Ingress mode change.
	modeChange := routingEffects(&config.AppConfig{App: "blog", Domain: "old.example.com", Port: 3000, Ingress: config.IngressHost},
		current, view)
	if e := findEffect(modeChange, "ingress mode"); e == nil || e.From != "caddy" || e.To != "host" {
		t.Errorf("ingress change wrong: %+v", e)
	}

	// First deploy: everything is an add.
	first := routingEffects(planned, nil, nil)
	if e := findEffect(first, "domain"); e == nil || e.Action != "add" {
		t.Errorf("first-deploy domain must be an add: %+v", e)
	}
	if e := findEffect(first, "application port"); e == nil || e.Action != "add" || e.To != "8080" {
		t.Errorf("first-deploy port must be an add: %+v", e)
	}
}

func TestEnvEffects(t *testing.T) {
	deployed := &config.AppConfig{App: "blog", Port: 80, Env: map[string]string{"KEEP": "1", "GONE": "1"}, EnvFiles: []string{".env.old"}}
	view := manifestView(t, deployed, "")

	planned := &config.AppConfig{App: "blog", Port: 80, Env: map[string]string{"KEEP": "2", "NEW": "3"}, EnvFiles: []string{".env.new"}}
	effects := envEffects(planned, view)

	if e := findEffect(effects, "env KEEP"); e != nil {
		t.Errorf("unchanged key reported: %+v", e)
	}
	if e := findEffect(effects, "env GONE"); e == nil || e.Action != "remove" {
		t.Errorf("removed key missing: %+v", e)
	}
	if e := findEffect(effects, "env NEW"); e == nil || e.Action != "add" {
		t.Errorf("added key missing: %+v", e)
	}
	if e := findEffect(effects, "env_file .env.old"); e == nil || e.Action != "remove" {
		t.Errorf("removed env file missing: %+v", e)
	}
	if e := findEffect(effects, "env_file .env.new"); e == nil || e.Action != "add" {
		t.Errorf("added env file missing: %+v", e)
	}
	if e := findEffect(effects, "env values"); e == nil {
		t.Errorf("env-file values note missing: %+v", e)
	}
	// Values never appear.
	for _, e := range effects {
		if e.From == "1" || e.To == "2" || e.To == "3" {
			t.Errorf("env VALUE leaked into plan: %+v", e)
		}
	}
}

func TestStorageEffects(t *testing.T) {
	deployed := &config.AppConfig{App: "blog", Port: 80, Volumes: map[string]string{
		"keep":          "/data/keep",
		"gone":          "/data/gone",
		"moved":         "/data/old-path",
		"/srv/hostbind": "/data/bind",
	}}
	view := manifestView(t, deployed, "")

	planned := &config.AppConfig{App: "blog", Port: 80, Volumes: map[string]string{
		"keep":          "/data/keep",
		"moved":         "/data/new-path",
		"new":           "/data/new",
		"/srv/hostbind": "/data/bind",
	}}
	effects := storageEffects("blog", planned, view)

	if e := findEffect(effects, "volume keep"); e != nil {
		t.Errorf("unchanged volume reported: %+v", e)
	}
	if e := findEffect(effects, "volume gone"); e == nil || e.Action != "remove" {
		t.Errorf("removed volume missing: %+v", e)
	}
	if e := findEffect(effects, "volume new"); e == nil || e.Action != "add" || !strings.Contains(e.Detail, "/deployments/blog/volumes/new") {
		t.Errorf("added volume must name the managed host path: %+v", e)
	}
	if e := findEffect(effects, "volume moved"); e == nil || e.Action != "change" || e.From != "/data/old-path" || e.To != "/data/new-path" {
		t.Errorf("moved volume wrong: %+v", e)
	}
	if e := findEffect(effects, "volume /srv/hostbind"); e != nil {
		t.Errorf("unchanged host bind reported: %+v", e)
	}
}

func TestResourceEffects(t *testing.T) {
	deployed := &config.AppConfig{App: "blog", Port: 80, Replicas: 2, Memory: "256m", CPU: "0.5"}
	view := manifestView(t, deployed, "")

	planned := &config.AppConfig{App: "blog", Port: 80, Replicas: 4, Memory: "1g", CPU: "0.5"}
	effects := resourceEffects(planned, view)

	if e := findEffect(effects, "replicas"); e == nil || e.Action != "change" || e.From != "2" || e.To != "4" {
		t.Errorf("replicas wrong: %+v", e)
	}
	if e := findEffect(effects, "memory limit"); e == nil || e.From != "256m" || e.To != "1g" {
		t.Errorf("memory wrong: %+v", e)
	}
	if e := findEffect(effects, "cpu limit"); e != nil {
		t.Errorf("unchanged cpu reported: %+v", e)
	}

	// First deploy: adds against an absent current.
	first := resourceEffects(&config.AppConfig{App: "blog", Port: 80, Replicas: 3}, nil)
	if e := findEffect(first, "replicas"); e == nil || e.Action != "add" || e.To != "3" {
		t.Errorf("first-deploy replicas must be an add: %+v", e)
	}
}

func TestAccessoryEffects(t *testing.T) {
	deployed := &config.AppConfig{App: "blog", Port: 80, Accessories: map[string]config.AccessoryConfig{
		"db":   {Image: "postgres:15", Port: 5432},
		"gone": {Image: "redis:7", Port: 6379},
	}}
	view := manifestView(t, deployed, "")

	planned := &config.AppConfig{App: "blog", Port: 80, Accessories: map[string]config.AccessoryConfig{
		"db":  {Image: "postgres:16", Port: 5432},
		"new": {Image: "meilisearch:v1.8", Port: 7700, Volumes: map[string]string{"msdata": "/var/lib/ms"}},
	}}
	effects := accessoryEffects(planned, view)

	if e := findEffect(effects, "accessory db"); e == nil || e.Action != "change" || e.From != "postgres:15" || e.To != "postgres:16" {
		t.Errorf("db image change wrong: %+v", e)
	}
	if e := findEffect(effects, "accessory gone"); e == nil || e.Action != "remove" {
		t.Errorf("removed accessory missing: %+v", e)
	}
	if e := findEffect(effects, "accessory new"); e == nil || e.Action != "add" || e.To != "meilisearch:v1.8" {
		t.Errorf("added accessory missing: %+v", e)
	}
}

func TestPlanImageIdentity_Classification(t *testing.T) {
	cases := []struct {
		name       string
		prov       releasemetaProv
		image      string
		needsBuild bool
		wantRes    string
		wantDigest string
	}{
		{
			name:       "digest-pinned",
			prov:       releasemetaProv{DigestPinned: true, ImageDigest: "sha256:aaaa"},
			image:      "repo/app@sha256:aaaa",
			wantRes:    imageResolvedByDigest,
			wantDigest: "sha256:aaaa",
		},
		{
			name:       "build",
			prov:       releasemetaProv{ContextFingerprint: "fp", DockerfileSHA256: "df"},
			needsBuild: true,
			wantRes:    imageUnresolvedAwaitingBuild,
		},
		{
			name:       "mutable resolved",
			prov:       releasemetaProv{ImageDigest: "sha256:bbbb"},
			image:      "repo/app:v1",
			wantRes:    imageResolvedByImageID,
			wantDigest: "sha256:bbbb",
		},
		{
			name:    "mutable unresolved",
			prov:    releasemetaProv{},
			image:   "repo/app:v1",
			wantRes: imageUnresolvedMutableTag,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := tc.prov.toProvenance()
			img := planImageIdentity(prov, tc.image, tc.needsBuild)
			if img.Resolution != tc.wantRes {
				t.Errorf("resolution = %s, want %s", img.Resolution, tc.wantRes)
			}
			if img.Digest != tc.wantDigest {
				t.Errorf("digest = %s, want %s", img.Digest, tc.wantDigest)
			}
			if tc.needsBuild && (img.ContextFingerprint != "fp" || img.DockerfileSHA256 != "df") {
				t.Errorf("build inputs not carried: %+v", img)
			}
		})
	}
}

// releasemetaProv is a test builder for the provenance fields
// planImageIdentity reads.
type releasemetaProv struct {
	DigestPinned       bool
	ImageDigest        string
	ContextFingerprint string
	DockerfileSHA256   string
}

func (p releasemetaProv) toProvenance() *releasemeta.Provenance {
	return &releasemeta.Provenance{
		DigestPinned:       p.DigestPinned,
		ImageDigest:        p.ImageDigest,
		ContextFingerprint: p.ContextFingerprint,
		DockerfileSHA256:   p.DockerfileSHA256,
	}
}

func TestPlanRecordJSON_Shape(t *testing.T) {
	rec := &PlanRecord{
		SchemaVersion: PlanRecordSchemaVersion,
		PlanID:        "abc123def4567890",
		App:           "blog", Server: "srv.example.com",
		TargetVersion: "v1", VersionKnown: true, ConfigDigest: "d1",
		Image:       PlanImageIdentity{Resolution: imageUnresolvedAwaitingBuild, NeedsBuild: true, ContextFingerprint: "fp"},
		TargetState: PlanTargetState{Deployed: false},
		Effects:     PlanEffects{Containers: []planChange{{Action: "create", Name: "blog-web-v1"}}},
		Unresolved:  []string{"image unresolved"},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema_version", "plan_id", "app", "server", "target_version", "version_known", "config_digest", "image", "target_state", "effects", "unresolved"} {
		if _, ok := m[key]; !ok {
			t.Errorf("plan record JSON missing %s: %s", key, data)
		}
	}
	// Old plan consumers saw these keys; they must stay.
	if _, ok := m["changes"]; ok {
		t.Error("top-level changes key should live under effects.containers now — but compatibility keys must not be silently renamed")
	}
	img := m["image"].(map[string]any)
	if img["resolution"] != imageUnresolvedAwaitingBuild {
		t.Errorf("image.resolution = %v", img["resolution"])
	}
	if img["needs_build"] != true {
		t.Errorf("image.needs_build = %v", img["needs_build"])
	}
}
