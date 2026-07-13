package backup

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("20060102-150405", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestParseBackupEntries(t *testing.T) {
	ls := strings.Join([]string{
		"2024-01-01 00:00:00        100 20240101-000000.tar.gz",
		"2024-01-02 03:04:05        100 myapp-backup-20240102-030405.tar.gz", // scheduled-shell naming
		"2024-01-03 00:00:00        100 20240103-000000.sql.gz",             // accessory naming
		"                           PRE nested/",                            // directory prefix — skipped
		"garbage line no timestamp",                                         // skipped
	}, "\n")

	entries := parseBackupEntries(ls)
	if len(entries) != 3 {
		t.Fatalf("expected 3 parsed entries, got %d: %+v", len(entries), entries)
	}
	if entries[1].name != "myapp-backup-20240102-030405.tar.gz" {
		t.Errorf("unexpected name: %s", entries[1].name)
	}
	if !entries[1].time.Equal(mustTime(t, "20240102-030405")) {
		t.Errorf("unexpected time: %s", entries[1].time)
	}
}

func TestSelectForDeletion(t *testing.T) {
	now := mustTime(t, "20240110-000000")
	entries := []backupEntry{
		{name: "a", time: mustTime(t, "20240101-000000")}, // 9 days old
		{name: "b", time: mustTime(t, "20240105-000000")}, // 5 days old
		{name: "c", time: mustTime(t, "20240108-000000")}, // 2 days old
		{name: "d", time: mustTime(t, "20240109-000000")}, // 1 day old
	}

	cases := []struct {
		name   string
		policy RetentionPolicy
		want   []string // newest-first order
	}{
		{"zero policy deletes nothing", RetentionPolicy{}, nil},
		{"keep last 2", RetentionPolicy{KeepLast: 2}, []string{"b", "a"}},
		{"keep last more than exist", RetentionPolicy{KeepLast: 10}, nil},
		{"max age 3 days", RetentionPolicy{MaxAgeDays: 3}, []string{"b", "a"}},
		// Floor wins: newest 3 kept even though b+a are older than the window.
		{"keep 3 floor beats max age", RetentionPolicy{KeepLast: 3, MaxAgeDays: 3}, []string{"a"}},
		// Beyond floor AND older than window.
		{"keep 1 and max age 3", RetentionPolicy{KeepLast: 1, MaxAgeDays: 3}, []string{"b", "a"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// selectForDeletion sorts in place; copy so cases don't interfere.
			cp := append([]backupEntry(nil), entries...)
			got := selectForDeletion(cp, tc.policy, now)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPruneBackups(t *testing.T) {
	ls := strings.Join([]string{
		"2024-01-01 00:00:00 100 20240101-000000.tar.gz",
		"2024-01-02 00:00:00 100 20240102-000000.tar.gz",
		"2024-01-03 00:00:00 100 20240103-000000.tar.gz",
	}, "\n")

	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: ls},
		ssh.MockCommand{Match: "aws s3 rm", Output: "delete: done\n"},
	)

	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	deleted, err := client.PruneBackups(context.Background(), "myapp", "volumes",
		S3Config{Bucket: "my-bucket", Region: "us-east-1"}, RetentionPolicy{KeepLast: 1})
	if err != nil {
		t.Fatalf("PruneBackups: %v", err)
	}

	if len(deleted) != 2 {
		t.Fatalf("expected 2 deleted, got %d: %v", len(deleted), deleted)
	}
	// Oldest two removed; newest kept.
	if deleted[0] != "20240102-000000.tar.gz" || deleted[1] != "20240101-000000.tar.gz" {
		t.Errorf("unexpected deletions: %v", deleted)
	}

	var rmCalls int
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "aws s3 rm") {
			rmCalls++
		}
		if strings.Contains(c, "20240103-000000") && strings.HasPrefix(c, "aws s3 rm") {
			t.Errorf("newest backup must not be deleted: %s", c)
		}
	}
	if rmCalls != 2 {
		t.Errorf("expected 2 rm calls, got %d", rmCalls)
	}
}

func TestPruneBackupsZeroPolicyNoop(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4") // no commands registered — any call would error
	var buf bytes.Buffer
	client := NewClient(mock, &buf)
	deleted, err := client.PruneBackups(context.Background(), "myapp", "volumes",
		S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{})
	if err != nil {
		t.Fatalf("zero policy should be a no-op, got: %v", err)
	}
	if len(deleted) != 0 || len(mock.Calls) != 0 {
		t.Errorf("zero policy must not touch the server: deleted=%v calls=%v", deleted, mock.Calls)
	}
}
