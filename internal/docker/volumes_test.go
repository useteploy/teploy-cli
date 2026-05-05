package docker

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

// mountsLine assembles a single inspect output line for the format the
// production code uses: "{type}|{source}|{destination}\n". Test helper
// for keeping fixtures tight.
func mountsLine(typ, src, dst string) string {
	return typ + "|" + src + "|" + dst + "\n"
}

func TestDetectVolumeMismatches_FirstDeploy(t *testing.T) {
	// No existing container = no mismatches, deploy proceeds normally.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: ""},
	)

	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "myapp", map[string]string{
		"/deployments/myapp/volumes/data": "/data",
	})
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no mismatches on first deploy, got %d", len(got))
	}
}

func TestDetectVolumeMismatches_AlreadyMatches(t *testing.T) {
	// Existing container's mount source already matches teploy's expected path.
	// Common case for any second-or-later deploy after this fix lands.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: "abc123"},
		ssh.MockCommand{
			Match:  "docker inspect abc123",
			Output: mountsLine("bind", "/deployments/myapp/volumes/data", "/data"),
		},
	)

	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "myapp", map[string]string{
		"/deployments/myapp/volumes/data": "/data",
	})
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no mismatches when sources match, got %v", got)
	}
}

func TestDetectVolumeMismatches_NamedVolumeMismatch(t *testing.T) {
	// The disaster case: existing container uses a Docker named volume,
	// teploy.yml resolves to a bind mount path. Must detect.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: "deadbeef"},
		ssh.MockCommand{
			Match:  "docker inspect deadbeef",
			Output: mountsLine("volume", "/var/lib/docker/volumes/199513fa.../_data", "/data"),
		},
	)

	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "forgejo", map[string]string{
		"/deployments/forgejo/volumes/data": "/data",
	})
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 mismatch, got %d", len(got))
	}
	if got[0].ContainerPath != "/data" {
		t.Errorf("ContainerPath: got %q, want /data", got[0].ContainerPath)
	}
	if got[0].ExistingSource != "/var/lib/docker/volumes/199513fa.../_data" {
		t.Errorf("ExistingSource wrong: %q", got[0].ExistingSource)
	}
	if got[0].ExpectedSource != "/deployments/forgejo/volumes/data" {
		t.Errorf("ExpectedSource wrong: %q", got[0].ExpectedSource)
	}
}

func TestDetectVolumeMismatches_PartialMismatch(t *testing.T) {
	// Multi-volume app: one volume matches, one doesn't. Only the bad one is
	// returned — we don't want noisy errors about volumes that are already fine.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: "c0"},
		ssh.MockCommand{
			Match: "docker inspect c0",
			Output: mountsLine("bind", "/deployments/app/volumes/data", "/data") +
				mountsLine("volume", "/var/lib/docker/volumes/abc/_data", "/uploads"),
		},
	)

	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "app", map[string]string{
		"/deployments/app/volumes/data":    "/data",
		"/deployments/app/volumes/uploads": "/uploads",
	})
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 mismatch (uploads), got %d: %v", len(got), got)
	}
	if got[0].ContainerPath != "/uploads" {
		t.Errorf("expected uploads mismatch, got %q", got[0].ContainerPath)
	}
}

func TestDetectVolumeMismatches_DestinationNotInExisting(t *testing.T) {
	// Existing container has no mount at the new container_path destination.
	// Not a mismatch — it's just a brand-new volume being added in this deploy.
	// Treat as safe.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: "c1"},
		ssh.MockCommand{
			Match:  "docker inspect c1",
			Output: mountsLine("bind", "/deployments/app/volumes/data", "/data"),
		},
	)

	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "app", map[string]string{
		"/deployments/app/volumes/data": "/data",
		"/deployments/app/volumes/new":  "/var/cache",
	})
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no mismatch for newly-added volume, got %v", got)
	}
}

func TestDetectVolumeMismatches_EmptyExpected(t *testing.T) {
	// No volumes declared in teploy.yml — nothing to check, no SSH calls needed.
	mock := ssh.NewMockExecutor("1.2.3.4")
	c := NewClient(mock)
	got, err := c.DetectVolumeMismatches(context.Background(), "app", nil)
	if err != nil {
		t.Fatalf("DetectVolumeMismatches: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result, got %v", got)
	}
}

func TestFormatMismatchError_IncludesEverythingUserNeeds(t *testing.T) {
	mismatches := []VolumeMismatch{
		{
			ContainerPath:  "/data",
			ExistingSource: "/var/lib/docker/volumes/199/_data",
			ExpectedSource: "/deployments/forgejo/volumes/data",
		},
	}
	err := FormatMismatchError("forgejo", mismatches)
	msg := err.Error()

	for _, want := range []string{
		"deploy aborted",
		"forgejo",
		"/data",
		"/var/lib/docker/volumes/199/_data",
		"/deployments/forgejo/volumes/data",
		"--migrate-volumes",
		"docker stop",
		"cp -a",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestMigrateVolumes_HappyPath(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: "deadbeef"},
		ssh.MockCommand{Match: "docker stop deadbeef", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "cp -a", Output: ""},
	)

	var buf bytes.Buffer
	mismatches := []VolumeMismatch{
		{
			ContainerPath:  "/data",
			ExistingSource: "/var/lib/docker/volumes/old/_data",
			ExpectedSource: "/deployments/app/volumes/data",
		},
	}

	if err := MigrateVolumes(context.Background(), mock, "app", mismatches, &buf); err != nil {
		t.Fatalf("MigrateVolumes: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Migrating 1 volume(s)") {
		t.Errorf("missing progress message; got:\n%s", out)
	}
	if !strings.Contains(out, "Migration complete") {
		t.Errorf("missing completion message; got:\n%s", out)
	}

	// Verify the cp command actually used cp -a with /. trailing the source.
	var cpCalled bool
	for _, call := range mock.Calls {
		if strings.Contains(call, "cp -a") && strings.Contains(call, "/. ") {
			cpCalled = true
			break
		}
	}
	if !cpCalled {
		t.Error("expected a `cp -a <src>/. <dst>/` call, none seen")
	}
}

func TestMigrateVolumes_NoExistingContainer(t *testing.T) {
	// User passed --migrate-volumes but the existing container was already gone.
	// Should no-op rather than error.
	mock := ssh.NewMockExecutor("1.2.3.4",
		ssh.MockCommand{Match: "docker ps -aq", Output: ""},
	)

	var buf bytes.Buffer
	mismatches := []VolumeMismatch{{
		ContainerPath:  "/data",
		ExistingSource: "/old",
		ExpectedSource: "/new",
	}}
	if err := MigrateVolumes(context.Background(), mock, "app", mismatches, &buf); err != nil {
		t.Fatalf("MigrateVolumes: %v", err)
	}
	if !strings.Contains(buf.String(), "no running container found") {
		t.Errorf("expected no-op message; got:\n%s", buf.String())
	}
}

func TestMigrateVolumes_EmptyMismatches(t *testing.T) {
	// Caller invoked us with nothing to do — fast-path return, no SSH calls.
	mock := ssh.NewMockExecutor("1.2.3.4")
	var buf bytes.Buffer
	if err := MigrateVolumes(context.Background(), mock, "app", nil, &buf); err != nil {
		t.Errorf("expected nil error for empty mismatches, got %v", err)
	}
	if len(mock.Calls) != 0 {
		t.Errorf("expected no SSH calls for empty mismatches, got %v", mock.Calls)
	}
}

func TestShellEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"o'reilly", `'o'\''reilly'`},
		{"$(whoami)", "'$(whoami)'"}, // metachars stay quoted, not interpreted
	}
	for _, c := range cases {
		got := shellEscape(c.in)
		if got != c.want {
			t.Errorf("shellEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
