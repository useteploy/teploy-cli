package cli

import (
	"context"
	"errors"
	"github.com/useteploy/teploy/internal/backup"
	"github.com/useteploy/teploy/internal/ssh"
	"io"
	"strings"
	"testing"
	"time"
)

var ownersNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// owners with a key limited to the current prefix: previous locations answer
// AccessDenied.
func ownersMock(extra ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := []ssh.MockCommand{{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"}}
	cmds = append(cmds, extra...)
	cmds = append(cmds, ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`})
	return ssh.NewMockExecutor("h", cmds...)
}

func ownersLocs() []backup.S3Config {
	return []backup.S3Config{{Bucket: "b", Region: "r", Prefix: "cur"}, {Bucket: "b", Region: "r", Prefix: ""}, {Bucket: "b", Region: "r", Prefix: "old"}}
}

const fp = "aaaaaaaaaaaaaaaa"

var (
	lsCur  = "aws s3 ls 's3://b/cur/"
	lsRoot = "aws s3 ls 's3://b/'"
	lsOld  = "aws s3 ls 's3://b/old/"
	denied = errors.New("exit status 254: An error occurred (AccessDenied) when calling ListObjectsV2")
)

func TestOwnersSkipsPreviousLocationsItCannotReach(t *testing.T) {
	stale := ownersNow.Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	mock := ownersMock(
		ssh.MockCommand{Match: lsCur, Output: stale + " 20 cur/app/.owners/" + fp + "\n"},
		ssh.MockCommand{Match: lsRoot, Err: denied},
		ssh.MockCommand{Match: lsOld, Output: stale + " 20 old/app/.owners/" + fp + "\n"},
	)
	markers, skipped, err := listOwnerMarkers(context.Background(), backup.NewClient(mock, io.Discard), ownersLocs())
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 2 || len(skipped) != 1 || skipped[0].Prefix != "" || !strings.Contains(skipped[0].Reason, "AccessDenied") {
		t.Fatalf("%+v %+v", markers, skipped)
	}
	// The current location not answering is not "no owners".
	mock = ownersMock(ssh.MockCommand{Match: lsCur, Err: denied})
	if _, _, err := listOwnerMarkers(context.Background(), backup.NewClient(mock, io.Discard), ownersLocs()); err == nil {
		t.Fatal("no error")
	}
}

func TestOwnersClearDoesNotStopPartway(t *testing.T) {
	now := ownersNow
	stale := now.Add(-30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	t.Run("key limited to the current prefix", func(t *testing.T) {
		mock := ownersMock(
			ssh.MockCommand{Match: lsCur, Output: stale + " 20 cur/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: lsRoot, Err: denied},
			ssh.MockCommand{Match: lsOld, Err: denied},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
		n, skipped, err := clearOwnerMarkers(context.Background(), mock, backup.NewClient(mock, io.Discard), ownersLocs(), fp, false, now)
		if err != nil || n != 1 || len(skipped) != 2 {
			t.Fatalf("%d %+v %v", n, skipped, err)
		}
		if !strings.Contains(mock.Inputs[len(mock.Inputs)-1], `"skipped_locations"`) {
			t.Fatalf("%v", mock.Inputs)
		}
	})
	t.Run("an unreachable location in the middle does not stop the last", func(t *testing.T) {
		mock := ownersMock(
			ssh.MockCommand{Match: lsCur, Output: stale + " 20 cur/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: lsRoot, Err: denied},
			ssh.MockCommand{Match: lsOld, Output: stale + " 20 old/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
		n, skipped, err := clearOwnerMarkers(context.Background(), mock, backup.NewClient(mock, io.Discard), ownersLocs(), fp, true, now)
		if err != nil || n != 2 || len(skipped) != 1 {
			t.Fatalf("%d %+v %v", n, skipped, err)
		}
	})
	t.Run("the current location failing still lets the others go, and is the error", func(t *testing.T) {
		mock := ownersMock(
			ssh.MockCommand{Match: lsCur, Err: denied},
			ssh.MockCommand{Match: lsRoot, Output: ""},
			ssh.MockCommand{Match: lsOld, Output: stale + " 20 old/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
		n, _, err := clearOwnerMarkers(context.Background(), mock, backup.NewClient(mock, io.Discard), ownersLocs(), fp, false, now)
		if err == nil || !strings.Contains(err.Error(), "AccessDenied") || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
	})
	t.Run("a removal failing in a previous location is reported and the rest go on", func(t *testing.T) {
		mock := ownersMock(
			ssh.MockCommand{Match: lsCur, Output: stale + " 20 cur/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: lsRoot, Output: ""},
			ssh.MockCommand{Match: lsOld, Output: stale + " 20 old/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: "aws s3 rm 's3://b/old/", Err: denied},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
		n, skipped, err := clearOwnerMarkers(context.Background(), mock, backup.NewClient(mock, io.Discard), ownersLocs(), fp, false, now)
		if err != nil || n != 1 || len(skipped) != 1 || skipped[0].Prefix != "old" {
			t.Fatalf("%d %+v %v", n, skipped, err)
		}
	})
	t.Run("a refusal anywhere removes nothing anywhere", func(t *testing.T) {
		recent := now.Add(-time.Hour).Format("2006-01-02 15:04:05")
		mock := ownersMock(
			ssh.MockCommand{Match: lsCur, Output: stale + " 20 cur/app/.owners/" + fp + "\n"},
			ssh.MockCommand{Match: lsRoot, Err: denied},
			ssh.MockCommand{Match: lsOld, Output: recent + " 20 old/app/.owners/" + fp + "\n"},
		)
		n, _, err := clearOwnerMarkers(context.Background(), mock, backup.NewClient(mock, io.Discard), ownersLocs(), fp, false, now)
		if !errors.Is(err, backup.ErrMarkerRecent) || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "aws s3 rm") {
				t.Fatalf("removed after a refusal: %s", c)
			}
		}
		if !strings.Contains(mock.Inputs[len(mock.Inputs)-1], `"outcome":"refused"`) {
			t.Fatalf("%v", mock.Inputs)
		}
	})
}

func TestAWSInstallScriptStaysVerified(t *testing.T) {
	for _, want := range []string{"VALIDSIG", "curl -fsSL"} {
		if !strings.Contains(backup.AWSCLIInstallScript, want) {
			t.Fatalf("no %q", want)
		}
	}
	if strings.Index(backup.AWSCLIInstallScript, "command -v curl") > strings.Index(backup.AWSCLIInstallScript, "curl -fsSL") {
		t.Fatal("curl is installed after it is used")
	}
}

func TestAWSRemedyNamesTheToolsOnlyPath(t *testing.T) {
	if !strings.Contains(backup.AWSCLIRemedy, "backup-preflight") || !strings.Contains(backup.AWSCLIRemedy, "--install-tools") {
		t.Fatal(backup.AWSCLIRemedy)
	}
	h := newServerBackupTargetSetCmd(&Flags{}).Long
	if !strings.Contains(h, "does not install the aws CLI") {
		t.Fatal(h)
	}
}
