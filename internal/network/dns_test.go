package network

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestBuildDNSBlock(t *testing.T) {
	entries := map[string]string{
		"myapp":          "100.64.0.1",
		"myapp-postgres": "100.64.0.2",
		"myapp-redis":    "100.64.0.3",
	}

	block := buildDNSBlock(entries)

	if !strings.HasPrefix(block, dnsBeginMarker) {
		t.Error("block should start with BEGIN marker")
	}
	if !strings.HasSuffix(block, dnsEndMarker) {
		t.Error("block should end with END marker")
	}
	if !strings.Contains(block, "100.64.0.1 myapp") {
		t.Error("block should contain myapp entry")
	}
	if !strings.Contains(block, "100.64.0.2 myapp-postgres") {
		t.Error("block should contain myapp-postgres entry")
	}
	if !strings.Contains(block, "100.64.0.3 myapp-redis") {
		t.Error("block should contain myapp-redis entry")
	}

	// Verify deterministic ordering (sorted by name).
	lines := strings.Split(block, "\n")
	// lines: [marker, myapp, myapp-postgres, myapp-redis, end-marker]
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[1], "myapp") || strings.Contains(lines[1], "postgres") {
		t.Errorf("first entry should be myapp, got: %s", lines[1])
	}
	if !strings.Contains(lines[2], "myapp-postgres") {
		t.Errorf("second entry should be myapp-postgres, got: %s", lines[2])
	}
	if !strings.Contains(lines[3], "myapp-redis") {
		t.Errorf("third entry should be myapp-redis, got: %s", lines[3])
	}
}

func TestReplaceDNSBlock_Append(t *testing.T) {
	existing := "127.0.0.1 localhost\n::1 localhost"
	block := buildDNSBlock(map[string]string{"myapp": "100.64.0.1"})

	result := replaceDNSBlock(existing, block)

	if !strings.Contains(result, "127.0.0.1 localhost") {
		t.Error("should preserve existing entries")
	}
	if !strings.Contains(result, "100.64.0.1 myapp") {
		t.Error("should append new entries")
	}
	if !strings.Contains(result, dnsBeginMarker) {
		t.Error("should include BEGIN marker")
	}
	if !strings.Contains(result, dnsEndMarker) {
		t.Error("should include END marker")
	}
}

func TestReplaceDNSBlock_Replace(t *testing.T) {
	existing := "127.0.0.1 localhost\n" +
		dnsBeginMarker + "\n" +
		"100.64.0.99 old-app\n" +
		dnsEndMarker + "\n" +
		"192.168.1.1 other"

	block := buildDNSBlock(map[string]string{"myapp": "100.64.0.1"})

	result := replaceDNSBlock(existing, block)

	if !strings.Contains(result, "127.0.0.1 localhost") {
		t.Error("should preserve entries before block")
	}
	if !strings.Contains(result, "192.168.1.1 other") {
		t.Error("should preserve entries after block")
	}
	if strings.Contains(result, "old-app") {
		t.Error("should remove old teploy entries")
	}
	if !strings.Contains(result, "100.64.0.1 myapp") {
		t.Error("should include new entries")
	}
	// Should have exactly one BEGIN and one END marker.
	if strings.Count(result, dnsBeginMarker) != 1 {
		t.Error("should have exactly one BEGIN marker")
	}
	if strings.Count(result, dnsEndMarker) != 1 {
		t.Error("should have exactly one END marker")
	}
}

func TestUpdateDNS(t *testing.T) {
	existingHosts := "127.0.0.1 localhost\n::1 localhost"

	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /etc/hosts", Output: existingHosts},
		ssh.MockCommand{Match: "cat > /tmp/teploy_hosts", Output: ""},
	)

	entries := map[string]string{
		"myapp": "100.64.0.1",
		"mydb":  "100.64.0.2",
	}

	if err := UpdateDNS(context.Background(), mock, entries); err != nil {
		t.Fatalf("UpdateDNS: %v", err)
	}

	// Verify the write command was called.
	if len(mock.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(mock.Calls), mock.Calls)
	}

	writeCmd := mock.Calls[1]
	if !strings.Contains(writeCmd, "100.64.0.1 myapp") {
		t.Error("write command should contain myapp entry")
	}
	if !strings.Contains(writeCmd, "100.64.0.2 mydb") {
		t.Error("write command should contain mydb entry")
	}
	if !strings.Contains(writeCmd, dnsBeginMarker) {
		t.Error("write command should contain BEGIN marker")
	}
	if !strings.Contains(writeCmd, dnsEndMarker) {
		t.Error("write command should contain END marker")
	}
	// Original content should be preserved.
	if !strings.Contains(writeCmd, "127.0.0.1 localhost") {
		t.Error("write command should preserve original hosts entries")
	}
}

func TestUpdateDNS_EmptyEntries(t *testing.T) {
	mock := ssh.NewMockExecutor("server1")

	if err := UpdateDNS(context.Background(), mock, map[string]string{}); err != nil {
		t.Fatalf("UpdateDNS with empty entries: %v", err)
	}

	if len(mock.Calls) != 0 {
		t.Errorf("should not make any calls for empty entries, got %d: %v", len(mock.Calls), mock.Calls)
	}
}

func TestUpdateDNS_ReadError(t *testing.T) {
	mock := ssh.NewMockExecutor("server1",
		ssh.MockCommand{Match: "cat /etc/hosts", Err: fmt.Errorf("permission denied")},
	)

	err := UpdateDNS(context.Background(), mock, map[string]string{"app": "1.2.3.4"})
	if err == nil {
		t.Fatal("expected error when /etc/hosts cannot be read")
	}
	if !strings.Contains(err.Error(), "reading /etc/hosts") {
		t.Errorf("error should mention reading /etc/hosts, got: %v", err)
	}
}
