package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/backup"
)

func TestBackupRunDetail(t *testing.T) {
	d := backupRunDetail(backup.BackupResult{ID: "20261103-040000", Key: "s3://b/k.pgdump", SizeBytes: 1616, Engine: "postgres", Consistency: "engine-consistent", DurationMs: 143}, backup.KindAccessory, "db")
	want := map[string]any{"id": "20261103-040000", "kind": "accessory", "accessory": "db", "size_bytes": int64(1616), "key": "s3://b/k.pgdump",
		"duration_ms": int64(143), "engine": "postgres", "consistency": "engine-consistent", "encrypted": false}
	for k, v := range want {
		if d[k] != v {
			t.Errorf("detail[%s] = %v, want %v", k, d[k], v)
		}
	}
	if _, ok := backupRunDetail(backup.BackupResult{}, backup.KindVolumes, "")["accessory"]; ok {
		t.Error("a volume backup carries no accessory")
	}
}

func TestVerifyEventOutcomeAndDetail(t *testing.T) {
	ok := &backup.VerifyResult{Date: "20261103-040000", OK: true, Engine: "postgres", Kind: "postgres", Metric: "tables=3", S3Key: "s3://b/k", SizeBytes: 10, DurationMs: 900}
	if err := verifyEventErr(ok, nil); err != nil {
		t.Errorf("a passing verification is not an error: %v", err)
	}
	d := verifyEventDetail("db", "", ok, time.Second)
	if d["ok"] != true || d["metric"] != "tables=3" || d["engine"] != "postgres" || d["date"] != "20261103-040000" || d["duration_ms"] != int64(900) {
		t.Errorf("unexpected detail %v", d)
	}
	failed := &backup.VerifyResult{OK: false, Detail: "restored database has zero tables"}
	if err := verifyEventErr(failed, nil); err == nil || err.Error() != "restored database has zero tables" {
		t.Errorf("a failed verification must record as failed with its detail, got %v", err)
	}
	if err := verifyEventErr(nil, errors.New("aws CLI not found")); err == nil {
		t.Error("an operational error must record as failed")
	}
	if d := verifyEventDetail("db", "20261103-040000", nil, 2*time.Second); d["ok"] != false || d["duration_ms"] != int64(2000) || d["date"] != "20261103-040000" {
		t.Errorf("an operational failure still records what was attempted: %v", d)
	}
}

func TestBackupRunDetailCarriesCoverage(t *testing.T) {
	d := backupRunDetail(backup.BackupResult{UncoveredDatabases: []string{"other"}, Warnings: []string{"covers only appdb"}}, backup.KindAccessory, "db")
	if u, ok := d["uncovered_databases"].([]string); !ok || u[0] != "other" {
		t.Errorf("uncovered_databases = %v", d["uncovered_databases"])
	}
	if w, ok := d["warnings"].([]string); !ok || w[0] != "covers only appdb" {
		t.Errorf("warnings = %v", d["warnings"])
	}
	if _, ok := backupRunDetail(backup.BackupResult{}, backup.KindVolumes, "")["warnings"]; ok {
		t.Error("no warnings key without warnings")
	}
}

func TestRestoreFlagsValidate(t *testing.T) {
	for _, tc := range []struct {
		name          string
		o             restoreFlags
		date, acc     string
		wantErrSubstr string
	}{
		{"plain restore", restoreFlags{}, "20260101-000000", "", ""},
		{"no id", restoreFlags{}, "", "", "backup id is required"},
		{"release", restoreFlags{release: true}, "", "db", ""},
		{"rollback pick", restoreFlags{rollback: true, recoveryDatabase: "x"}, "", "db", ""},
		{"both", restoreFlags{release: true, rollback: true}, "", "db", "mutually exclusive"},
		{"release needs accessory", restoreFlags{release: true}, "", "", "need --accessory"},
		{"release takes no id", restoreFlags{release: true}, "20260101-000000", "db", "not on a backup id"},
		{"pick without rollback", restoreFlags{recoveryDatabase: "x"}, "20260101-000000", "db", "--recovery-database"},
		{"release with pick", restoreFlags{release: true, recoveryDatabase: "x"}, "", "db", "--recovery-database"},
		{"database needs accessory", restoreFlags{database: "x"}, "20260101-000000", "", "--database needs --accessory"},
		{"override only on a restore", restoreFlags{rollback: true, ignoreSpace: true}, "", "db", "applies to a restore"},
		{"force only on release", restoreFlags{rollback: true, force: true}, "", "db", "--force applies"},
		{"force on release", restoreFlags{release: true, force: true}, "", "db", ""},
		{"release one database", restoreFlags{release: true, database: "reports"}, "", "db", ""},
		{"rollback one database", restoreFlags{rollback: true, database: "reports"}, "", "db", ""},
	} {
		err := tc.o.validate(tc.date, tc.acc)
		if tc.wantErrSubstr == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.wantErrSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErrSubstr)
		}
	}
}

func TestRestoreEventCarriesDatabase(t *testing.T) {
	d := restoreEventDetail(backupRestoreDTO{ID: "x", Kind: backup.KindAccessory, Accessory: "db", Database: "reports"})
	if d["database"] != "reports" {
		t.Errorf("detail = %v", d)
	}
}

// The coverage warning reaches the alert script, only one that understands it:
// an older script would report this successful backup as failed.
func TestBackupAlertWarning(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(dir, "sent")
	stub := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -d ]; then printf '%s' \"$2\" > " + sent + "; fi; shift; done\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "alert.sh")
	body := buildBackupAlertScript("myapp", "srv", "http://example.invalid/hook", "")
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		os.Remove(sent)
		cmd := exec.Command(script, args...)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		data, _ := os.ReadFile(sent)
		return string(data)
	}
	if got := run(); !strings.Contains(got, "Scheduled backup failed") {
		t.Errorf("failure body = %q", got)
	}
	if got := run("warning"); !strings.Contains(got, `"warning":true`) || !strings.Contains(got, "does not cover every database") || strings.Contains(got, "backup failed") {
		t.Errorf("warning body = %q", got)
	}
	cmd := backupWarningCmd("myapp")
	if !strings.Contains(cmd, "grep -qF '"+backupAlertWarningMarker+"'") || !strings.Contains(cmd, " warning; fi") {
		t.Errorf("warning command = %s", cmd)
	}
	if !strings.Contains(body, backupAlertWarningMarker) {
		t.Error("the script must carry the marker")
	}
}
