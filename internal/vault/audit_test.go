package vault

import "testing"

func TestToObserveEvent_KVSuccess(t *testing.T) {
	e := OpenBaoAuditEntry{Type: "response"}
	e.Auth.DisplayName = "baotest"
	e.Auth.PolicyResults.Allowed = true
	e.Request.Operation = "read"
	e.Request.Path = "secret/data/baotest/db"
	e.Request.MountType = "kv"
	e.Request.RemoteAddress = "10.0.0.5"
	e.Time = "2026-07-14T21:00:00Z"

	ev, ok := ToObserveEvent("baotest", e)
	if !ok {
		t.Fatal("kv response should be forwarded")
	}
	if ev.Actor != "baotest" || ev.Action != "vault.kv.read" || ev.Target != "secret/data/baotest/db" || ev.Result != "success" {
		t.Errorf("unexpected event: %+v", ev)
	}
	if ev.Metadata["source"] != "10.0.0.5" {
		t.Errorf("source not carried: %v", ev.Metadata)
	}
}

func TestToObserveEvent_Denied(t *testing.T) {
	e := OpenBaoAuditEntry{Type: "response"}
	e.Auth.DisplayName = "baotest"
	e.Request.Operation = "read"
	e.Request.MountType = "kv"
	e.Auth.PolicyResults.Allowed = false // denied
	ev, ok := ToObserveEvent("baotest", e)
	if !ok || ev.Result != "denied" {
		t.Errorf("denied access should forward with result=denied: ok=%v ev=%+v", ok, ev)
	}
}

func TestToObserveEvent_DatabaseCreds(t *testing.T) {
	e := OpenBaoAuditEntry{Type: "response"}
	e.Auth.DisplayName = "baotest"
	e.Auth.PolicyResults.Allowed = true
	e.Request.Operation = "read"
	e.Request.MountType = "database"
	e.Request.Path = "database/creds/baotest-role"
	ev, ok := ToObserveEvent("baotest", e)
	if !ok || ev.Action != "vault.database.read" {
		t.Errorf("db creds access should forward: ok=%v ev=%+v", ok, ev)
	}
}

func TestToObserveEvent_Skips(t *testing.T) {
	// Request records (not responses) are skipped to avoid double-counting.
	req := OpenBaoAuditEntry{Type: "request"}
	req.Request.MountType = "kv"
	if _, ok := ToObserveEvent("app", req); ok {
		t.Error("request records must be skipped")
	}
	// System/token mounts are skipped.
	sys := OpenBaoAuditEntry{Type: "response"}
	sys.Request.MountType = "system"
	if _, ok := ToObserveEvent("app", sys); ok {
		t.Error("system mount must be skipped")
	}
}
