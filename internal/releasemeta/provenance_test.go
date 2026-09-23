package releasemeta

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func provenanceFixture(app, release string) *Provenance {
	return &Provenance{
		App:                app,
		Release:            release,
		Revision:           "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f",
		Dirty:              true,
		ContextPath:        ".",
		ContextFingerprint: strings.Repeat("f", 64),
		Dockerfile:         "Dockerfile",
		DockerfileSHA256:   strings.Repeat("d", 64),
		Platform:           "linux/amd64",
		ImageRef:           "myapp-build-v1",
		ImageDigest:        "sha256:" + strings.Repeat("a", 64),
		DigestPinned:       false,
		ManifestSHA256:     strings.Repeat("c", 64),
	}
}

// provenanceTestExecutor answers the commands WriteAttemptProvenance issues
// (mkdir, atomic upload) and lets framed reads resolve from recorded state.
func provenanceTestExecutor(t *testing.T, extra ...ssh.MockCommand) *ssh.MockExecutor {
	t.Helper()
	return ssh.NewMockExecutor("1.2.3.4", append(extra,
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "UPLOAD:", Output: ""},
		ssh.MockCommand{Match: "mv -f --", Output: ""},
	)...)
}

func TestWriteAttemptProvenance_PersistsIntoAttemptNamespace(t *testing.T) {
	mock := provenanceTestExecutor(t)
	att := MustAttempt("myapp", "v1")

	if err := WriteAttemptProvenance(context.Background(), mock, att, provenanceFixture("myapp", "v1")); err != nil {
		t.Fatalf("WriteAttemptProvenance: %v", err)
	}

	raw, ok := mock.Files[AttemptProvenancePath(att)]
	if !ok {
		t.Fatalf("provenance receipt not written to %s\nfiles: %v", AttemptProvenancePath(att), mock.Files)
	}
	if !strings.Contains(string(raw), `"revision":"462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f"`) {
		t.Errorf("revision not recorded: %s", raw)
	}
	if !strings.Contains(string(raw), `"dirty":true`) {
		t.Errorf("dirty flag not recorded: %s", raw)
	}

	got, err := ReadAttemptProvenance(context.Background(), mock, att)
	if err != nil {
		t.Fatalf("ReadAttemptProvenance: %v", err)
	}
	if got == nil {
		t.Fatal("expected the provenance receipt back")
	}
	if got.Attempt != att.Name() {
		t.Errorf("receipt does not carry the writing attempt: %q vs %q", got.Attempt, att.Name())
	}
	if got.Revision != "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f" || !got.Dirty ||
		got.ContextFingerprint != strings.Repeat("f", 64) ||
		got.Dockerfile != "Dockerfile" || got.DockerfileSHA256 != strings.Repeat("d", 64) ||
		got.Platform != "linux/amd64" || got.ImageRef != "myapp-build-v1" ||
		got.ImageDigest != "sha256:"+strings.Repeat("a", 64) ||
		got.DigestPinned || got.ManifestSHA256 != strings.Repeat("c", 64) {
		t.Errorf("provenance round-trip lost fields: %+v", got)
	}
}

func TestWriteAttemptProvenance_RejectsForeignIdentity(t *testing.T) {
	mock := provenanceTestExecutor(t)
	att := MustAttempt("myapp", "v1")
	if err := WriteAttemptProvenance(context.Background(), mock, att, provenanceFixture("otherapp", "v1")); err == nil {
		t.Fatal("a provenance record describing another app must be refused at write time")
	}
	if err := WriteAttemptProvenance(context.Background(), mock, att, provenanceFixture("myapp", "v2")); err == nil {
		t.Fatal("a provenance record describing another release must be refused at write time")
	}
}

func TestReadAttemptProvenance_IdentityMismatchRefused(t *testing.T) {
	att := MustAttempt("myapp", "v1")
	foreign := fmt.Sprintf(`{"schema_version":%d,"app":"myapp","release":"v1","attempt":"v1.0000000000000000","revision":"x"}`, ProvenanceSchemaVersion)
	mock := provenanceTestExecutor(t,
		ssh.MockCommand{Match: "if [ ! -e '" + AttemptProvenancePath(att) + "'", Output: "present\n" + foreign},
	)
	if _, err := ReadAttemptProvenance(context.Background(), mock, att); err == nil {
		t.Fatal("a receipt describing a different attempt must be refused, not accepted")
	}
}

func TestReadAttemptProvenance_AbsentIsNilNil(t *testing.T) {
	att := MustAttempt("myapp", "v1")
	mock := provenanceTestExecutor(t)
	got, err := ReadAttemptProvenance(context.Background(), mock, att)
	if err != nil || got != nil {
		t.Fatalf("confirmed-missing provenance must be (nil, nil), got (%v, %v)", got, err)
	}
}

// C04 retry stability, artifact side: the F08 attempt namespace gives every
// attempt of the same release its own write-once provenance receipt — a
// response-loss retry lands beside the first attempt's receipt, never
// over it, and both name the same resolved source.
func TestAttemptProvenance_RetriesGetDistinctImmutableReceipts(t *testing.T) {
	mock := provenanceTestExecutor(t)
	first := MustAttempt("myapp", "v1")
	second := MustAttempt("myapp", "v1")

	if err := WriteAttemptProvenance(context.Background(), mock, first, provenanceFixture("myapp", "v1")); err != nil {
		t.Fatalf("first attempt write: %v", err)
	}
	if err := WriteAttemptProvenance(context.Background(), mock, second, provenanceFixture("myapp", "v1")); err != nil {
		t.Fatalf("second attempt write: %v", err)
	}

	if AttemptProvenancePath(first) == AttemptProvenancePath(second) {
		t.Fatalf("two attempts of one release share a provenance path: %s", AttemptProvenancePath(first))
	}
	if _, ok := mock.Files[AttemptProvenancePath(first)]; !ok {
		t.Fatal("the first attempt's receipt did not survive the retry")
	}
	for _, att := range []Attempt{first, second} {
		got, err := ReadAttemptProvenance(context.Background(), mock, att)
		if err != nil || got == nil {
			t.Fatalf("attempt %s receipt unreadable: (%v, %v)", att.Name(), got, err)
		}
		if got.Revision != "462d7a7b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f" {
			t.Errorf("attempt %s re-resolved to a different source revision: %s", att.Name(), got.Revision)
		}
	}
}
