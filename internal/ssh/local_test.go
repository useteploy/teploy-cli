package ssh

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalExecutor_Run(t *testing.T) {
	e := NewLocalExecutor()
	out, err := e.Run(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("got %q, want \"hello\"", out)
	}
}

func TestLocalExecutor_Run_Error(t *testing.T) {
	e := NewLocalExecutor()
	if _, err := e.Run(context.Background(), "exit 1"); err == nil {
		t.Error("expected error for a failing command")
	}
}

func TestLocalExecutor_Upload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")

	e := NewLocalExecutor()
	if err := e.Upload(context.Background(), strings.NewReader("hello world"), path, "0644"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading uploaded file: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("got %q, want \"hello world\"", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("got mode %o, want 0644", info.Mode().Perm())
	}
}

func TestLocalExecutor_Upload_ExecutableMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teploy")

	e := NewLocalExecutor()
	if err := e.Upload(context.Background(), strings.NewReader("#!/bin/sh\necho hi"), path, "0755"); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("got mode %o, want 0755", info.Mode().Perm())
	}
}

func TestLocalExecutor_HostAndUser(t *testing.T) {
	e := NewLocalExecutor()
	if e.Host() != "localhost" {
		t.Errorf("Host() = %q, want \"localhost\"", e.Host())
	}
	if e.User() == "" {
		t.Error("User() should not be empty")
	}
}
