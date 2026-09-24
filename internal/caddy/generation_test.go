package caddy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// caddyMutateMocks satisfies one full mutate transaction (lock, read,
// adapt, commit, reload, verify, release).
func caddyMutateMocks(caddyfile string) (*ssh.MockExecutor, []ssh.MockCommand) {
	extra := []ssh.MockCommand{
		ssh.MockCommand{Match: "mkdir /deployments/caddy/.lock", Output: ""},
		ssh.MockCommand{Match: "docker exec caddy caddy reload", Output: ""},
		ssh.MockCommand{Match: "a=$(docker exec caddy md5sum", Output: "TEPLOY_CADDY_OK"},
	}
	mock := ssh.NewMockExecutor("1.2.3.4", extra...)
	if caddyfile != "" {
		mock.Files["/deployments/caddy/Caddyfile"] = []byte(caddyfile)
	}
	return mock, extra
}

// TestWithGeneration_StampAndRegionHash pins the C01-8/9 identity: the
// managed block carries a generation stamp line inside the markers, and
// ManagedRegionHash(ReadManagedBlock) round-trips the exact region bytes.
func TestWithGeneration_StampAndRegionHash(t *testing.T) {
	mock, _ := caddyMutateMocks("{\n\tadmin 0.0.0.0:2019\n}\n")
	cd := NewClient(mock).WithGeneration(7)
	if err := cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v1", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute: %v", err)
	}
	content := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(content, "# TEPLOY GENERATION 7\n") {
		t.Errorf("the managed block must be stamped with its generation:\n%s", content)
	}

	region, present, err := NewClient(mock).ReadManagedBlock(context.Background(), "myapp")
	if err != nil || !present {
		t.Fatalf("ReadManagedBlock: %q %v", region, err)
	}
	for _, want := range []string{
		"# TEPLOY BEGIN myapp",
		"# TEPLOY GENERATION 7",
		"myapp-web-v1:3000",
		"# TEPLOY END myapp",
	} {
		if !strings.Contains(region, want) {
			t.Errorf("region missing %q:\n%s", want, region)
		}
	}
	// The hash normalization is the CAS's identity contract: region + "\n"
	// (what sed prints), the empty input for an absent region.
	want := sha256.Sum256([]byte(region + "\n"))
	if got := ManagedRegionHash(region); got != hex.EncodeToString(want[:]) {
		t.Errorf("ManagedRegionHash mismatch: %s", got)
	}
	empty := sha256.Sum256(nil)
	if ManagedRegionHash("") != hex.EncodeToString(empty[:]) {
		t.Error("absent region must hash the empty input")
	}
}

// TestRegionGenerationAndUpstreams pins the refusal-evidence parsers.
func TestRegionGenerationAndUpstreams(t *testing.T) {
	region := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 9\nmyapp.com {\n\treverse_proxy myapp-web-v9:3000 myapp-web-v9-2:3000 {\n\t}\n}\n# TEPLOY END myapp"
	gen, ok := RegionGeneration(region)
	if !ok || gen != 9 {
		t.Fatalf("expected stamp 9, got %d %v", gen, ok)
	}
	if gen, ok := RegionGeneration(strings.ReplaceAll(region, "# TEPLOY GENERATION 9\n", "")); ok {
		t.Fatalf("an unstamped region must report ok=false, got %d", gen)
	}
	ups := RegionUpstreams(region)
	if len(ups) != 2 || ups[0] != "myapp-web-v9:3000" || ups[1] != "myapp-web-v9-2:3000" {
		t.Fatalf("upstreams must parse for the evidence, got %v", ups)
	}
}

// TestRouteCAS_RefusesWhenPredecessorBlockChanged is the A12/T05 core
// acceptance: the block changed since this operation resolved it (a newer
// writer switched the route) — the commit is refused, nothing lands, no
// reload runs, and the refusal names BOTH generations plus the live
// upstreams.
func TestRouteCAS_RefusesWhenPredecessorBlockChanged(t *testing.T) {
	predecessor := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v7:3000\n}\n# TEPLOY END myapp\n"
	successor := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 8\nmyapp.com {\n\treverse_proxy myapp-web-v8:3000\n}\n# TEPLOY END myapp\n"
	mock, _ := caddyMutateMocks(predecessor)

	// The operation resolved the PREDECESSOR's region…
	resolved, _, err := NewClient(mock).ReadManagedBlock(context.Background(), "myapp")
	if err != nil {
		t.Fatalf("ReadManagedBlock: %v", err)
	}
	// …and a successor switched the route before its commit.
	mock.Files["/deployments/caddy/Caddyfile"] = []byte(successor)

	cd := NewClient(mock).WithRouteCAS("myapp", []string{ManagedRegionHash(resolved)}, 7)
	err = cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v7", 3000, TLS{}, "", nil, Firewall{}, Access{})
	var cas *ErrRouteCAS
	if !errors.As(err, &cas) {
		t.Fatalf("expected *ErrRouteCAS, got %v", err)
	}
	if cas.ExpectedGeneration != 7 || !cas.FoundStamped || cas.FoundGeneration != 8 {
		t.Errorf("the refusal must name both generations: %+v", cas)
	}
	if len(cas.FoundUpstreams) != 1 || cas.FoundUpstreams[0] != "myapp-web-v8:3000" {
		t.Errorf("the refusal must name the live upstreams: %+v", cas)
	}
	for _, want := range []string{"generation 7", "generation 8", "myapp-web-v8:3000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must carry %q: %v", want, err)
		}
	}
	// Nothing landed: the live Caddyfile is still the successor's block.
	if string(mock.Files["/deployments/caddy/Caddyfile"]) != successor {
		t.Errorf("a refused CAS must not modify the Caddyfile:\n%s", mock.Files["/deployments/caddy/Caddyfile"])
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "docker exec caddy caddy reload") {
			t.Error("no reload may run after a refused CAS")
		}
	}
}

// TestRouteCAS_PassesOnExactPredecessor: the same region resolved is the
// region live at commit — the switch lands.
func TestRouteCAS_PassesOnExactPredecessor(t *testing.T) {
	predecessor := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v7:3000\n}\n# TEPLOY END myapp\n"
	mock, _ := caddyMutateMocks(predecessor)
	resolved, _, err := NewClient(mock).ReadManagedBlock(context.Background(), "myapp")
	if err != nil {
		t.Fatal(err)
	}
	cd := NewClient(mock).WithGeneration(8).WithRouteCAS("myapp", []string{ManagedRegionHash(resolved)}, 7)
	if err := cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v8", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("SetRoute over the exact predecessor must land: %v", err)
	}
	content := string(mock.Files["/deployments/caddy/Caddyfile"])
	if !strings.Contains(content, "# TEPLOY GENERATION 8") || !strings.Contains(content, "myapp-web-v8:3000") {
		t.Errorf("the switch must land stamped with its generation:\n%s", content)
	}
}

// TestRouteCAS_AcceptsRestoreSet: the compensation restore accepts either
// the region the operation resolved or the one it switched to, and refuses
// a successor's block — an UNFENCED restore cannot clobber the newer
// generation's route (the acceptance's delayed-effect race).
func TestRouteCAS_AcceptsRestoreSet(t *testing.T) {
	// Region consts carry NO trailing newline (ReadManagedBlock's shape);
	// seeded files add it.
	resolvedBlock := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v7:3000\n}\n# TEPLOY END myapp"
	switchedBlock := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 8\nmyapp.com {\n\treverse_proxy myapp-web-v8:3000\n}\n# TEPLOY END myapp"
	successorBlock := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 9\nmyapp.com {\n\treverse_proxy myapp-web-v9:3000\n}\n# TEPLOY END myapp"

	// Restore over our own uncommitted switch: live == switched — lands.
	mock, _ := caddyMutateMocks(switchedBlock + "\n")
	cd := NewClient(mock).WithGeneration(7).WithRouteCAS("myapp",
		[]string{ManagedRegionHash(resolvedBlock), ManagedRegionHash(switchedBlock)}, 7)
	if err := cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v7", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("restore over the operation's own switch must land: %v", err)
	}
	if !strings.Contains(string(mock.Files["/deployments/caddy/Caddyfile"]), "# TEPLOY GENERATION 7") {
		t.Error("the restored block must carry the restored generation's stamp")
	}

	// Restore against a successor's block: refused, nothing lands.
	mock2, _ := caddyMutateMocks(successorBlock + "\n")
	cd2 := NewClient(mock2).WithGeneration(7).WithRouteCAS("myapp",
		[]string{ManagedRegionHash(resolvedBlock), ManagedRegionHash(switchedBlock)}, 7)
	err := cd2.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v7", 3000, TLS{}, "", nil, Firewall{}, Access{})
	var cas *ErrRouteCAS
	if !errors.As(err, &cas) || cas.FoundGeneration != 9 {
		t.Fatalf("expected a CAS refusal naming the successor's generation 9, got %v", err)
	}
	if string(mock2.Files["/deployments/caddy/Caddyfile"]) != successorBlock+"\n" {
		t.Error("a refused restore must not modify the Caddyfile")
	}
}

// TestRouteCAS_FirstDeployAbsentRegion: no managed region at resolution and
// none at commit — the CAS passes (hash of the empty input on both sides).
func TestRouteCAS_FirstDeployAbsentRegion(t *testing.T) {
	mock, _ := caddyMutateMocks("{\n\tadmin 0.0.0.0:2019\n}\n")
	cd := NewClient(mock).WithGeneration(1).WithRouteCAS("myapp", []string{ManagedRegionHash("")}, 0)
	if err := cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v1", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("first deploy over an absent region must land: %v", err)
	}
}

// TestManagedBlockHostsSkipsStamp keeps the legacy lb- detection honest
// with stamped blocks: the stamp comment is not the address line.
func TestManagedBlockHostsSkipsStamp(t *testing.T) {
	content := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 3\nmyapp.com, www.myapp.com {\n\treverse_proxy x:3000\n}\n# TEPLOY END myapp\n"
	hosts := managedBlockHosts(content, "myapp")
	if len(hosts) != 2 || hosts[0] != "myapp.com" || hosts[1] != "www.myapp.com" {
		t.Fatalf("stamp line must be skipped, got %v", hosts)
	}
}

// TestRouteCASFragment_HashParityWithSed proves the Go normalization and
// the shell extraction agree: the fragment's acceptable hash is exactly
// sha256sum of sed's marker-inclusive output.
func TestRouteCASFragment_HashParityWithSed(t *testing.T) {
	region := "# TEPLOY BEGIN myapp\n# TEPLOY GENERATION 7\nmyapp.com {\n\treverse_proxy myapp-web-v7:3000\n}\n# TEPLOY END myapp"
	content := "{\n\tadmin 0.0.0.0:2019\n}\n\n" + region + "\n"
	mock, _ := caddyMutateMocks(content)
	goHash := ManagedRegionHash(region)

	// Run the actual fragment shape through the mock: the commit's CAS
	// evaluates against Files and must PASS with the Go-computed hash.
	cd := NewClient(mock).WithGeneration(8).WithRouteCAS("myapp", []string{goHash}, 7)
	if err := cd.SetRoute(context.Background(), "myapp", "myapp.com", "myapp-web-v8", 3000, TLS{}, "", nil, Firewall{}, Access{}); err != nil {
		t.Fatalf("the Go-computed region hash must satisfy the shell-side sha256sum comparison: %v", err)
	}
}
