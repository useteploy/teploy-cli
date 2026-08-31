package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/ssh"
)

// lockInfoJSON builds the contents of /deployments/<app>/.lock/info as the
// server stores it, at an age relative to now.
func lockInfoJSON(lockType, user, message string, age time.Duration) string {
	info := map[string]string{
		"type": lockType,
		"ts":   time.Now().UTC().Add(-age).Format(time.RFC3339),
	}
	if user != "" {
		info["user"] = user
	}
	if message != "" {
		info["message"] = message
	}
	out, err := json.Marshal(info)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// TestReportLockStatus covers the states an operator otherwise cannot tell
// apart: nothing held, a deploy in progress, a human freeze, and a lock left
// behind by a deploy that died before releasing it. The staleness durations
// are chosen against state's TTLs — 30 minutes for auto, 2 minutes for heal.
func TestReportLockStatus(t *testing.T) {
	tests := []struct {
		name        string
		lockOut     string
		wantLocked  bool
		wantType    string
		wantUser    string
		wantMessage string
		wantStale   bool
	}{
		{
			name:       "no lock",
			lockOut:    "",
			wantLocked: false,
		},
		{
			name:       "fresh auto lock — a deploy is running",
			lockOut:    lockInfoJSON("auto", "", "", 30*time.Second),
			wantLocked: true,
			wantType:   "auto",
		},
		{
			name:        "manual lock carries who froze deploys and why",
			lockOut:     lockInfoJSON("manual", "tyler", "incident 412", time.Minute),
			wantLocked:  true,
			wantType:    "manual",
			wantUser:    "tyler",
			wantMessage: "incident 412",
		},
		{
			name:       "stale auto lock — the deploy died without releasing",
			lockOut:    lockInfoJSON("auto", "", "", 2*time.Hour),
			wantLocked: true,
			wantType:   "auto",
			wantStale:  true,
		},
		{
			name:       "stale heal lock — heal's TTL is minutes, not the auto half-hour",
			lockOut:    lockInfoJSON("heal", "", "", 10*time.Minute),
			wantLocked: true,
			wantType:   "heal",
			wantStale:  true,
		},
		{
			// A manual lock is an explicit freeze with no expiry and is never
			// auto-broken, so age must never make it look abandoned.
			name:       "old manual lock is never stale",
			lockOut:    lockInfoJSON("manual", "tyler", "", 30*24*time.Hour),
			wantLocked: true,
			wantType:   "manual",
			wantUser:   "tyler",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := ssh.NewMockExecutor("test-host",
				ssh.MockCommand{Match: "cat /deployments/blog/.lock/info", Output: tt.lockOut},
			)
			appCfg := &config.AppConfig{App: "blog"}

			var out bytes.Buffer
			if err := reportLockStatus(context.Background(), &Flags{JSON: true}, appCfg, mock, &out); err != nil {
				t.Fatalf("reportLockStatus() error: %v", err)
			}

			var got lockStatusDTO
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("parsing JSON output %q: %v", out.String(), err)
			}
			if got.App != "blog" || got.Server != "test-host" {
				t.Errorf("app/server = %s/%s, want blog/test-host", got.App, got.Server)
			}
			if got.Locked != tt.wantLocked {
				t.Errorf("locked = %v, want %v", got.Locked, tt.wantLocked)
			}
			if got.Type != tt.wantType {
				t.Errorf("type = %q, want %q", got.Type, tt.wantType)
			}
			if got.User != tt.wantUser {
				t.Errorf("user = %q, want %q", got.User, tt.wantUser)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tt.wantMessage)
			}
			if got.Stale != tt.wantStale {
				t.Errorf("stale = %v, want %v", got.Stale, tt.wantStale)
			}
			if tt.wantLocked && got.Since == "" {
				t.Error("since is empty for a held lock")
			}
		})
	}
}

// A status command that mutates the thing it reports is a trap: reading the
// lock must never create the app directory, take the lock, or release it.
func TestReportLockStatus_IsReadOnly(t *testing.T) {
	mock := ssh.NewMockExecutor("test-host",
		ssh.MockCommand{Match: "cat /deployments/blog/.lock/info", Output: lockInfoJSON("auto", "", "", 2*time.Hour)},
	)

	var out bytes.Buffer
	if err := reportLockStatus(context.Background(), &Flags{}, &config.AppConfig{App: "blog"}, mock, &out); err != nil {
		t.Fatalf("reportLockStatus() error: %v", err)
	}

	for _, call := range mock.Calls {
		switch {
		case strings.HasPrefix(call, "mkdir"):
			t.Errorf("status created something on the server: %s", call)
		case strings.HasPrefix(call, "rm "), strings.HasPrefix(call, "rm -rf"):
			t.Errorf("status removed the lock: %s", call)
		case strings.HasPrefix(call, "UPLOAD:"):
			t.Errorf("status wrote to the server: %s", call)
		}
	}
	if len(mock.Files) != 0 {
		t.Errorf("status uploaded files: %v", mock.Files)
	}
}

// The text output has to say which of the three situations it found, not just
// "locked" — that distinction is the reason the command exists.
func TestWriteLockStatus_Text(t *testing.T) {
	tests := []struct {
		name     string
		status   lockStatusDTO
		contains []string
	}{
		{
			name:     "no lock",
			status:   lockStatusDTO{App: "blog", Server: "prod"},
			contains: []string{"blog is not locked"},
		},
		{
			name:     "auto lock reads as a deploy in progress",
			status:   lockStatusDTO{App: "blog", Server: "prod", Locked: true, Type: "auto", Since: "2026-08-30T12:00:00Z"},
			contains: []string{"blog is locked on prod", "a deploy is in progress", "2026-08-30T12:00:00Z"},
		},
		{
			name: "manual lock names the human and the reason",
			status: lockStatusDTO{
				App: "blog", Server: "prod", Locked: true, Type: "manual",
				User: "tyler", Message: "incident 412", Since: "2026-08-30T12:00:00Z",
			},
			contains: []string{"deploys frozen", "tyler", "incident 412"},
		},
		{
			name:     "stale lock says so",
			status:   lockStatusDTO{App: "blog", Server: "prod", Locked: true, Type: "auto", Stale: true, Since: "2026-08-30T12:00:00Z"},
			contains: []string{"Stale:", "presumed dead"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeLockStatus(&out, tt.status, false); err != nil {
				t.Fatalf("writeLockStatus() error: %v", err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// `locked` and `stale` must survive JSON encoding even when false — a script
// branching on them can't tell false from an omitted field.
func TestWriteLockStatus_JSONAlwaysEmitsLockedAndStale(t *testing.T) {
	var out bytes.Buffer
	if err := writeLockStatus(&out, lockStatusDTO{App: "blog", Server: "prod"}, true); err != nil {
		t.Fatalf("writeLockStatus() error: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("parsing JSON output %q: %v", out.String(), err)
	}
	for _, key := range []string{"app", "server", "locked", "stale"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("JSON output is missing %q: %s", key, out.String())
		}
	}
}

// `teploy lock status` must resolve to the subcommand, and plain `teploy lock`
// must keep placing a lock rather than becoming a group with no action.
func TestLockCmd_StatusSubcommandDoesNotShadowLock(t *testing.T) {
	lockCmd := newLockCmd(&Flags{})

	sub, _, err := lockCmd.Find([]string{"status"})
	if err != nil {
		t.Fatalf("finding 'lock status': %v", err)
	}
	if sub.Name() != "status" {
		t.Errorf("'lock status' resolved to %q, want status", sub.Name())
	}
	if lockCmd.RunE == nil {
		t.Error("'teploy lock' lost its own action after gaining a subcommand")
	}
}
