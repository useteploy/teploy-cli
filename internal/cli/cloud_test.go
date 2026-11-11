package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/cloud"
	"github.com/useteploy/teploy/internal/config"
	cryptossh "golang.org/x/crypto/ssh"
)

const cloudTestKey = "tp_test_abcdef0123456789TOPSECRET"

// fakeCloud is an in-memory Teploy Cloud /api/v1.
type fakeCloud struct {
	mu            sync.Mutex
	servers       map[string]map[string]any
	keys          []map[string]any
	keyPosts      int
	createPosts   int
	idemKeys      []string
	failFirstPost bool
	pollsToActive int
	gets          int
	deleted       []string
}

func (f *fakeCloud) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, status int, data any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+cloudTestKey || r.Header.Get("X-Organization-Id") != "org_1" {
				w.WriteHeader(401)
				w.Write([]byte(`{"success":false,"error":{"code":"UNAUTHORIZED","message":"Invalid API key"}}`))
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("GET /api/v1/servers", auth(func(w http.ResponseWriter, r *http.Request) {
		list := []map[string]any{}
		for _, s := range f.servers {
			list = append(list, s)
		}
		ok(w, 200, map[string]any{"items": list, "total": len(list)})
	}))
	mux.HandleFunc("GET /api/v1/servers/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		s := f.servers[r.PathValue("id")]
		f.gets++
		if f.gets >= f.pollsToActive {
			s["status"] = "active"
			s["ipAddress"] = "203.0.113.7"
		}
		ok(w, 200, s)
	}))
	mux.HandleFunc("POST /api/v1/servers", auth(func(w http.ResponseWriter, r *http.Request) {
		f.createPosts++
		f.idemKeys = append(f.idemKeys, r.Header.Get("Idempotency-Key"))
		if f.failFirstPost && f.createPosts == 1 {
			w.WriteHeader(503)
			w.Write([]byte(`{"success":false,"error":{"code":"SERVICE_UNAVAILABLE","message":"try again"}}`))
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s := map[string]any{"id": "srv_1", "name": body["name"], "status": "provisioning", "ipAddress": nil,
			"provider": body["provider"], "providerRegion": body["region"], "providerSize": body["size"],
			"priceMonthlyCents": 600, "sshKeyId": body["sshKeyId"]}
		f.servers["srv_1"] = s
		ok(w, 201, s)
	}))
	mux.HandleFunc("DELETE /api/v1/servers/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.PathValue("id"))
		delete(f.servers, r.PathValue("id"))
		ok(w, 200, nil)
	}))
	mux.HandleFunc("GET /api/v1/providers/{provider}/sizes", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, 200, []map[string]any{{"id": "vc2-1c-1gb", "vcpus": 1, "memory": 1024, "disk": 25, "priceMonthlyCents": 600}})
	}))
	mux.HandleFunc("GET /api/v1/providers/{provider}/regions", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, 200, []map[string]any{{"id": "ewr", "name": "New Jersey", "country": "US", "available": true}})
	}))
	mux.HandleFunc("GET /api/v1/ssh-keys", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, 200, f.keys)
	}))
	mux.HandleFunc("POST /api/v1/ssh-keys", auth(func(w http.ResponseWriter, r *http.Request) {
		f.keyPosts++
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		k := map[string]any{"id": fmt.Sprintf("key_%d", f.keyPosts), "name": body["name"], "publicKey": body["publicKey"]}
		f.keys = append(f.keys, k)
		ok(w, 201, k)
	}))
	return mux
}

type cloudHarness struct {
	t        *testing.T
	fake     *fakeCloud
	home     string
	out      *bytes.Buffer
	env      *cloudEnv
	flags    *Flags
	confirms []string
	answer   bool
	setups   []string
	sshWaits []string
	pubKey   string
}

func newCloudHarness(t *testing.T) *cloudHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeCloud{servers: map[string]map[string]any{}, pollsToActive: 2}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	t.Setenv(cloud.BaseURLEnv, srv.URL)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(sshPub))) + " me@laptop"
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &cloudHarness{t: t, fake: fake, home: home, out: &bytes.Buffer{}, flags: &Flags{}, answer: true, pubKey: line}
	h.env = &cloudEnv{
		in:              strings.NewReader(""),
		out:             h.out,
		stdinIsTerminal: func() bool { return false },
		readHidden:      func(string) (string, error) { return "", io.EOF },
		confirm: func(p string) bool {
			h.confirms = append(h.confirms, p)
			return h.answer
		},
		waitSSH: func(_ context.Context, host, _ string) error {
			h.sshWaits = append(h.sshWaits, host)
			return nil
		},
		setup: func(f *Flags, host, name string, _ bool) error {
			h.setups = append(h.setups, f.User+"@"+host+" "+name)
			return nil
		},
		sleep:            func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		pollInterval:     time.Millisecond,
		provisionTimeout: time.Minute,
		createAttempts:   3,
	}
	return h
}

func (h *cloudHarness) run(args ...string) error {
	cmd := newCloudCmdWithEnv(h.flags, h.env)
	cmd.SetArgs(args)
	cmd.SetOut(h.out)
	cmd.SetErr(h.out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd.Execute()
}

func (h *cloudHarness) login() {
	h.t.Helper()
	h.env.in = strings.NewReader(cloudTestKey + "\n")
	if err := h.run("login", "--key-stdin", "--org", "org_1"); err != nil {
		h.t.Fatalf("login: %v", err)
	}
	h.out.Reset()
}

func TestCloudLoginStoresPrivateFileAndNeverEchoesKey(t *testing.T) {
	h := newCloudHarness(t)
	h.env.in = strings.NewReader(cloudTestKey + "\n")
	if err := h.run("login", "--key-stdin", "--org", "org_1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.home, ".teploy", "cloud.yml")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("cloud.yml mode %o, want 600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), cloudTestKey) || !strings.Contains(string(data), "org_1") {
		t.Errorf("cloud.yml missing credentials: %s", data)
	}
	if strings.Contains(h.out.String(), cloudTestKey) {
		t.Errorf("login output echoes the key: %s", h.out)
	}

	h.out.Reset()
	if err := h.run("status"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.out.String(), cloudTestKey) || !strings.Contains(h.out.String(), "works") || !strings.Contains(h.out.String(), "org_1") {
		t.Errorf("status output: %s", h.out)
	}

	h.out.Reset()
	if err := h.run("logout"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("logout left %s", path)
	}
}

func TestCloudLoginRejectsBadKeyWithoutStoring(t *testing.T) {
	h := newCloudHarness(t)
	h.env.in = strings.NewReader("tp_test_wrongwrongwrong\n")
	err := h.run("login", "--key-stdin", "--org", "org_1")
	if err == nil || !strings.Contains(err.Error(), "teploy cloud login") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "wrongwrong") {
		t.Errorf("error echoes the key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".teploy", "cloud.yml")); !os.IsNotExist(err) {
		t.Error("a rejected key was stored")
	}
}

func TestCloudLoginRefusesKeyFlag(t *testing.T) {
	h := newCloudHarness(t)
	h.flags.Key = cloudTestKey
	err := h.run("login", "--org", "org_1")
	if err == nil || strings.Contains(err.Error(), cloudTestKey) {
		t.Fatalf("got %v", err)
	}
}

func TestCloudServersTableAndJSON(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.servers["s1"] = map[string]any{"id": "s1", "name": "web", "status": "active", "ipAddress": "203.0.113.9",
		"provider": "vultr", "providerRegion": "ewr", "providerSize": "vc2-1c-1gb", "priceMonthlyCents": 600}
	if err := h.run("servers"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "web", "active", "203.0.113.9", "vultr", "ewr", "vc2-1c-1gb", "$6.00"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("table missing %q:\n%s", want, h.out)
		}
	}
	h.out.Reset()
	h.flags.JSON = true
	if err := h.run("servers"); err != nil {
		t.Fatal(err)
	}
	var got []cloud.Server
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil || len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("json: %v %s", err, h.out)
	}
}

func TestCloudSizesAndRegions(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	if err := h.run("sizes", "--provider", "vultr", "--region", "ewr"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "vc2-1c-1gb") || !strings.Contains(h.out.String(), "$6.00") {
		t.Errorf("sizes: %s", h.out)
	}
	h.out.Reset()
	if err := h.run("regions", "--provider", "vultr"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "ewr") || !strings.Contains(h.out.String(), "New Jersey") {
		t.Errorf("regions: %s", h.out)
	}
}

func TestCloudServerCreateFullFlow(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.failFirstPost = true

	err := h.run("server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if h.fake.keyPosts != 1 {
		t.Errorf("ssh key posts = %d, want 1", h.fake.keyPosts)
	}
	if got := h.fake.keys[0]["publicKey"]; got != h.pubKey {
		t.Errorf("registered key %q", got)
	}
	if len(h.confirms) != 1 || !strings.Contains(h.out.String(), "$6.00 per month") {
		t.Errorf("confirmations %v, output %s", h.confirms, h.out)
	}
	if h.fake.createPosts != 2 || h.fake.idemKeys[0] == "" || h.fake.idemKeys[0] != h.fake.idemKeys[1] {
		t.Errorf("create posts %d, idempotency keys %v", h.fake.createPosts, h.fake.idemKeys)
	}
	if h.fake.gets < 2 {
		t.Errorf("polled %d times", h.fake.gets)
	}
	servers, err := config.ListServers(filepath.Join(h.home, ".teploy", "servers.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if s := servers["web1"]; s.Host != "203.0.113.7" || s.User != "root" {
		t.Errorf("servers.yml entry %+v", s)
	}
	if len(h.sshWaits) != 1 || len(h.setups) != 1 || h.setups[0] != "root@203.0.113.7 web1" {
		t.Errorf("ssh waits %v, setups %v", h.sshWaits, h.setups)
	}
	if !strings.Contains(h.out.String(), "server: web1") {
		t.Errorf("no next step in output: %s", h.out)
	}
	if strings.Contains(h.out.String(), cloudTestKey) {
		t.Error("output contains the key")
	}

	// A second create reuses the registered key and refuses the existing name.
	err = h.run("server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second create: %v", err)
	}
	if h.fake.createPosts != 2 {
		t.Errorf("refused create still posted")
	}
	if err := h.run("server", "create", "web2", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes", "--no-setup"); err != nil {
		t.Fatal(err)
	}
	if h.fake.keyPosts != 1 {
		t.Errorf("key registered again: %d posts", h.fake.keyPosts)
	}
}

func TestCloudServerCreateNoSetupSkipsSetup(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	if err := h.run("server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes", "--no-setup"); err != nil {
		t.Fatal(err)
	}
	if len(h.setups) != 0 || len(h.sshWaits) != 0 {
		t.Errorf("setup ran: %v %v", h.setups, h.sshWaits)
	}
	if len(h.confirms) != 0 {
		t.Errorf("--yes still asked: %v", h.confirms)
	}
	servers, _ := config.ListServers(filepath.Join(h.home, ".teploy", "servers.yml"))
	if servers["web1"].Host != "203.0.113.7" {
		t.Errorf("servers.yml: %+v", servers)
	}
}

func TestCloudServerCreateDeclinedBuysNothing(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.answer = false
	err := h.run("server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb")
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("got %v", err)
	}
	if h.fake.createPosts != 0 {
		t.Error("declined create posted")
	}
}

func TestCloudServerCreateErrorStatusFails(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.pollsToActive = 1 << 30
	h.env.sleep = func(ctx context.Context, d time.Duration) error {
		h.fake.mu.Lock()
		if s := h.fake.servers["srv_1"]; s != nil {
			s["status"] = "error"
		}
		h.fake.mu.Unlock()
		return nil
	}
	err := h.run("server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes")
	if err == nil || !strings.Contains(err.Error(), "failed to provision") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".teploy", "servers.yml")); !os.IsNotExist(err) {
		t.Error("failed server written to servers.yml")
	}
}

func TestCloudServerDelete(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.servers["srv_1"] = map[string]any{"id": "srv_1", "name": "web1", "status": "active", "ipAddress": "203.0.113.7"}
	h.fake.servers["srv_2"] = map[string]any{"id": "srv_2", "name": "web2", "status": "active", "ipAddress": "203.0.113.8"}
	path := filepath.Join(h.home, ".teploy", "servers.yml")
	if err := config.AddServer(path, "web1", "203.0.113.7", "root", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := config.AddServer(path, "web2", "198.51.100.1", "root", "", ""); err != nil {
		t.Fatal(err)
	}

	h.answer = false
	if err := h.run("server", "delete", "web1"); err == nil {
		t.Fatal("declined delete succeeded")
	}
	if len(h.fake.deleted) != 0 || len(h.confirms) != 1 || !strings.Contains(h.confirms[0], "web1") {
		t.Fatalf("deleted %v confirms %v", h.fake.deleted, h.confirms)
	}

	h.answer = true
	if err := h.run("server", "delete", "web1"); err != nil {
		t.Fatal(err)
	}
	if err := h.run("server", "delete", "srv_2", "--yes"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.fake.deleted, ",") != "srv_1,srv_2" {
		t.Errorf("deleted %v", h.fake.deleted)
	}
	servers, _ := config.ListServers(path)
	if _, ok := servers["web1"]; ok {
		t.Error("web1 entry kept although its host matched")
	}
	if _, ok := servers["web2"]; !ok {
		t.Error("web2 entry removed although its host differs")
	}
}
