package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
	"strings"
	"testing"
)

func TestBackupHeadThroughTheStoredTarget(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte("age-encryption.org/v1\n"))
	mock := ssh.NewMockExecutor("192.0.2.10",
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: targetStored},
		ssh.MockCommand{Match: "which aws", Output: "aws-cli/2.30.0\n"},
		ssh.MockCommand{Match: "sh -c", Output: `{"ContentLength":22,"ContentRange":"bytes 0-21/1048576"}` + "\nTEPLOY-OBJECT-START-BODY\n" + body + "\n"},
	)
	key := "s3://b/teploy/srv1/app/accessories/db/20261104-030000.pgdump.age"
	d, err := readBackupHead(context.Background(), mock, key, 22)
	if err != nil {
		t.Fatal(err)
	}
	if d.DataBase64 != body || d.Length != 22 || *d.TotalSize != 1048576 || !d.Encrypted || d.RequestedBytes != 22 {
		t.Fatalf("%+v", d)
	}
	out, _ := json.Marshal(d)
	if strings.Contains(string(out), `"x"`) {
		t.Fatalf("credentials in output: %s", out)
	}
	// Refusals.
	for _, tc := range []struct {
		key string
		n   int
	}{{"s3://b/teploy/srv2/app/x", 22}, {"s3://other/teploy/srv1/app/x", 22}, {key, backup.MaxObjectStartBytes + 1}, {key, 0}} {
		mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: targetStored})
		if _, err := readBackupHead(context.Background(), mock, tc.key, tc.n); err == nil {
			t.Fatalf("%+v was read", tc)
		}
	}
	mock = ssh.NewMockExecutor("h", ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"})
	if _, err := readBackupHead(context.Background(), mock, key, 22); err == nil {
		t.Fatal("no target, and it read anyway")
	}
}
