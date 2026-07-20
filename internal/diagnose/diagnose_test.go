package diagnose

import (
	"errors"
	"strings"
	"testing"
)

func summaries(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Summary)
	}
	return strings.Join(out, " | ")
}

func TestOOMKilled(t *testing.T) {
	fs := Diagnose(Context{State: "exited", OOMKilled: true, ExitCode: 137})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "OOM") {
		t.Fatalf("expected OOM finding first, got: %s", summaries(fs))
	}
}

func TestMissingEnvVar(t *testing.T) {
	fs := Diagnose(Context{
		State:    "exited",
		ExitCode: 1,
		Logs:     `Error: environment variable "DATABASE_URL" is not set`,
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "DATABASE_URL") {
		t.Fatalf("expected env-var finding naming DATABASE_URL, got: %s", summaries(fs))
	}
	if !strings.Contains(fs[0].Try[0], "teploy secret set DATABASE_URL") {
		t.Fatalf("expected secret suggestion, got: %v", fs[0].Try)
	}
}

func TestDBConnRefused(t *testing.T) {
	fs := Diagnose(Context{
		State:    "exited",
		ExitCode: 1,
		Logs:     "panic: dial tcp 10.0.0.5:5432: connection refused (postgres)",
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "database") {
		t.Fatalf("expected db finding, got: %s", summaries(fs))
	}
}

func TestExit127(t *testing.T) {
	fs := Diagnose(Context{State: "exited", ExitCode: 127})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "exit 127") {
		t.Fatalf("expected exec-format finding, got: %s", summaries(fs))
	}
}

func TestGenericCrash(t *testing.T) {
	fs := Diagnose(Context{State: "exited", ExitCode: 3, Logs: "boom"})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "exit code 3") {
		t.Fatalf("expected generic crash finding, got: %s", summaries(fs))
	}
}

func TestPortMismatch(t *testing.T) {
	fs := Diagnose(Context{
		State:          "running",
		Stage:          "health",
		ConfiguredPort: 3000,
		ListenersKnown: true,
		Listeners:      []Listener{{Port: 8080}},
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "8080") || !strings.Contains(fs[0].Summary, "port: 3000") {
		t.Fatalf("expected port mismatch finding, got: %s", summaries(fs))
	}
}

func TestNoMismatchWhenConfiguredPortListening(t *testing.T) {
	fs := Diagnose(Context{
		State:          "running",
		ConfiguredPort: 3000,
		ListenersKnown: true,
		Listeners:      []Listener{{Port: 3000}},
	})
	for _, f := range fs {
		if strings.Contains(f.Summary, "teploy.yml says") {
			t.Fatalf("unexpected mismatch finding: %s", summaries(fs))
		}
	}
}

func TestLoopbackBind(t *testing.T) {
	fs := Diagnose(Context{
		State:          "running",
		ConfiguredPort: 3000,
		ListenersKnown: true,
		Listeners:      []Listener{{Port: 3000, LoopbackOnly: true}},
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "127.0.0.1") {
		t.Fatalf("expected loopback finding, got: %s", summaries(fs))
	}
}

func TestNothingListening(t *testing.T) {
	fs := Diagnose(Context{
		State:          "running",
		ConfiguredPort: 3000,
		ListenersKnown: true,
		Listeners:      nil,
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "nothing is listening") {
		t.Fatalf("expected nothing-listening finding, got: %s", summaries(fs))
	}
}

func TestUnknownListenersNoFalseClaims(t *testing.T) {
	// Tool missing inside the container — we know nothing, so no
	// listening-related findings may fire.
	fs := Diagnose(Context{State: "running", ConfiguredPort: 3000, ListenersKnown: false})
	if len(fs) != 0 {
		t.Fatalf("expected no findings on unknown listeners, got: %s", summaries(fs))
	}
}

func TestDiskFull(t *testing.T) {
	fs := Diagnose(Context{
		State: "exited",
		Err:   errors.New("write /var/lib/docker/tmp: no space left on device"),
	})
	if len(fs) == 0 || !strings.Contains(fs[0].Summary, "disk space") {
		t.Fatalf("expected disk finding, got: %s", summaries(fs))
	}
}

func TestParseListenersSS(t *testing.T) {
	out := "LISTEN 0      4096         0.0.0.0:8080       0.0.0.0:*\nLISTEN 0      128        127.0.0.1:9229       0.0.0.0:*\n"
	ls, ok := ParseListeners(out)
	if !ok || len(ls) != 2 {
		t.Fatalf("expected 2 listeners ok=true, got %v ok=%v", ls, ok)
	}
	byPort := map[int]Listener{}
	for _, l := range ls {
		byPort[l.Port] = l
	}
	if byPort[8080].LoopbackOnly || !byPort[9229].LoopbackOnly {
		t.Fatalf("loopback classification wrong: %v", ls)
	}
}

func TestParseListenersNetstat(t *testing.T) {
	out := "Active Internet connections (only servers)\nProto Recv-Q Send-Q Local Address           Foreign Address         State\ntcp        0      0 0.0.0.0:3000            0.0.0.0:*               LISTEN\ntcp6       0      0 :::3000                 :::*                    LISTEN\n"
	ls, ok := ParseListeners(out)
	if !ok || len(ls) != 1 || ls[0].Port != 3000 || ls[0].LoopbackOnly {
		t.Fatalf("expected single public :3000, got %v ok=%v", ls, ok)
	}
}

func TestParseListenersEmpty(t *testing.T) {
	if _, ok := ParseListeners(""); ok {
		t.Fatal("empty output must not claim knowledge")
	}
	if _, ok := ParseListeners("sh: ss: not found\n"); ok {
		t.Fatal("tool-missing output must not claim knowledge")
	}
}
