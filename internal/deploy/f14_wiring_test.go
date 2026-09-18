package deploy

// Behavioral tests for the F14 wiring: deploy records the release spec,
// rollback restores from the record (backfilling pre-store releases), the
// recorded primary port drives health checks and Caddy upstreams (TCL-14),
// publish deploys/rollbacks take the explicit recreate strategy (F21), and
// state-only static rollback works from the record (F13).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
)

const f14Inspect = `[{
  "Image": "sha256:%s",
  "Config": {
    "Image": "myapp:v9",
    "Env": ["PORT=3000", "API_KEY=x"],
    "Cmd": ["npm", "start"],
    "Labels": {"teploy.app": "myapp", "teploy.version": "v9", "teploy.process": "web"},
    "Healthcheck": {"Test": ["NONE"]}
  },
  "HostConfig": {
    "NetworkMode": "teploy",
    "PortBindings": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "49152"}]},
    "Binds": ["/deployments/myapp/volumes/data:/data"],
    "RestartPolicy": {"Name": "unless-stopped"},
    "Memory": 268435456,
    "LogConfig": {"Type": "json-file", "Config": {"max-size": "10m"}}
  },
  "NetworkSettings": {"Networks": {"teploy": {"Aliases": ["myapp"]}}}
}]`

func withDigest(s string) string { return fmt.Sprintf(s, strings.Repeat("9", 64)) }

// deployRecordFixture is the standard successful-deploy mock. The web
// container's full inspect is registered BEFORE the generic "docker inspect"
// status fixture so InspectRecreate sees real JSON. Extra fixtures are
// registered FIRST (the mock matches in order), so per-test overrides of
// base fixtures win.
func deployRecordFixture(imageDigest string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	base := []ssh.MockCommand{
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "absent"},
		{Match: "ss -tln", Output: ssOutput},
		{Match: "docker ps --all --filter label=teploy.app='myapp'", Output: ""},
		{Match: "docker run", Output: "abc123def456"},
		{Match: "docker inspect 'myapp-web-v9'", Output: withDigest(f14Inspect)},
		{Match: "docker inspect -f '{{.Image}}'", Output: imageDigest},
		{Match: "docker inspect", Output: "running"},
		{Match: "curl -s -o /dev/null", Output: "200"},
		{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		{Match: "curl -sf -X PATCH", Err: fmt.Errorf("not found")},
		{Match: "curl -sf -X POST http://localhost:2019/config/apps/http/servers/srv0/routes", Output: ""},
		{Match: "rm -f /tmp/teploy_caddy", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		{Match: "printf %s", Output: ""},
		{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
		{Match: "UPLOAD:", Output: ""},
		{Match: "mkdir -p", Output: ""},
		{Match: "mv", Output: ""},
	}
	return ssh.NewMockExecutor("1.2.3.4", append(extra, base...)...)
}

func TestDeploy_RecordsReleaseMetadata(t *testing.T) {
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	mock := deployRecordFixture(imageDigest)

	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:           "myapp",
		Domain:        "myapp.com",
		Image:         "myapp:v9",
		Version:       "v9",
		ContainerPort: 3000,
		Publish:       []string{"0.0.0.0:3001:3001"},
		EnvFiles:      []string{"/deployments/myapp/.env", "/deployments/myapp/.deploy-env"},
		Env:           map[string]string{"PLAIN": "1"},
		Volumes:       map[string]string{"/deployments/myapp/volumes/data": "/data"},
		Memory:        "256m",
		CPU:           "1.5",
		Health:        HealthConfig{Path: "/ready", Timeout: 45 * time.Second, Interval: 2 * time.Second},
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}

	raw, ok := mock.Files["/deployments/myapp/meta/v9.json"]
	if !ok {
		t.Fatalf("release record not written\n%s", buf.String())
	}
	var rec releasemeta.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("invalid record: %v\n%s", err, raw)
	}

	if rec.App != "myapp" || rec.Hash != "v9" || rec.DeploymentType != "container" || rec.IngressMode != "caddy" || rec.Domain != "myapp.com" {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.ImageRef != "myapp:v9" || rec.ImageDigest != imageDigest {
		t.Errorf("image identity not recorded: %s / %s", rec.ImageRef, rec.ImageDigest)
	}
	if len(rec.EnvFiles) != 2 || rec.EnvFiles[0] != "/deployments/myapp/.env" {
		t.Errorf("env-file references not recorded: %v", rec.EnvFiles)
	}
	if rec.Env["PLAIN"] != "1" || rec.Volumes["/deployments/myapp/volumes/data"] != "/data" {
		t.Errorf("env/volumes not recorded: %v %v", rec.Env, rec.Volumes)
	}
	if rec.Memory != "256m" || rec.CPU != "1.5" {
		t.Errorf("resource limits not recorded: %s %s", rec.Memory, rec.CPU)
	}
	if rec.Health == nil || rec.Health.Path != "/ready" || rec.Health.TimeoutSeconds != 45 {
		t.Errorf("health gate not recorded: %+v", rec.Health)
	}
	if rec.Caddy == nil {
		t.Fatalf("caddy route refs not recorded")
	}
	// Ports: primary (3000 on the allocated ephemeral host port) plus the
	// parsed publish entry, flagged fixed (TCL-14/F21 contract).
	var primary, publish *releasemeta.Port
	for i := range rec.Ports {
		switch {
		case rec.Ports[i].Primary:
			primary = &rec.Ports[i]
		case rec.Ports[i].ContainerPort == 3001:
			publish = &rec.Ports[i]
		}
	}
	if primary == nil || primary.ContainerPort != 3000 || primary.HostPort != 49152 {
		t.Errorf("primary port not recorded with its host binding: %+v", rec.Ports)
	}
	if primary.Fixed {
		t.Errorf("a caddy-ingress primary is ephemeral blue/green, not fixed: %+v", *primary)
	}
	if publish == nil || !publish.Fixed || publish.HostPort != 3001 || publish.Bind != "0.0.0.0" {
		t.Errorf("publish entry not recorded as a fixed port: %+v", rec.Ports)
	}
	if len(rec.Publish) != 1 || rec.Publish[0] != "0.0.0.0:3001:3001" {
		t.Errorf("raw publish specs not recorded: %v", rec.Publish)
	}
	if rec.Recreate == nil || rec.Recreate.Name != "myapp-web-v9" || !rec.Recreate.NoHealthcheck {
		t.Errorf("recreate spec not embedded from docker's view: %+v", rec.Recreate)
	}
}

// A record-write failure is degraded rollback metadata, never a failed
// deploy — the containers are live and routed by the time it happens.
func TestDeploy_RecordWriteFailureIsAWarning(t *testing.T) {
	mock := deployRecordFixture("sha256:"+strings.Repeat("a", 64),
		ssh.MockCommand{Match: "UPLOAD:/deployments/myapp/meta/", Err: fmt.Errorf("disk full")},
	)
	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App: "myapp", Domain: "myapp.com", Image: "myapp:v9", Version: "v9",
	})
	if err != nil {
		t.Fatalf("deploy must not fail on a record-write failure: %v", err)
	}
	if !strings.Contains(buf.String(), "Warning: could not record release metadata") {
		t.Errorf("expected a warning about the unwritten record:\n%s", buf.String())
	}
}

func TestDeploy_PublishRecreatesByDisplacement(t *testing.T) {
	currentState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-09-18T10:00:00Z","current_port":49152,"current_hash":"v8"}`
	mock := deployRecordFixture("sha256:"+strings.Repeat("a", 64),
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + currentState},
		// The displacement inventory: v8's web container is running and
		// holds the fixed publish port.
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=myapp --filter label=teploy.process=web",
			Output: "myapp-web-v8\n"},
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)

	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v9",
		Version: "v9",
		Publish: []string{"0.0.0.0:3001:3001"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v\n%s", err, buf.String())
	}

	// The publish port is verbatim on the new container's docker run.
	var runCmd string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker run") && strings.Contains(c, "myapp-web-v9") {
			runCmd = c
		}
	}
	if runCmd == "" || !strings.Contains(runCmd, "-p '0.0.0.0:3001:3001'") {
		t.Fatalf("publish spec missing from the new container's run:\n%s", runCmd)
	}

	// F21's core: the current web container was stopped BEFORE the
	// replacement started — blue/green would have died on the fixed port.
	stopIdx, runIdx := -1, -1
	for i, c := range mock.Calls {
		if stopIdx < 0 && strings.HasPrefix(c, "docker stop") && strings.Contains(c, "myapp-web-v8") {
			stopIdx = i
		}
		if runIdx < 0 && strings.HasPrefix(c, "docker run") && strings.Contains(c, "myapp-web-v9") {
			runIdx = i
		}
	}
	if stopIdx < 0 {
		t.Fatalf("the current web container was never displaced\ncalls: %v", mock.Calls)
	}
	if runIdx < 0 || stopIdx > runIdx {
		t.Errorf("displacement (idx %d) must precede the replacement run (idx %d)", stopIdx, runIdx)
	}
}

func TestDeploy_PublishRecreateFailureRestoresDisplaced(t *testing.T) {
	currentState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-09-18T10:00:00Z","current_port":49152,"current_hash":"v8"}`
	mock := deployRecordFixture("sha256:"+strings.Repeat("a", 64),
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + currentState},
		ssh.MockCommand{Match: "docker ps --filter label=teploy.app=myapp --filter label=teploy.process=web",
			Output: "myapp-web-v8\n"},
		// The new container cannot start (its fixed publish port is still
		// held); the once-flag lets the displaced workload's restore run
		// succeed afterwards.
		ssh.MockCommand{Match: "docker run", Err: fmt.Errorf("port already allocated"), Once: true},
		// The displaced workload's restore: full inspect (Restart), rm -f,
		// and a docker run that succeeds again.
		ssh.MockCommand{Match: "docker inspect 'myapp-web-v8'", Output: `[{"Config":{"Image":"myapp:v8","Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		ssh.MockCommand{Match: "docker rm -f 'myapp-web-v8'", Output: ""},
		ssh.MockCommand{Match: "docker run", Err: nil},
		ssh.MockCommand{Match: "docker stop", Output: ""},
	)
	var buf bytes.Buffer
	err := NewDeployer(mock, &buf).Deploy(context.Background(), Config{
		App:     "myapp",
		Domain:  "myapp.com",
		Image:   "myapp:v9",
		Version: "v9",
		Publish: []string{"0.0.0.0:3001:3001"},
	})
	if err == nil {
		t.Fatalf("expected the deploy to fail\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Restored myapp-web-v8") {
		t.Errorf("the displaced fixed-port workload was not restored:\n%s", buf.String())
	}
}

// rollbackFixtureCommands is the standard two-version rollback command
// table; record is the framed meta-read response ("absent", or
// "present\n<json>"). Compose with extra fixtures prepended when a test
// needs additional inspect forms.
func rollbackFixtureCommands(record string) []ssh.MockCommand {
	stateContent := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-07-22T10:00:00Z","current_port":49153,"current_hash":"v2","previous_port":49152,"previous_hash":"v1"}`
	return []ssh.MockCommand{
		{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		{Match: "mkdir -p /deployments/myapp", Output: ""},
		{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		{Match: "cat /deployments/myapp/.lock/info", Err: fmt.Errorf("none")},
		{Match: "if [ ! -e '/deployments/myapp/meta/v1.json' ]", Output: record},
		{Match: "docker ps --all --filter label=teploy.app='myapp'",
			Output: `{"ID":"aaa","Names":"myapp-web-v1","Image":"myapp:latest","State":"exited","Status":"Exited","Labels":"teploy.app=myapp,teploy.version=v1,teploy.process=web"}` + "\n" +
				`{"ID":"bbb","Names":"myapp-web-v2","Image":"myapp:latest","State":"running","Status":"Up 1h","Labels":"teploy.app=myapp,teploy.version=v2,teploy.process=web"}`,
		},
		{Match: "docker inspect 'myapp-web-v1'", Output: `[{"Image":"sha256:` + strings.Repeat("1", 64) + `","Config":{"Image":"myapp:v1","Env":["PORT=3000"],"Labels":{"teploy.app":"myapp"}},"HostConfig":{"NetworkMode":"teploy","PortBindings":{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]},"RestartPolicy":{"Name":"no"}},"NetworkSettings":{"Networks":{"teploy":{"Aliases":["myapp"]}}}}]`},
		{Match: "docker rm -f 'myapp-web-v1'", Output: ""},
		{Match: "docker run", Output: ""},
		{Match: "curl -s -o /dev/null", Output: "200"},
		{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}", Output: "127.0.0.1 "},
		{Match: "docker inspect -f '{{range $p, $b := .NetworkSettings.Ports}}", Output: "49152"},
		{Match: "docker inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}", Output: "3000/tcp"},
		{Match: "caddy", Output: ""},
		{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		{Match: "docker exec caddy caddy reload", Output: ""},
		{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		{Match: "docker stop", Output: ""},
		{Match: "mkdir -p", Output: ""},
		{Match: "cat /tmp", Output: ""},
		{Match: "UPLOAD:", Output: ""},
	}
}

func rollbackRecordFixture(record string) *ssh.MockExecutor {
	return ssh.NewMockExecutor("1.2.3.4", rollbackFixtureCommands(record)...)
}

func marshalRecord(t *testing.T, rec *releasemeta.Record) string {
	t.Helper()
	rec.SchemaVersion = releasemeta.SchemaVersion
	if rec.App == "" {
		rec.App = "myapp"
	}
	if rec.Hash == "" {
		rec.Hash = "v1"
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return "present\n" + string(data)
}

func TestRollback_RestoresFromRecordedSpec(t *testing.T) {
	rec := &releasemeta.Record{
		DeploymentType: "container", IngressMode: "caddy", Domain: "rec.example.com",
		Health: &releasemeta.Health{Path: "/deep", TimeoutSeconds: 30, IntervalSeconds: 1},
		Caddy: &releasemeta.CaddyRoute{
			TLSInternal: true,
			CaddyExtra:  "header X-Recorded yes",
			Cache:       map[string]string{"assets/*": "max-age=60"},
		},
	}
	mock := rollbackRecordFixture(marshalRecord(t, rec))

	var buf bytes.Buffer
	cfg := rollbackCfg() // passes /health, no TLS, domain myapp.com — all wrong for v1
	if err := Rollback(context.Background(), mock, &buf, cfg); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, buf.String())
	}

	// The health probe used the recorded path, not the CLI-passed /health.
	var healthCurl string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl -s -o /dev/null") {
			healthCurl = c
		}
	}
	if !strings.Contains(healthCurl, "/deep") {
		t.Errorf("health probe did not use the recorded path:\n%s", healthCurl)
	}

	// The Caddy block carries the recorded edge config: the recorded domain,
	// internal TLS, the recorded extra directive, and the cache rule.
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	for _, want := range []string{"rec.example.com {", "tls internal", "header X-Recorded yes", "max-age=60"} {
		if !strings.Contains(caddyfile, want) {
			t.Errorf("restored route missing recorded %q:\n%s", want, caddyfile)
		}
	}
	if strings.Contains(caddyfile, "myapp.com {") {
		t.Errorf("rollback routed the CLI-passed domain instead of the recorded one:\n%s", caddyfile)
	}
}

func TestRollback_BackfillsRecordOnFirstUse(t *testing.T) {
	// No record on disk: the read comes back absent, Backfill synthesizes
	// one from the live v1 containers and persists it.
	mock := rollbackRecordFixture("absent")

	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, buf.String())
	}

	raw, ok := mock.Files["/deployments/myapp/meta/v1.json"]
	if !ok {
		t.Fatalf("backfilled record not persisted\n%s", buf.String())
	}
	var rec releasemeta.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("invalid backfilled record: %v", err)
	}
	if !rec.Backfilled {
		t.Error("record must be flagged backfilled")
	}
	// Primary port derived from the container's PORT env (3000).
	if p, ok := releasemeta.PrimaryContainerPort(&rec); !ok || p != 3000 {
		t.Errorf("primary container port not recovered from PORT env: %+v", rec.Ports)
	}
	if rec.Env["PORT"] != "3000" {
		t.Errorf("resolved env not captured: %v", rec.Env)
	}
	if rec.ImageDigest != "sha256:"+strings.Repeat("1", 64) {
		t.Errorf("image digest not captured: %s", rec.ImageDigest)
	}
	if rec.Health != nil || rec.Caddy != nil {
		t.Errorf("backfill must not invent health/caddy config: %+v %+v", rec.Health, rec.Caddy)
	}
}

// TCL-14: with a recorded primary container port, the health check probes
// that port's host binding and Caddy dials the primary container port — an
// auxiliary published listener can neither steal the probe nor the route.
func TestRollback_RecordedPrimaryPortDrivesHealthAndUpstream(t *testing.T) {
	rec := &releasemeta.Record{
		DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.com",
		Health: &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
		Ports: []releasemeta.Port{
			{HostPort: 49152, ContainerPort: 8080, Primary: true},
			{HostPort: 9100, ContainerPort: 9000, Bind: "0.0.0.0", Fixed: true},
		},
	}
	// HostPortFor reads the full ports map: 8080 is NOT the numerically
	// first key (9000 is), which is exactly the fields[0] ambiguity being
	// fixed.
	mock := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{
			{Match: "docker inspect -f '{{json .NetworkSettings.Ports}}'",
				Output: `{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"49160"}],"9000/tcp":[{"HostIp":"0.0.0.0","HostPort":"9100"}]}`},
		}, rollbackFixtureCommands(marshalRecord(t, rec))...)...)

	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, buf.String())
	}

	var healthCurl string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "curl -s -o /dev/null") {
			healthCurl = c
		}
	}
	if !strings.Contains(healthCurl, ":49160") {
		t.Errorf("health probe did not target the primary port's host binding (49160):\n%s", healthCurl)
	}
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(caddyfile, "myapp-web-v1:8080") {
		t.Errorf("Caddy upstream is not the recorded primary container port (8080):\n%s", caddyfile)
	}
}

// F21 rollback: a target release with recorded publish entries takes the
// recreate order — the current web containers stop before the target's
// containers restart, because the fixed publish ports cannot double-bind.
func TestRollback_FixedPortTargetDisplacesCurrent(t *testing.T) {
	rec := &releasemeta.Record{
		DeploymentType: "container", IngressMode: "caddy", Domain: "myapp.com",
		Health:  &releasemeta.Health{Path: "/health", TimeoutSeconds: 30, IntervalSeconds: 1},
		Publish: []string{"0.0.0.0:3001:3001"},
		Ports: []releasemeta.Port{
			{HostPort: 49152, ContainerPort: 3000, Primary: true},
			{HostPort: 3001, ContainerPort: 3001, Bind: "0.0.0.0", Fixed: true},
		},
	}
	mock := ssh.NewMockExecutor("1.2.3.4",
		append([]ssh.MockCommand{
			{Match: "docker inspect -f '{{json .NetworkSettings.Ports}}'",
				Output: `{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"49152"}]}`},
		}, rollbackFixtureCommands(marshalRecord(t, rec))...)...)

	var buf bytes.Buffer
	if err := Rollback(context.Background(), mock, &buf, rollbackCfg()); err != nil {
		t.Fatalf("Rollback: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "Freed the fixed port") {
		t.Errorf("current web container was not displaced for the fixed-port target:\n%s", buf.String())
	}
	stopIdx, runIdx := -1, -1
	for i, c := range mock.Calls {
		if stopIdx < 0 && strings.HasPrefix(c, "docker stop") && strings.Contains(c, "myapp-web-v2") {
			stopIdx = i
		}
		if runIdx < 0 && strings.HasPrefix(c, "docker run") && strings.Contains(c, "myapp-web-v1") {
			runIdx = i
		}
	}
	if stopIdx < 0 || runIdx < 0 || stopIdx > runIdx {
		t.Errorf("displacement (idx %d) must precede the target recreation (idx %d)", stopIdx, runIdx)
	}
}

// --- static: F13 state-only rollback on top of the F14 record ---

// staticMetaFixture returns the framed meta-read response for hash.
func staticMetaFixture(t *testing.T, rec *releasemeta.Record, hash string) string {
	t.Helper()
	rec.SchemaVersion = releasemeta.SchemaVersion
	rec.App = "myapp"
	rec.Hash = hash
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return "present\n" + string(data)
}

func TestStaticDeploy_RecordsReleaseMetadata(t *testing.T) {
	src := staticTestSource(t)
	hash, err := hashDir(src)
	if err != nil {
		t.Fatal(err)
	}
	shortHash := hash[:12]

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "absent"},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state' ]", Output: "present\n" + ""},
		ssh.MockCommand{Match: "mkdir -p /deployments/myapp/releases", Output: ""},
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/", Output: "yes"},
		ssh.MockCommand{Match: "ln -s -- releases/", Output: ""},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X DELETE", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "ls -1t /deployments/myapp/releases", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
		ssh.MockCommand{Match: "cat", Output: ""},
	)

	var buf bytes.Buffer
	err = NewStaticDeployer(mock, &buf).Deploy(context.Background(), StaticConfig{
		App:    "myapp",
		Domain: "myapp.com",
		Source: src,
		SPA:    true,
		Headers: map[string]string{"X-Static": "1"},
		Cache:  map[string]string{"assets/*": "max-age=3600"},
	})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	raw, ok := mock.Files["/deployments/myapp/meta/"+shortHash+".json"]
	if !ok {
		t.Fatalf("static release record not written for %s", shortHash)
	}
	var rec releasemeta.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("invalid static record: %v", err)
	}
	if rec.DeploymentType != "static" || rec.IngressMode != "caddy" || rec.Hash != shortHash {
		t.Errorf("identity fields wrong: %+v", rec)
	}
	if rec.Static == nil {
		t.Fatalf("static serving config not recorded")
	}
	if !rec.Static.SPA || rec.Static.Domain != "myapp.com" ||
		rec.Static.Headers["X-Static"] != "1" || rec.Static.Cache["assets/*"] != "max-age=3600" {
		t.Errorf("serving config not recorded faithfully: %+v", rec.Static)
	}
}

func TestRollbackStateOnly_RestoresRecordedServingConfig(t *testing.T) {
	stateContent := `{"schema_version":2,"deployment_type":"static","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-09-18T10:00:00Z","current_hash":"aaa111","previous_hash":"bbb222"}`
	rec := &releasemeta.Record{
		DeploymentType: "static", IngressMode: "caddy",
		Static: &releasemeta.Static{
			Domain:      "rec.example.com",
			SPA:         true,
			SPAFallback: "/index.html",
			Headers:     map[string]string{"X-Rec": "1"},
			Cache:       map[string]string{"assets/*": "max-age=99"},
			CaddyExtra:  "header X-Extra 2",
		},
	}
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/bbb222.json' ]", Output: staticMetaFixture(t, rec, "bbb222")},
		ssh.MockCommand{Match: "test -d /deployments/myapp/releases/bbb222", Output: "yes"},
		ssh.MockCommand{Match: "ln -s -- releases/", Output: ""},
		ssh.MockCommand{Match: "curl -sf http://localhost:2019/config/apps/http/servers/srv0", Output: `{"listen":[":80",":443"]}`},
		ssh.MockCommand{Match: "curl -sf -X DELETE", Err: fmt.Errorf("not found")},
		ssh.MockCommand{Match: "cat /deployments/caddy/Caddyfile", Output: "{\n\tadmin 0.0.0.0:2019\n}\n"},
		ssh.MockCommand{Match: "mv /tmp/teploy_caddyfile.tmp", Output: ""},
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
		ssh.MockCommand{Match: "rmdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "printf %s", Output: ""},
		ssh.MockCommand{Match: "rm -rf /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "mv -Tf", Output: ""},
		ssh.MockCommand{Match: "mv", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)

	var buf bytes.Buffer
	if err := NewStaticDeployer(mock, &buf).RollbackStateOnly(context.Background(), "myapp", ""); err != nil {
		t.Fatalf("RollbackStateOnly: %v\n%s", err, buf.String())
	}

	// The Caddyfile block was rebuilt from the RECORD: recorded domain, SPA
	// fallback, headers, cache, and extra directive — none of which exist in
	// state.json.
	caddyfile := string(mock.Files["/deployments/caddy/Caddyfile"])
	for _, want := range []string{"rec.example.com {", "try_files {path} {path}/ {path}/index.html /index.html", "X-Rec", "max-age=99", "header X-Extra 2"} {
		if !strings.Contains(caddyfile, want) {
			t.Errorf("restored static block missing recorded %q:\n%s", want, caddyfile)
		}
	}
	if strings.Contains(caddyfile, "myapp.com {") {
		t.Errorf("state's domain was served instead of the recorded one:\n%s", caddyfile)
	}

	// The symlink flipped to the target release and state swapped.
	var swap string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "ln -s -- releases/") {
			swap = c
		}
	}
	if !strings.Contains(swap, "releases/bbb222") {
		t.Errorf("current symlink did not flip to bbb222: %s", swap)
	}
	stateData := writtenState(t, mock, "myapp")
	if stateData.CurrentHash != "bbb222" || stateData.PreviousHash != "aaa111" {
		t.Errorf("state not swapped: %+v", stateData)
	}
}

// Without a record there is nothing faithful to restore — static serving
// config is unrecoverable from disk (F48 territory) — so the request is a
// redeploy, never a guess.
func TestRollbackStateOnly_NoRecordFailsClosed(t *testing.T) {
	stateContent := `{"schema_version":2,"deployment_type":"static","ingress_mode":"caddy","domain":"myapp.com","updated_at":"2026-09-18T10:00:00Z","current_hash":"aaa111","previous_hash":"bbb222"}`
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "mkdir /deployments/myapp/.lock", Output: ""},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/state.json' ]", Output: "present\n" + stateContent},
		ssh.MockCommand{Match: "if [ ! -e '/deployments/myapp/meta/bbb222.json' ]", Output: "absent"},
	)

	err := NewStaticDeployer(mock, &bytes.Buffer{}).RollbackStateOnly(context.Background(), "myapp", "")
	if err == nil || !strings.Contains(err.Error(), "no recorded release metadata") {
		t.Fatalf("expected fail-closed no-record error, got %v", err)
	}
	if !strings.Contains(err.Error(), "redeploy") {
		t.Errorf("error must say what to do next: %v", err)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "ln -s -- releases/") {
			t.Errorf("the symlink was touched despite refusing: %s", c)
		}
	}
}
