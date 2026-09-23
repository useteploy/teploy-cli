package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestWriteVersionJSONShape(t *testing.T) {
	var out bytes.Buffer
	if err := writeVersion(&out, "v0.1.37-test", true); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("version --json is not valid JSON: %q: %v", out.String(), err)
	}
	if decoded["version"] != "v0.1.37-test" {
		t.Fatalf("version = %v, want v0.1.37-test", decoded["version"])
	}
	if decoded["machine_interface"] != float64(MachineInterface) {
		t.Fatalf("machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
	}
	caps, ok := decoded["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities missing or not a list: %s", out.String())
	}
	if len(caps) != len(MachineCapabilities()) {
		t.Fatalf("capabilities count = %d, want %d", len(caps), len(MachineCapabilities()))
	}
	for i, token := range MachineCapabilities() {
		if caps[i] != token {
			t.Fatalf("capabilities[%d] = %v, want %q", i, caps[i], token)
		}
	}
	// Exact key set — additive fields are allowed only with the MI rules
	// documented on MachineInterface; a removal or rename is a bump.
	for _, key := range []string{"version", "machine_interface", "capabilities"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("version envelope missing %q: %s", key, out.String())
		}
	}
	if len(decoded) != 3 {
		t.Fatalf("version envelope has unexpected keys: %s", out.String())
	}
}

func TestWriteVersionHumanUnchanged(t *testing.T) {
	var out bytes.Buffer
	if err := writeVersion(&out, "v0.1.37-test", false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "teploy v0.1.37-test\n" {
		t.Fatalf("human version output = %q", out.String())
	}
}

func TestVersionCommandJSONEndToEnd(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCmd("vtest")
	root.SetOut(&out)
	root.SetArgs([]string{"version", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("version --json output not JSON: %q: %v", out.String(), err)
	}
	if decoded["machine_interface"] != float64(1) {
		t.Fatalf("machine_interface = %v, want 1", decoded["machine_interface"])
	}
}

// TestCapabilityTokenRegistry pins the exact token set: renaming or
// removing any advertised capability fails here, and a new token cannot
// land without being added to the golden list (adding is additive per the
// MI rules; the golden list forces the addition to be deliberate).
func TestCapabilityTokenRegistry(t *testing.T) {
	want := []string{
		"app-list-machine",
		"autodeploy-redeploy",
		"doctor-diagnostics",
		"env-set-stdin",
		"error-envelope",
		"health-modes",
		"kv-set-stdin",
		"plan-apply",
		"preview-blue-green",
		"preview-canonical-id",
		"provenance-records",
		"readiness-receipts",
		"repair-debt",
		"server-rename",
		"server-status-machine",
		"server-update",
		"template-var-stdin",
	}
	got := MachineCapabilities()
	if len(got) != len(want) {
		t.Fatalf("capability registry drifted: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("capability registry drifted: got %v, want %v", got, want)
		}
	}
	// Every named constant stays a member of the advertised set — deleting
	// a constant breaks compilation, redirecting one to a foreign string
	// breaks this membership check.
	member := map[string]bool{}
	for _, token := range got {
		if member[token] {
			t.Fatalf("duplicate capability token: %q", token)
		}
		member[token] = true
	}
	for _, token := range []string{
		CapEnvSetStdin, CapKvSetStdin, CapTemplateVarStdin,
		CapServerRename, CapServerUpdate, CapAutodeployRedeploy,
		CapHealthModes, CapProvenanceRecords, CapReadinessReceipts,
		CapPreviewCanonicalID, CapRepairDebt, CapPreviewBlueGreen,
		CapErrorEnvelope, CapAppListMachine, CapServerStatusMachine,
		CapDoctorDiagnostics, CapPlanApply,
	} {
		if !member[token] {
			t.Fatalf("capability constant %q is not advertised", token)
		}
	}
}

func TestMachineEnvelopesCarryInterfaceVersion(t *testing.T) {
	t.Run("app list", func(t *testing.T) {
		empty := ssh.NewMockExecutor("empty", ssh.MockCommand{Match: "for f in /deployments/*/state.json", Output: ""})
		got := collectAppList(context.Background(), empty, time.Now())
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["machine_interface"] != float64(MachineInterface) {
			t.Fatalf("app list machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
		}
	})

	t.Run("server status", func(t *testing.T) {
		empty := ssh.NewMockExecutor("empty",
			ssh.MockCommand{Match: "cat /proc/uptime", Err: nil},
		)
		got := collectServerStatus(context.Background(), empty, "prod", time.Now())
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["machine_interface"] != float64(MachineInterface) {
			t.Fatalf("server status machine_interface = %v, want %d", decoded["machine_interface"], MachineInterface)
		}
	})
}
