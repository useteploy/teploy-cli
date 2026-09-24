package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestGenerationCASPrefixSemantics(t *testing.T) {
	ctx := context.Background()

	t.Run("absent sidecar reads as generation 0 and passes", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "printf ok", Output: "ok"})
		out, err := mock.Run(ctx, GenerationCASPrefix("myapp", 0)+"printf ok")
		if err != nil || strings.TrimSpace(out) != "ok" {
			t.Fatalf("expected the effect to run, got %q %v", out, err)
		}
	})

	t.Run("equal committed generation passes", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "printf ok", Output: "ok"})
		mock.Files[GenerationSidecarPath("myapp")] = []byte("7\n")
		out, err := mock.Run(ctx, GenerationCASPrefix("myapp", 7)+"printf ok")
		if err != nil || strings.TrimSpace(out) != "ok" {
			t.Fatalf("expected the effect to run, got %q %v", out, err)
		}
	})

	t.Run("older committed generation passes", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4",
			ssh.MockCommand{Match: "printf ok", Output: "ok"})
		mock.Files[GenerationSidecarPath("myapp")] = []byte("6\n")
		if _, err := mock.Run(ctx, GenerationCASPrefix("myapp", 7)+"printf ok"); err != nil {
			t.Fatalf("an older committed generation never fences: %v", err)
		}
	})

	t.Run("newer committed generation refuses naming both", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4")
		mock.Files[GenerationSidecarPath("myapp")] = []byte("8\n")
		_, err := mock.Run(ctx, GenerationCASPrefix("myapp", 7)+"printf ok")
		if err == nil || !strings.Contains(err.Error(), "TEPLOY_GENERATION_FENCED 8 7") {
			t.Fatalf("refusal must name committed and expected generations, got %v", err)
		}
		if !GenerationFenced(err) {
			t.Fatal("GenerationFenced must match the refusal")
		}
	})

	t.Run("corrupt sidecar refuses fail-closed", func(t *testing.T) {
		mock := ssh.NewMockExecutor("1.2.3.4")
		mock.Files[GenerationSidecarPath("myapp")] = []byte("eight\n")
		_, err := mock.Run(ctx, GenerationCASPrefix("myapp", 99)+"printf ok")
		if err == nil || !strings.Contains(err.Error(), "TEPLOY_GENERATION_BADGEN") {
			t.Fatalf("a corrupt sidecar must fail closed, got %v", err)
		}
		if !GenerationFenced(err) {
			t.Fatal("GenerationFenced must match the BADGEN refusal")
		}
	})
}

func TestReadCommittedGeneration(t *testing.T) {
	ctx := context.Background()
	mock := ssh.NewMockExecutor("1.2.3.4")
	if gen, err := ReadCommittedGeneration(ctx, mock, "myapp"); err != nil || gen != 0 {
		t.Fatalf("absent sidecar must read as 0, got %d %v", gen, err)
	}
	mock.Files[GenerationSidecarPath("myapp")] = []byte("12\n")
	if gen, err := ReadCommittedGeneration(ctx, mock, "myapp"); err != nil || gen != 12 {
		t.Fatalf("expected 12, got %d %v", gen, err)
	}
	mock.Files[GenerationSidecarPath("myapp")] = []byte("junk\n")
	if _, err := ReadCommittedGeneration(ctx, mock, "myapp"); err == nil {
		t.Fatal("a corrupt sidecar must be an error, never a silent 0")
	}
}

// TestWriteFenced_PublishesGenerationSidecar pins the C01-8/9 commit shape:
// the guarded command renames state.json and the sidecar TOGETHER —
// authority first, fence second — so the generation a later CAS fences
// against is exactly the generation state.json names.
func TestWriteFenced_PublishesGenerationSidecar(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	s := &AppState{SchemaVersion: SchemaVersionV2, CurrentHash: "abc", UpdatedAt: time.Now().UTC(), OperationID: "op", Generation: 8}
	if err := WriteFenced(context.Background(), mock, "myapp", s, lk); err != nil {
		t.Fatalf("WriteFenced: %v", err)
	}
	gen, ok := mock.Files[GenerationSidecarPath("myapp")]
	if !ok || strings.TrimSpace(string(gen)) != "8" {
		t.Fatalf("the sidecar must be published with the committed generation, got %q", string(gen))
	}
	// Ordering: the state rename precedes the sidecar rename in the SAME
	// guarded command (state.json is the authority; the sidecar trails
	// in-shell, never leads).
	var commit string
	for _, c := range mock.Calls {
		if strings.Contains(c, "mv -f -- ") && strings.Contains(c, "/state.json") && strings.Contains(c, ".generation") {
			commit = c
		}
	}
	if commit == "" {
		t.Fatal("state and sidecar must commit in one guarded command")
	}
	stateIdx := strings.Index(commit, "state.json")
	genIdx := strings.Index(commit, ".generation")
	if stateIdx > genIdx {
		t.Fatalf("state.json must be renamed before the sidecar:\n%s", commit)
	}
}

// TestWriteFencedGeneration_RefusesOverNewerCommittedGeneration is the
// stale-rollback acceptance at the authority boundary: an operation that
// resolved the world at generation 7 cannot commit when the target already
// committed generation 8 — the refusal names the expected generation and
// state.json keeps the successor's content.
func TestWriteFencedGeneration_RefusesOverNewerCommittedGeneration(t *testing.T) {
	lk, mock := takeFencedLock(t, "myapp")
	mock.Files[GenerationSidecarPath("myapp")] = []byte("8\n")
	successorState := `{"schema_version":2,"deployment_type":"container","ingress_mode":"caddy","updated_at":"2026-09-24T00:00:00Z","operation_id":"successor","generation":8,"current_hash":"new"}`
	mock.Files["/deployments/myapp/state.json"] = []byte(successorState)

	s := &AppState{SchemaVersion: SchemaVersionV2, CurrentHash: "old", UpdatedAt: time.Now().UTC(), OperationID: "stale", Generation: 8}
	err := WriteFencedGeneration(context.Background(), mock, "myapp", s, lk, 7)
	if !errors.Is(err, ErrGenerationFenced) {
		t.Fatalf("expected ErrGenerationFenced, got %v", err)
	}
	if !strings.Contains(err.Error(), "expected predecessor 7") {
		t.Errorf("refusal must name the expected generation: %v", err)
	}
	if string(mock.Files["/deployments/myapp/state.json"]) != successorState {
		t.Fatal("a refused commit must not touch the successor's state.json")
	}
}

// TestWrite_PublishesSidecar pins the unfenced writer's sidecar
// maintenance (heal and static paths share it).
func TestWrite_PublishesSidecar(t *testing.T) {
	mock := ssh.NewMockExecutor("1.2.3.4")
	s := &AppState{SchemaVersion: SchemaVersionV2, CurrentHash: "abc", UpdatedAt: time.Now().UTC(), OperationID: "op", Generation: 3}
	if err := Write(context.Background(), mock, "myapp", s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	gen, ok := mock.Files[GenerationSidecarPath("myapp")]
	if !ok || strings.TrimSpace(string(gen)) != "3" {
		t.Fatalf("Write must publish the sidecar, got %q", string(gen))
	}
}
