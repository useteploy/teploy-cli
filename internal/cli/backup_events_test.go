package cli

import (
	"errors"
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
