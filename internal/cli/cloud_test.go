package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
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

// fakeCloud is an in-memory Teploy Cloud /api/v1 that behaves like the
// real POST /api/v1/servers: it reads no Idempotency-Key, so every POST
// is a new purchase with a new id, and the server row (address 0.0.0.0)
// exists from the start of the request, before the answer is sent.
type fakeCloud struct {
	mu          sync.Mutex
	servers     map[string]map[string]any
	order       []string
	keys        []map[string]any
	keyPosts    int
	createPosts int
	idemKeys    []string
	lists       int
	// postDelay holds the create answer back (the row already exists).
	postDelay time.Duration
	// postStatus, when set, answers the create with this status and
	// postBody. postKeepsRow says whether the row stays (a 5xx after the
	// machine was found) or never existed (a refusal).
	postStatus   int
	postBody     string
	postKeepsRow bool
	// foreignOnPost is a row somebody else creates while the create runs.
	foreignOnPost map[string]any
	// pollsToActive: GETs before the server is active; pollsToAddress:
	// GETs before it has a real address (0.0.0.0 until then).
	pollsToActive  int
	pollsToAddress int
	gets           int
	deleted        []string
	release        chan struct{}
}

func (f *fakeCloud) addServer(s map[string]any) {
	id := s["id"].(string)
	if _, ok := f.servers[id]; !ok {
		f.order = append(f.order, id)
	}
	f.servers[id] = s
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
		f.lists++
		list := []map[string]any{}
		for _, id := range f.order {
			if s, ok := f.servers[id]; ok {
				list = append(list, s)
			}
		}
		ok(w, 200, list)
	}))
	mux.HandleFunc("GET /api/v1/servers/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		s, found := f.servers[r.PathValue("id")]
		if !found {
			w.WriteHeader(404)
			w.Write([]byte(`{"success":false,"error":{"code":"NOT_FOUND","message":"Server not found"}}`))
			return
		}
		f.gets++
		if f.gets >= f.pollsToActive && s["status"] == "provisioning" {
			s["status"] = "active"
		}
		if f.gets >= f.pollsToAddress && s["status"] == "active" {
			s["ipAddress"] = "203.0.113.7"
		}
		ok(w, 200, s)
	}))
	mux.HandleFunc("POST /api/v1/servers", auth(func(w http.ResponseWriter, r *http.Request) {
		f.createPosts++
		f.idemKeys = append(f.idemKeys, r.Header.Get("Idempotency-Key"))
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if f.foreignOnPost != nil {
			f.addServer(f.foreignOnPost)
		}
		if f.postStatus >= 400 && f.postStatus < 500 && !f.postKeepsRow {
			w.WriteHeader(f.postStatus)
			w.Write([]byte(f.postBody))
			return
		}
		s := map[string]any{"id": fmt.Sprintf("srv_%d", f.createPosts), "name": body["name"], "status": "provisioning",
			"ipAddress": "0.0.0.0", "provider": body["provider"], "providerRegion": body["region"],
			"providerSize": body["size"], "priceMonthlyCents": 600, "sshKeyId": body["sshKeyId"]}
		if f.postStatus == 0 || f.postKeepsRow {
			f.addServer(s)
		}
		if f.postDelay > 0 {
			f.mu.Unlock()
			select {
			case <-time.After(f.postDelay):
			case <-r.Context().Done():
			case <-f.release:
			}
			f.mu.Lock()
		}
		if f.postStatus != 0 {
			w.WriteHeader(f.postStatus)
			w.Write([]byte(f.postBody))
			return
		}
		ok(w, 201, s)
	}))
	mux.HandleFunc("DELETE /api/v1/servers/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.deleted = append(f.deleted, r.PathValue("id"))
		delete(f.servers, r.PathValue("id"))
		ok(w, 200, nil)
	}))
	mux.HandleFunc("GET /api/v1/providers", auth(func(w http.ResponseWriter, r *http.Request) {
		ok(w, 200, []map[string]any{{"type": "vultr", "name": "Vultr", "description": "Cloud servers", "docsUrl": "https://example.com", "managedAvailable": true}})
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
	errOut   *bytes.Buffer
	env      *cloudEnv
	flags    *Flags
	confirms []string
	answer   bool
	setups   []string
	setupKey []string
	sshWaits []string
	pubKey   string
}

// writeTestSSHKey writes an unencrypted ed25519 private key at path and
// its .pub (with comment), returning the authorized_keys line.
func writeTestSSHKey(t *testing.T, path, comment string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(sshPub)))
	if err := os.WriteFile(path+".pub", []byte(line+" "+comment+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return line
}

func newCloudHarness(t *testing.T) *cloudHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	fake := &fakeCloud{servers: map[string]map[string]any{}, pollsToActive: 2, pollsToAddress: 2, release: make(chan struct{})}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(fake.release) })
	t.Setenv(cloud.BaseURLEnv, srv.URL)

	line := writeTestSSHKey(t, filepath.Join(home, ".ssh", "id_ed25519"), "me@laptop")
	h := &cloudHarness{t: t, fake: fake, home: home, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, flags: &Flags{}, answer: true, pubKey: line}
	h.env = &cloudEnv{
		in:              strings.NewReader(""),
		out:             h.out,
		errOut:          h.errOut,
		stdinIsTerminal: func() bool { return false },
		readHidden:      func(string) (string, error) { return "", io.EOF },
		confirm: func(p string) bool {
			h.confirms = append(h.confirms, p)
			return h.answer
		},
		waitSSH: func(_ context.Context, host, key string) error {
			h.sshWaits = append(h.sshWaits, host+" "+key)
			return nil
		},
		setup: func(f *Flags, host, name string, _ bool) error {
			h.setups = append(h.setups, f.User+"@"+host+" "+name)
			h.setupKey = append(h.setupKey, f.Key)
			return nil
		},
		sleep:            func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		pollInterval:     time.Millisecond,
		provisionTimeout: time.Minute,
		confirmChecks:    3,
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
	h.fake.lists = 0 // login checks the key with a list
}

func (h *cloudHarness) serversYML() map[string]config.Server {
	h.t.Helper()
	servers, err := config.ListServers(filepath.Join(h.home, ".teploy", "servers.yml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		h.t.Fatal(err)
	}
	return servers
}

var createArgs = []string{"server", "create", "web1", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb"}

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
	h.fake.addServer(map[string]any{"id": "s1", "name": "web", "status": "active", "ipAddress": "203.0.113.9",
		"provider": "vultr", "providerRegion": "ewr", "providerSize": "vc2-1c-1gb", "priceMonthlyCents": 600})
	h.fake.addServer(map[string]any{"id": "s2", "name": "new", "status": "provisioning", "ipAddress": "0.0.0.0",
		"provider": "vultr", "providerRegion": "ewr", "providerSize": "vc2-1c-1gb", "priceMonthlyCents": 600})
	if err := h.run("servers"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "web", "active", "203.0.113.9", "vultr", "ewr", "vc2-1c-1gb", "$6.00"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("table missing %q:\n%s", want, h.out)
		}
	}
	if strings.Contains(h.out.String(), "0.0.0.0") {
		t.Errorf("table shows the placeholder address:\n%s", h.out)
	}
	h.out.Reset()
	h.flags.JSON = true
	if err := h.run("servers"); err != nil {
		t.Fatal(err)
	}
	var got cloudServersDTO
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("json: %v %s", err, h.out)
	}
	if got.MachineInterface != MachineInterface || len(got.Servers) != 2 || got.Servers[0].Name != "web" ||
		got.Servers[0].IPAddress != "203.0.113.9" || got.Servers[0].Region != "ewr" || got.Servers[1].IPAddress != "" {
		t.Fatalf("json envelope: %s", h.out)
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

// TestCloudJSONEnvelopes: every `teploy cloud ... --json` document carries
// machine_interface at its root (CLI-3); none is a bare array.
func TestCloudJSONEnvelopes(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.flags.JSON = true
	for _, tc := range []struct {
		args []string
		key  string
	}{
		{[]string{"servers"}, "servers"},
		{[]string{"regions"}, "providers"},
		{[]string{"regions", "--provider", "vultr"}, "regions"},
		{[]string{"sizes", "--provider", "vultr", "--region", "ewr"}, "sizes"},
		{[]string{"status"}, "key_works"},
	} {
		h.out.Reset()
		if err := h.run(tc.args...); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
			t.Fatalf("%v: not one JSON object: %v\n%s", tc.args, err, h.out)
		}
		if doc["machine_interface"] != float64(MachineInterface) {
			t.Errorf("%v: machine_interface = %v", tc.args, doc["machine_interface"])
		}
		if _, ok := doc[tc.key]; !ok {
			t.Errorf("%v: no %q in %s", tc.args, tc.key, h.out)
		}
	}
}

// TestCloudStatusJSONBadKeyIsOneDocument: a key that stopped working is
// reported in the status document, exit 0, not as a second document.
func TestCloudStatusJSONBadKeyIsOneDocument(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	if err := cloud.SaveCredentials(filepath.Join(h.home, ".teploy", "cloud.yml"), &cloud.Credentials{OrgID: "org_1", APIKey: "tp_test_revokedrevokedrevoked"}); err != nil {
		t.Fatal(err)
	}
	h.flags.JSON = true
	if err := h.run("status"); err != nil {
		t.Fatalf("status --json with a bad key: %v", err)
	}
	var st cloudStatusDTO
	if err := json.Unmarshal(h.out.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.LoggedIn || st.KeyWorks || st.Error == "" || st.MachineInterface != MachineInterface {
		t.Errorf("status: %+v", st)
	}
	h.flags.JSON = false
	if err := h.run("status"); err == nil {
		t.Error("human status with a bad key exited 0")
	}
}

func TestCloudServerCreateFullFlow(t *testing.T) {
	h := newCloudHarness(t)
	h.login()

	if err := h.run(createArgs...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if h.fake.keyPosts != 1 {
		t.Errorf("ssh key posts = %d, want 1", h.fake.keyPosts)
	}
	if got := h.fake.keys[0]["publicKey"]; !samePublicKey(got.(string), h.pubKey) {
		t.Errorf("registered key %q, want %q", got, h.pubKey)
	}
	if got := h.fake.keys[0]["name"]; got != "me@laptop" {
		t.Errorf("registered key name %q", got)
	}
	if len(h.confirms) != 1 || !strings.Contains(h.out.String(), "$6.00 per month") {
		t.Errorf("confirmations %v, output %s", h.confirms, h.out)
	}
	if h.fake.createPosts != 1 || h.fake.idemKeys[0] == "" {
		t.Errorf("create posts %d, idempotency keys %v", h.fake.createPosts, h.fake.idemKeys)
	}
	if h.fake.gets < 2 {
		t.Errorf("polled %d times", h.fake.gets)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" || s.User != "root" {
		t.Errorf("servers.yml entry %+v", s)
	}
	defaultKey := filepath.Join(h.home, ".ssh", "id_ed25519")
	if len(h.sshWaits) != 1 || h.sshWaits[0] != "203.0.113.7 "+defaultKey || len(h.setups) != 1 || h.setups[0] != "root@203.0.113.7 web1" || h.setupKey[0] != defaultKey {
		t.Errorf("ssh waits %v, setups %v, keys %v", h.sshWaits, h.setups, h.setupKey)
	}
	if !strings.Contains(h.out.String(), "server: web1") {
		t.Errorf("no next step in output: %s", h.out)
	}
	if strings.Contains(h.out.String(), cloudTestKey) {
		t.Error("output contains the key")
	}

	// A second create reuses the registered key and refuses the existing name.
	err := h.run(append(createArgs, "--yes")...)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second create: %v", err)
	}
	if h.fake.createPosts != 1 {
		t.Errorf("refused create still posted")
	}
	if err := h.run("server", "create", "web2", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes", "--no-setup"); err != nil {
		t.Fatal(err)
	}
	if h.fake.keyPosts != 1 {
		t.Errorf("key registered again: %d posts", h.fake.keyPosts)
	}
	if h.fake.createPosts != 2 || h.fake.idemKeys[0] == h.fake.idemKeys[1] {
		t.Errorf("each invocation needs its own idempotency key: %v", h.fake.idemKeys)
	}
}

// TestCloudServerCreateSlowAnswerPostsOnce is CLI-1: the create answer
// comes after the client gave up. The server was bought all the same; the
// CLI must not post again, and must find and continue with that server.
func TestCloudServerCreateSlowAnswerPostsOnce(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.postDelay = 10 * time.Second
	h.env.createTimeout = 100 * time.Millisecond

	if err := h.run(append(createArgs, "--yes")...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if h.fake.createPosts != 1 {
		t.Fatalf("create posted %d times, want exactly 1 (a timed-out create was re-sent)", h.fake.createPosts)
	}
	if !strings.Contains(h.out.String(), "not sent again") || !strings.Contains(h.out.String(), "Found server web1 (id srv_1)") {
		t.Errorf("output does not explain the adoption:\n%s", h.out)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" {
		t.Errorf("servers.yml entry %+v", s)
	}
	if len(h.setups) != 1 {
		t.Errorf("setups %v", h.setups)
	}
}

// TestCloudServerCreate502ThenAdoption: a 5xx after the machine was found
// keeps the row (teploy-cloud provisioning.go); the CLI adopts it.
func TestCloudServerCreate502ThenAdoption(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	// A server of another name, and one of the same name bought earlier,
	// exist before the attempt: neither may be adopted.
	h.fake.addServer(map[string]any{"id": "old_web1", "name": "web1", "status": "active", "ipAddress": "198.51.100.4"})
	h.fake.addServer(map[string]any{"id": "other", "name": "db", "status": "active", "ipAddress": "198.51.100.5"})
	h.fake.postStatus = 502
	h.fake.postKeepsRow = true
	h.fake.postBody = `{"success":false,"error":{"code":"PROVIDER_ERROR","message":"provider rejected instance creation"}}`

	if err := h.run(append(createArgs, "--yes", "--no-setup")...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if h.fake.createPosts != 1 {
		t.Fatalf("create posted %d times, want 1", h.fake.createPosts)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" {
		t.Errorf("servers.yml entry %+v (adopted the wrong server?)", s)
	}
	if !strings.Contains(h.out.String(), "id srv_1") {
		t.Errorf("output:\n%s", h.out)
	}
}

// TestCloudServerCreate502NothingBought: a 5xx with no new server of the
// name means nothing was bought; the CLI says so, exits non-zero, posts once.
func TestCloudServerCreate502NothingBought(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.addServer(map[string]any{"id": "old_web1", "name": "web1", "status": "active", "ipAddress": "198.51.100.4"})
	h.fake.postStatus = 503
	h.fake.postBody = `{"success":false,"error":{"code":"SERVICE_UNAVAILABLE","message":"try again"}}`

	err := h.run(append(createArgs, "--yes")...)
	if err == nil {
		t.Fatal("create with a failed purchase succeeded")
	}
	if !errors.Is(err, errCloudPurchaseUnknown) || !strings.Contains(err.Error(), "does not appear to have been bought") || !strings.Contains(err.Error(), "teploy cloud servers") {
		t.Errorf("error does not say what happened and how to check: %v", err)
	}
	if h.fake.createPosts != 1 {
		t.Errorf("create posted %d times, want 1", h.fake.createPosts)
	}
	if h.fake.lists < 1+h.env.confirmChecks {
		t.Errorf("listed %d times: the before-list plus %d checks expected", h.fake.lists, h.env.confirmChecks)
	}
	if len(h.serversYML()) != 0 || len(h.setups) != 0 {
		t.Errorf("servers.yml %v, setups %v", h.serversYML(), h.setups)
	}
	var env bytes.Buffer
	_ = writeMachineErrorEnvelope(&env, err)
	if !strings.Contains(env.String(), `"code":"uncertain-outcome"`) {
		t.Errorf("machine error envelope: %s", env.String())
	}
}

// TestCloudServerCreate4xxNoRetry: a refusal is final, shown with the
// server's own message, and nothing is posted twice or adopted.
func TestCloudServerCreate4xxNoRetry(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.postStatus = 403
	h.fake.postBody = `{"success":false,"error":{"code":"TRUST_LIMIT","message":"New organizations can run 1 managed server."}}`
	err := h.run(append(createArgs, "--yes")...)
	if err == nil || !strings.Contains(err.Error(), "New organizations can run 1 managed server.") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "(code") || !strings.Contains(err.Error(), "nothing was bought") {
		t.Errorf("TRUST_LIMIT message not shown as is: %v", err)
	}
	if h.fake.createPosts != 1 || h.fake.lists != 1 {
		t.Errorf("posts %d, lists %d (want 1 and the before-list only)", h.fake.createPosts, h.fake.lists)
	}
}

// TestCloudServerCreateDoesNotAdoptAnotherShape: a same-named server that
// appears during a failed create but is in another region or of another
// size is somebody else's; it is not adopted as the purchase.
func TestCloudServerCreateDoesNotAdoptAnotherShape(t *testing.T) {
	for _, foreign := range []map[string]any{
		{"id": "theirs", "name": "web1", "status": "active", "ipAddress": "198.51.100.9", "provider": "vultr", "providerRegion": "fra", "providerSize": "vc2-1c-1gb"},
		{"id": "theirs", "name": "web1", "status": "active", "ipAddress": "198.51.100.9", "provider": "vultr", "providerRegion": "ewr", "providerSize": "vc2-4c-8gb"},
		{"id": "theirs", "name": "web1", "status": "active", "ipAddress": "198.51.100.9", "provider": "hetzner", "providerRegion": "ewr", "providerSize": "vc2-1c-1gb"},
	} {
		h := newCloudHarness(t)
		h.login()
		h.fake.foreignOnPost = foreign
		h.fake.postStatus = 503
		h.fake.postBody = `{"success":false,"error":{"code":"SERVICE_UNAVAILABLE","message":"try again"}}`
		err := h.run(append(createArgs, "--yes", "--no-setup")...)
		if err == nil || !errors.Is(err, errCloudPurchaseUnknown) {
			t.Fatalf("%v: got %v\n%s", foreign, err, h.out)
		}
		if len(h.serversYML()) != 0 {
			t.Errorf("%v: adopted into servers.yml: %v", foreign, h.serversYML())
		}
	}
}

// TestCloudServerCreateIdempotencyConflictIsNotARefusal: a 409 saying a
// request with this Idempotency-Key is still running means an earlier copy
// of the POST (re-sent by a proxy) is buying the server. The CLI must not
// say "nothing was bought"; it looks for the server and adopts it.
func TestCloudServerCreateIdempotencyConflictIsNotARefusal(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.postStatus = 409
	h.fake.postKeepsRow = true
	h.fake.postBody = `{"success":false,"error":{"code":"IDEMPOTENCY_ERROR","message":"A request with this Idempotency-Key is still in progress"}}`
	if err := h.run(append(createArgs, "--yes", "--no-setup")...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if strings.Contains(h.out.String(), "nothing was bought") || h.fake.createPosts != 1 {
		t.Errorf("posts %d, output:\n%s", h.fake.createPosts, h.out)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" {
		t.Errorf("servers.yml entry %+v", s)
	}
}

func TestCloudServerCreateNoSetupSkipsSetup(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	if err := h.run(append(createArgs, "--yes", "--no-setup")...); err != nil {
		t.Fatal(err)
	}
	if len(h.setups) != 0 || len(h.sshWaits) != 0 {
		t.Errorf("setup ran: %v %v", h.setups, h.sshWaits)
	}
	if len(h.confirms) != 0 {
		t.Errorf("--yes still asked: %v", h.confirms)
	}
	if h.serversYML()["web1"].Host != "203.0.113.7" {
		t.Errorf("servers.yml: %+v", h.serversYML())
	}
}

// TestCloudServerCreateDeclinedTouchesNothing: declining registers no key
// and buys nothing (CLI-6).
func TestCloudServerCreateDeclinedTouchesNothing(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.answer = false
	err := h.run(createArgs...)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("got %v", err)
	}
	if h.fake.createPosts != 0 || h.fake.keyPosts != 0 {
		t.Errorf("declined create posted: %d servers, %d keys", h.fake.createPosts, h.fake.keyPosts)
	}
}

// TestCloudServerCreateUsesGlobalKey is CLI-2: with the global --key, the
// key installed on the server is that key's public half, and setup
// connects with that key.
func TestCloudServerCreateUsesGlobalKey(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	deployKey := filepath.Join(h.home, "keys", "deploy")
	deployPub := writeTestSSHKey(t, deployKey, "deploy@ci")
	os.Remove(deployKey + ".pub") // derived from the private key, no .pub needed
	h.flags.Key = deployKey

	if err := h.run(append(createArgs, "--yes")...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if len(h.fake.keys) != 1 || !samePublicKey(h.fake.keys[0]["publicKey"].(string), deployPub) {
		t.Fatalf("registered %v, want the --key public half %s", h.fake.keys, deployPub)
	}
	if samePublicKey(h.fake.keys[0]["publicKey"].(string), h.pubKey) {
		t.Fatal("installed ~/.ssh/id_ed25519.pub although --key was given")
	}
	if len(h.setupKey) != 1 || h.setupKey[0] != deployKey || !strings.HasSuffix(h.sshWaits[0], " "+deployKey) {
		t.Errorf("setup connected with %v / %v, want %s", h.setupKey, h.sshWaits, deployKey)
	}
}

func TestCloudServerCreateRefusesMismatchedSSHKey(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	other := filepath.Join(h.home, "keys", "other")
	writeTestSSHKey(t, other, "other")
	h.flags.Key = filepath.Join(h.home, ".ssh", "id_ed25519")
	err := h.run(append(createArgs, "--yes", "--ssh-key", other+".pub")...)
	if err == nil || !strings.Contains(err.Error(), "not the public half") {
		t.Fatalf("got %v", err)
	}
	h.flags.Key = ""
	err = h.run(append(createArgs, "--yes", "--ssh-key", filepath.Join(h.home, "keys", "missing.pub"))...)
	if err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("--ssh-key without its private key: %v", err)
	}
	if h.fake.createPosts != 0 || h.fake.keyPosts != 0 {
		t.Errorf("refused create posted: %d servers, %d keys", h.fake.createPosts, h.fake.keyPosts)
	}
	// --ssh-key alone selects the private key next to it.
	if err := h.run(append(createArgs, "--yes", "--no-setup", "--ssh-key", other+".pub")...); err != nil {
		t.Fatal(err)
	}
}

// TestCloudServerCreateWaitsForRealAddress is CLI-5: active with the
// 0.0.0.0 placeholder is not ready, and the placeholder is never written.
func TestCloudServerCreateWaitsForRealAddress(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.pollsToActive = 1
	h.fake.pollsToAddress = 4
	if err := h.run(append(createArgs, "--yes", "--no-setup")...); err != nil {
		t.Fatalf("create: %v\n%s", err, h.out)
	}
	if h.fake.gets < 4 {
		t.Errorf("stopped polling after %d GETs", h.fake.gets)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" {
		t.Errorf("servers.yml entry %+v", s)
	}
	if strings.Contains(h.out.String(), "0.0.0.0") || !strings.Contains(h.out.String(), "waiting for an IP address") {
		t.Errorf("output:\n%s", h.out)
	}
}

// TestCloudServerCreateResumeWithAttach is CLI-5/CLI-6: a server that stays
// on the placeholder address fails the wait without touching servers.yml,
// and the error names the attach command, which finishes the job without
// buying again.
func TestCloudServerCreateResumeWithAttach(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.pollsToAddress = 1 << 30
	h.env.provisionTimeout = 50 * time.Millisecond
	h.env.sleep = sleepCtx
	err := h.run(append(createArgs, "--yes")...)
	if err == nil {
		t.Fatal("create without an address succeeded")
	}
	if !strings.Contains(err.Error(), "teploy cloud server attach srv_1") || !strings.Contains(err.Error(), "do not create it again") {
		t.Fatalf("no resume command: %v", err)
	}
	if len(h.serversYML()) != 0 {
		t.Fatalf("servers.yml written without an address: %v", h.serversYML())
	}

	h.fake.mu.Lock()
	h.fake.pollsToAddress = 0
	h.fake.mu.Unlock()
	h.env.provisionTimeout = time.Minute
	if err := h.run("server", "attach", "srv_1"); err != nil {
		t.Fatalf("attach: %v\n%s", err, h.out)
	}
	if h.fake.createPosts != 1 {
		t.Errorf("attach bought: %d posts", h.fake.createPosts)
	}
	if s := h.serversYML()["web1"]; s.Host != "203.0.113.7" || len(h.setups) != 1 {
		t.Errorf("servers.yml %+v, setups %v", s, h.setups)
	}
	// Attach again: the entry already points at the server, nothing changes.
	if err := h.run("server", "attach", "web1", "--no-setup"); err != nil {
		t.Fatalf("second attach: %v", err)
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
	err := h.run(append(createArgs, "--yes")...)
	if err == nil || !strings.Contains(err.Error(), "failed to provision") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "attach") {
		t.Errorf("a failed server is not resumable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.home, ".teploy", "servers.yml")); !os.IsNotExist(err) {
		t.Error("failed server written to servers.yml")
	}
}

func TestCloudServerCreateJSON(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.flags.JSON = true
	if err := h.run(append(createArgs, "--no-setup")...); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("--json without --yes: %v", err)
	}
	if err := h.run(append(createArgs, "--yes")...); err == nil || !strings.Contains(err.Error(), "--no-setup") {
		t.Fatalf("--json without --no-setup: %v", err)
	}
	if h.fake.createPosts != 0 {
		t.Fatal("refused --json create posted")
	}
	if err := h.run(append(createArgs, "--yes", "--no-setup")...); err != nil {
		t.Fatalf("create --json: %v\n%s", err, h.errOut)
	}
	var res cloudServerResultDTO
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.out)
	}
	if !res.OK || res.MachineInterface != MachineInterface || res.Server.ID != "srv_1" || res.Server.IPAddress != "203.0.113.7" ||
		res.ServersYML != "web1" || res.Setup != cloudSetupSkipped || res.Adopted || len(res.Resume) != 0 {
		t.Errorf("result %+v", res)
	}
	if !strings.Contains(h.errOut.String(), "Created server") {
		t.Errorf("progress not on stderr: %q", h.errOut)
	}

	// A failure after the purchase still prints the document, with resume.
	h.out.Reset()
	h.fake.pollsToAddress = 1 << 30
	h.env.provisionTimeout = 50 * time.Millisecond
	h.env.sleep = sleepCtx
	err := h.run("server", "create", "web2", "--provider", "vultr", "--region", "ewr", "--size", "vc2-1c-1gb", "--yes", "--no-setup")
	if err == nil {
		t.Fatal("expected failure")
	}
	res = cloudServerResultDTO{}
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil {
		t.Fatalf("no result document after a post-purchase failure: %v\n%s", err, h.out)
	}
	if res.OK || res.Server.ID != "srv_2" || res.Error == "" || strings.Join(res.Resume, " ") != "teploy cloud server attach srv_2 --no-setup" {
		t.Errorf("failure result %+v", res)
	}
}

func TestCloudServerDelete(t *testing.T) {
	h := newCloudHarness(t)
	h.login()
	h.fake.addServer(map[string]any{"id": "srv_1", "name": "web1", "status": "active", "ipAddress": "203.0.113.7"})
	h.fake.addServer(map[string]any{"id": "srv_2", "name": "web2", "status": "active", "ipAddress": "203.0.113.8"})
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
	h.flags.JSON = true
	if err := h.run("server", "delete", "srv_2"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("--json delete without --yes: %v", err)
	}
	h.out.Reset()
	if err := h.run("server", "delete", "srv_2", "--yes"); err != nil {
		t.Fatal(err)
	}
	var res cloudServerDeleteDTO
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil {
		t.Fatalf("delete --json: %v\n%s", err, h.out)
	}
	if !res.OK || res.Server.ID != "srv_2" || res.ServersYMLRemoved || res.MachineInterface != MachineInterface {
		t.Errorf("delete result %+v", res)
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
