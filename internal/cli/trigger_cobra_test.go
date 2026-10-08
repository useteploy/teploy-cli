package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/trigger"
)

func TestTriggerActualCobraNonzeroFinalJSON(t *testing.T) {
	for _, args := range [][]string{{"deploy", "--trigger-stdin", "--json"}, {"preview", "deploy", "--trigger-stdin", "--json"}, {"preview", "destroy", "--trigger-stdin", "--json"}} {
		for _, input := range []string{`{"schema_version":1,"operation_key":"dummy-secret","unknown":"private-value"}`, strings.Repeat("x", trigger.MaxRequestBytes+1), `{`, `{}`} {
			cmd := NewRootCmd("fixture")
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			cmd.SetIn(strings.NewReader(input))
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("malformed trigger succeeded")
			}
			var result trigger.Result
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("nonzero stdout not exactly JSON: %s %v", out.String(), err)
			}
			if result.SchemaVersion != 1 || result.Publication != "not_started" || result.Generation != nil {
				t.Fatal(result)
			}
			if strings.Contains(out.String(), "private-value") || strings.Contains(out.String(), "dummy-secret") {
				t.Fatal("private input echoed")
			}
		}
	}
}
func TestRound5AuthenticatedUnusableCommitNeverAdmitsOrInvokes(t *testing.T) {
	run := newCountingRun()
	handler, ledger, queue := newAdmissionStack("s3cret", "main", "myapp", run.run)
	for _, body := range []string{`{"ref":"refs/heads/main"}`, `{"ref":"refs/heads/main","after":"malformed"}`, `{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `"}`, `{"ref":"refs/heads/main","deleted":true,"after":"` + strings.Repeat("a", 40) + `"}`} {
		response := postSigned(t, handler, "s3cret", "", body)
		if response.Code != http.StatusUnprocessableEntity && response.Code != http.StatusAccepted {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	run.waitIdle(t, queue)
	if run.count() != 0 {
		t.Fatal("malformed event reached production invocation")
	}
	if len(ledger.snapshot()) != 0 {
		t.Fatal("nondeployable event durably admitted")
	}
}
func TestRound5TriggerBindingSurvivesAliasRenameButNotTargetReuse(t *testing.T) {
	cfg := &config.AppConfig{App: "demo", Server: "original"}
	first, err := TriggerBindingDigest(cfg, "image:A", strings.Repeat("a", 32)+"/demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server = "renamed"
	second, _ := TriggerBindingDigest(cfg, "image:A", strings.Repeat("a", 32)+"/demo")
	if first != second {
		t.Fatal("alias entered execution identity")
	}
	third, _ := TriggerBindingDigest(cfg, "image:A", strings.Repeat("b", 32)+"/demo")
	if first == third {
		t.Fatal("new target reused old private binding")
	}
}
