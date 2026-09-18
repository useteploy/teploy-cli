package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TCL-53: an early-exiting consumer must not leave the producer blocked on
// a full pipe while the parent waits on the producer — the legacy
// StdoutPipe + producer-first-wait shape stalled until context
// cancellation. Fakes for docker/ssh are injected via PATH and the real
// streamImage drives them.
func TestStreamImage_EarlyConsumerExitTerminates(t *testing.T) {
	bin := t.TempDir()
	// Fake docker: `docker save X` writes 16 MiB then sleeps — with a dead
	// reader the pipe fills and the writer blocks.
	docker := "#!/bin/sh\ndd if=/dev/zero bs=65536 count=256 2>/dev/null\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0755); err != nil {
		t.Fatal(err)
	}
	// Fake ssh: exits immediately.
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	err := streamImage(ctx, "myapp:v1", "example.com", "root", "", os.Stderr)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("consumer failure hidden")
	}
	if elapsed > 8*time.Second {
		t.Fatalf("pipeline stalled for %s — producer was left blocked on a full pipe", elapsed)
	}
	if ctx.Err() != nil {
		t.Fatal("pipeline needed the context deadline to terminate")
	}
}

// TCL-53: the consumer Start-failure path must reap its child.
func TestStreamImage_ConsumerStartFailureIsClean(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// A non-executable ssh on PATH: found, but Start fails.
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("not executable\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	err := streamImage(context.Background(), "myapp:v1", "example.com", "root", "", os.Stderr)
	if err == nil {
		t.Fatal("consumer start failure not surfaced")
	}
}
