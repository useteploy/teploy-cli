package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/ssh"
)

func TestCheckOwnKey(t *testing.T) {
	s3 := S3Config{Bucket: "b", Prefix: "teploy/srv1", PreviousPrefixes: []PreviousPrefix{{Prefix: "old"}}}
	for _, ok := range []string{
		"s3://b/teploy/srv1/app/accessories/db/20261104-030000.pgdump.age",
		"s3://b/old/app/volumes/20261104-030000.tar.gz",
	} {
		if err := s3.CheckOwnKey(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"s3://other/teploy/srv1/app/x",
		"s3://b/teploy/srv2/app/x",
		"s3://b/teploy/srv1-evil/app/x",
		"s3://b/oldish/app/x",
		"s3://b/teploy/srv1/../srv2/x",
		"s3://b/teploy/srv1/./x",
		"s3://b/teploy/srv1//x",
		"s3://b/teploy/srv1/x\nrm",
		"s3://b/",
		"b/teploy/srv1/x",
		"/etc/passwd",
	} {
		if err := s3.CheckOwnKey(bad); !errors.Is(err, ErrKeyOutsideTarget) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A target with no prefix has the whole bucket.
	if err := (S3Config{Bucket: "b"}).CheckOwnKey("s3://b/app/x"); err != nil {
		t.Fatal(err)
	}
}

func TestReadObjectStartRefusesBeforeAnyCommand(t *testing.T) {
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "mine"}
	for _, tc := range []struct {
		key string
		n   int
	}{
		{"s3://b/mine/x", 0},
		{"s3://b/mine/x", -1},
		{"s3://b/mine/x", MaxObjectStartBytes + 1},
		{"s3://b/other/x", 1024},
	} {
		mock := ssh.NewMockExecutor("h")
		if _, err := NewClient(mock, io.Discard).ReadObjectStart(context.Background(), s3, tc.key, tc.n); err == nil {
			t.Errorf("%+v: no refusal", tc)
		}
		if len(mock.Calls) != 0 {
			t.Errorf("%+v ran %v", tc, mock.Calls)
		}
	}
}

// fakeAWS is an aws on PATH whose get-object runs the given shell; "$out" is the
// outfile and "$range" the requested range.
func fakeAWS(t *testing.T, getObject string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "aws-cli/2.30.0 Python/3"; exit 0; fi
out=
range=
while [ $# -gt 0 ]; do
  case "$1" in
    --range) range=$2; shift;;
    */o) out=$1;;
  esac
  shift
done
` + getObject + "\n"
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEPLOY_BACKUP_TMPDIR", t.TempDir())
}

func tmpEntries(t *testing.T) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(os.Getenv("TEPLOY_BACKUP_TMPDIR"))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// The real script through a real shell: binary bytes round trip, the range reaches
// aws, the temp file does not outlive the command.
func TestReadObjectStartReadsTheRange(t *testing.T) {
	fakeAWS(t, `[ "$range" = "bytes=0-7" ] || { echo "wrong range $range" >&2; exit 9; }
printf 'age\377\000\001\002\003' > "$out"
echo '{"ContentLength": 8, "ContentRange": "bytes 0-7/4096"}'`)
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "mine"}
	got, err := NewClient(ssh.NewLocalExecutor(), io.Discard).ReadObjectStart(context.Background(), s3, "s3://b/mine/app/x.pgdump.age", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data, []byte("age\xff\x00\x01\x02\x03")) {
		t.Fatalf("%q", got.Data)
	}
	if got.TotalSize == nil || *got.TotalSize != 4096 || !got.Encrypted || got.Key != "s3://b/mine/app/x.pgdump.age" {
		t.Fatalf("%+v", got)
	}
	if e := tmpEntries(t); len(e) != 0 {
		t.Fatalf("left behind: %v", e)
	}
}

func TestReadObjectStartFailures(t *testing.T) {
	s3 := S3Config{Bucket: "b", Region: "r"}
	for _, tc := range []struct{ name, fake, want string }{
		{"store refuses", `echo "An error occurred (AccessDenied)" >&2; exit 254`, "AccessDenied"},
		{"silent failure still has a reason", `exit 2`, "no reason"},
		{"store ignored the range", `head -c 64 /dev/zero > "$out"; echo '{"ContentLength": 64}'`, "ignored the requested range"},
		{"length disagrees", `printf abc > "$out"; echo '{"ContentLength": 8}'`, "said 8 bytes and sent 3"},
		{"no bytes", `: > "$out"; echo '{"ContentLength": 0}'`, "no bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeAWS(t, tc.fake)
			_, err := NewClient(ssh.NewLocalExecutor(), io.Discard).ReadObjectStart(context.Background(), s3, "s3://b/x", 8)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v", err)
			}
			if e := tmpEntries(t); len(e) != 0 {
				t.Fatalf("left behind: %v", e)
			}
		})
	}
}

// It reads through the stored credentials, decrypting only those, and never asks age
// about the object.
func TestReadObjectStartCommand(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "aws-cli/2.30.0\n"},
		ssh.MockCommand{Match: "sh -c", Output: `{"ContentLength":3}` + "\n" + objectStartBodyMark + "\nYWJj\n"},
	)
	s3 := S3Config{Bucket: "b", Region: "r", StoredTarget: "/etc/teploy/backup-target.json"}
	if _, err := NewClient(mock, io.Discard).ReadObjectStart(context.Background(), s3, "s3://b/x.age", 3); err != nil {
		t.Fatal(err)
	}
	cmd := mock.Calls[len(mock.Calls)-1]
	if !strings.Contains(cmd, "exec aws s3api get-object") || !strings.Contains(cmd, "/etc/teploy/backup-target.json") {
		t.Fatalf("not through the stored target: %s", cmd)
	}
	if strings.Contains(cmd, "AWS_ACCESS_KEY_ID") || strings.Contains(cmd, "AWS_SECRET_ACCESS_KEY") {
		t.Fatalf("credentials in the command: %s", cmd)
	}
	if strings.Count(cmd, "age -d") != 1 || !strings.Contains(cmd, `sys.stdout.write(json.load(open(sys.argv[1])).get("credentials")`) {
		t.Fatalf("age is for the credentials and nothing else: %s", cmd)
	}
}
