//go:build !windows

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

// Regressions from the Slice 4 re-review, named after what they protect.

func publishCalls(mock *ssh.MockExecutor) (copies, reuploads int, joined string) {
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "aws s3 cp 's3://b/.incoming/k/1.pgdump' 's3://b/k/1.pgdump'") {
			copies++
		}
		if strings.HasPrefix(c, "aws s3 cp '/w/x' 's3://b/k/1.pgdump' --metadata 'sha256=abc'") {
			reuploads++
		}
	}
	return copies, reuploads, strings.Join(mock.Calls, "\n")
}

const putCopyMatch = "aws s3 cp 's3://b/.incoming/k/1.pgdump' 's3://b/k/1.pgdump'"

// B2 refuses a copy that would change the checksum algorithm, on every object big
// enough to be multipart. The verified local file goes straight to its final name.
func TestPutReuploadsWhenTheStoreRefusesTheCopy(t *testing.T) {
	ctx := context.Background()
	for _, refusal := range []string{
		"exit status 1: copy failed: An error occurred (InvalidArgument) when calling the UploadPartCopy operation: checksum algorithm cannot be changed",
		"exit status 1: An error occurred (NotImplemented) when calling the GetObjectTagging operation: not implemented",
		"exit status 1: An error occurred (InternalError) when calling the CompleteMultipartUpload operation: copy failed",
	} {
		mock := putMock(ssh.MockCommand{Match: putCopyMatch, Err: errors.New(refusal)})
		var out strings.Builder
		obj, err := NewClient(mock, &out).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump")
		if err != nil {
			t.Fatalf("%s: %v", refusal, err)
		}
		if obj.Key != "s3://b/k/1.pgdump" {
			t.Fatalf("%+v", obj)
		}
		copies, reuploads, joined := publishCalls(mock)
		if copies != 1 || reuploads != 1 {
			t.Fatalf("copies %d reuploads %d: %s", copies, reuploads, joined)
		}
		// The final object is verified as itself: its bytes are read back, and the
		// pending one is gone.
		if !strings.Contains(joined, "{ aws s3 cp 's3://b/k/1.pgdump' -") || !strings.Contains(joined, "aws s3 rm 's3://b/.incoming/k/1.pgdump'") {
			t.Fatalf("calls: %s", joined)
		}
		if !strings.Contains(out.String(), "uploaded straight to its final name") {
			t.Fatalf("out: %s", out.String())
		}
	}
}

// A large object's re-upload keeps the provider checksum.
func TestPutReuploadOfALargeObjectStaysProviderChecked(t *testing.T) {
	mock := putMock(
		ssh.MockCommand{Match: putCopyMatch, Err: errors.New("exit status 1: An error occurred (InvalidRequest) when calling the UploadPartCopy operation")},
		ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"},"ChecksumSHA256":"AAA=-3"}`},
	)
	cl := NewClient(mock, io.Discard)
	cl.readBack = 1
	if _, err := cl.put(context.Background(), S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if _, _, joined := publishCalls(mock); !strings.Contains(joined, "aws s3 cp '/w/x' 's3://b/k/1.pgdump' --metadata 'sha256=abc' --checksum-algorithm SHA256") {
		t.Fatalf("calls: %s", joined)
	}
}

// A dead connection is not a refusal: a re-upload would fail the same way, and the
// run should say the real thing. A re-upload that fails or does not verify leaves
// nothing at the final name.
func TestPutFailuresThatAreNotRefusals(t *testing.T) {
	ctx := context.Background()
	mock := putMock(ssh.MockCommand{Match: putCopyMatch, Err: errors.New("exit status 1: Could not connect to the endpoint URL")})
	_, err := NewClient(mock, io.Discard).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump")
	if err == nil || !strings.Contains(err.Error(), "publishing the verified upload") {
		t.Fatalf("err = %v", err)
	}
	if _, reuploads, joined := publishCalls(mock); reuploads != 0 || !strings.Contains(joined, "aws s3 rm 's3://b/.incoming/k/1.pgdump'") {
		t.Fatalf("calls: %s", joined)
	}
	refused := ssh.MockCommand{Match: putCopyMatch, Err: errors.New("exit status 1: An error occurred (InvalidRequest) when calling the UploadPartCopy operation")}
	mock = putMock(refused, ssh.MockCommand{Match: "aws s3 cp '/w/x' 's3://b/k/1.pgdump'", Err: errors.New("exit status 1: connection reset")})
	if _, err = NewClient(mock, io.Discard).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err == nil {
		t.Fatal("a failed re-upload must fail")
	}
	if !strings.Contains(strings.Join(rmKeys(mock), ","), "s3://b/k/1.pgdump") {
		t.Fatalf("final not cleaned: %v", rmKeys(mock))
	}
	mock = putMock(refused, ssh.MockCommand{Match: "{ aws s3 cp 's3://b/k/1.pgdump' -", Output: "zzz  -\n"})
	if _, err = NewClient(mock, io.Discard).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.Join(rmKeys(mock), ","), "s3://b/k/1.pgdump") {
		t.Fatalf("an unverified final object was kept: %v", rmKeys(mock))
	}
}

// R2 has no GetObjectTagging, which the copy's default props read.
func TestPublishNamesItsCopyProps(t *testing.T) {
	mock := putMock()
	if _, err := NewClient(mock, io.Discard).put(context.Background(), S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if _, _, joined := publishCalls(mock); !strings.Contains(joined, putCopyMatch+" --copy-props metadata-directive") {
		t.Fatalf("calls: %s", joined)
	}
}

// aws CLI v1 has no --copy-props, and its multipart copy drops metadata, so a final
// object with no recorded checksum is replaced: fetch checks against that record.
func TestPublishOnAWSCLIv1(t *testing.T) {
	ctx := context.Background()
	mock := putMock(ssh.MockCommand{Match: putCopyMatch + " --copy-props", Err: errors.New("exit status 2: Unknown options: --copy-props,metadata-directive")})
	if _, err := NewClient(mock, io.Discard).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if copies, reuploads, joined := publishCalls(mock); copies != 2 || reuploads != 0 {
		t.Fatalf("copies %d reuploads %d: %s", copies, reuploads, joined)
	}
	mock = putMock(
		ssh.MockCommand{Match: "aws s3api head-object --bucket 'b' --key 'k/1.pgdump'", Output: `{"ContentLength":4096}`},
	)
	if _, err := NewClient(mock, io.Discard).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if _, reuploads, joined := publishCalls(mock); reuploads != 1 {
		t.Fatalf("a final object that lost its checksum was kept: %s", joined)
	}
}

// One tree under the base: a lifecycle rule can only name a prefix.
func TestPendingUploadsLiveInOneTree(t *testing.T) {
	for _, tc := range []struct{ prefix, key, want string }{
		{"", "s3://b/a/accessories/db/1.pgdump", "s3://b/.incoming/a/accessories/db/1.pgdump"},
		{"srv1", "s3://b/srv1/a/accessories/db/databases/x/1.pgdump", "s3://b/srv1/.incoming/a/accessories/db/databases/x/1.pgdump"},
		{"srv1/eu", "s3://b/srv1/eu/a/volumes/1.tar.gz", "s3://b/srv1/eu/.incoming/a/volumes/1.tar.gz"},
		{"srv1", "s3://b/elsewhere/1.pgdump", "s3://b/.incoming/elsewhere/1.pgdump"},
	} {
		if got := incomingKey(S3Config{Bucket: "b", Prefix: tc.prefix}, tc.key); got != tc.want {
			t.Fatalf("%s: %s", tc.key, got)
		}
	}
	// And nothing in it reads as a backup.
	if _, ok := classifyListedKey(".incoming", ".incoming/a/volumes/20260101-000000.tar.gz"); ok {
		t.Fatal("a pending upload was listed")
	}
}

// A marker on a versioned store costs a stored copy per write.
func TestPruneDoesNotRewriteItsMarker(t *testing.T) {
	r := "a/volumes/"
	marker := ssh.MockCommand{Match: "aws s3 ls 's3://b/a/.owners/'", Output: "d t 20 a/.owners/0123456789abcdef\n"}
	body := ssh.MockCommand{Match: "aws s3 cp 's3://b/a/.owners/0123456789abcdef' -", Output: `{"recipient":"` + testServerRecipient + `","first_seen":"2026-01-01T00:00:00Z"}`}
	recipient := ssh.MockCommand{Match: "for p in /etc/teploy/age-key", Output: testServerRecipient + "\n"}
	mock := ssh.NewMockExecutor("h", append([]ssh.MockCommand{marker, body, recipient, {Match: "which aws", Output: "/usr/bin/aws\n"},
		{Match: "aws s3 ls", Output: "d t 1000 " + r + "20240103-000000.tar.gz\nd t 1000 " + r + "20240102-000000.tar.gz\n"}, {Match: "aws s3 rm", Output: ""}}, storeStubs...)...)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	clock := false
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "printf") && strings.Contains(c, ".owners/") {
			t.Fatalf("the marker was rewritten: %s", c)
		}
		clock = clock || (strings.HasPrefix(c, "printf") && strings.Contains(c, "s3://b/.incoming/.clock-"))
	}
	if !clock {
		t.Fatalf("the clock was not read under .incoming/: %v", mock.Calls)
	}
}

func TestLifecycleCovers(t *testing.T) {
	s3 := S3Config{Bucket: "b", Prefix: "srv1"}
	for _, tc := range []struct{ name, reply, want string }{
		{"exact", `{"Rules":[{"Status":"Enabled","Filter":{"Prefix":"srv1/.incoming/"},"Expiration":{"Days":1}}]}`, LifecycleCovered},
		{"legacy prefix field", `{"Rules":[{"Status":"Enabled","Prefix":"srv1/.incoming/","NoncurrentVersionExpiration":{"NoncurrentDays":1}}]}`, LifecycleCovered},
		{"disabled", `{"Rules":[{"Status":"Disabled","Filter":{"Prefix":"srv1/.incoming/"},"Expiration":{"Days":1}}]}`, LifecycleAbsent},
		{"another server's", `{"Rules":[{"Status":"Enabled","Filter":{"Prefix":"srv2/.incoming/"},"Expiration":{"Days":1}}]}`, LifecycleAbsent},
		{"expires nothing", `{"Rules":[{"Status":"Enabled","Filter":{"Prefix":"srv1/.incoming/"}}]}`, LifecycleAbsent},
		{"the whole bucket is not this rule", `{"Rules":[{"Status":"Enabled","Filter":{},"Expiration":{"Days":1}}]}`, LifecycleAbsent},
		{"no rules", `{"Rules":[]}`, LifecycleAbsent},
		{"garbage", `nope`, LifecycleUnknown},
	} {
		if got := lifecycleCovers([]byte(tc.reply), s3); got != tc.want {
			t.Fatalf("%s: %s", tc.name, got)
		}
	}
	ctx := context.Background()
	c := NewClient(ssh.NewMockExecutor("h", ssh.MockCommand{Match: "aws s3api get-bucket-lifecycle-configuration", Err: errors.New("exit status 254: An error occurred (NoSuchLifecycleConfiguration)")}), io.Discard)
	if got := c.incomingLifecycle(ctx, s3); got != LifecycleAbsent {
		t.Fatal(got)
	}
	c = NewClient(ssh.NewMockExecutor("h", ssh.MockCommand{Match: "aws s3api get-bucket-lifecycle-configuration", Err: errors.New("exit status 254: AccessDenied")}), io.Discard)
	if got := c.incomingLifecycle(ctx, s3); got != LifecycleUnknown {
		t.Fatal(got)
	}
}

// ---- the policy file: one location, whoever asks ----

// policySandbox runs the policy scripts for real with /etc/teploy moved to a temp
// directory and a sudo stand-in that opens it for the command's length.
type policySandbox struct {
	t         *testing.T
	root, etc string
	home, bin string
}

func newPolicySandbox(t *testing.T, sudoWorks bool) *policySandbox {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("modes do not stop root")
	}
	root := t.TempDir()
	s := &policySandbox{t: t, root: root, etc: filepath.Join(root, "etc-teploy"), home: filepath.Join(root, "home"), bin: filepath.Join(root, "bin")}
	for _, d := range []string{s.home, s.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sudo := "#!/bin/sh\nexit 1\n"
	if sudoWorks {
		sudo = fmt.Sprintf("#!/bin/sh\nshift\nm=$(stat -f %%Lp %[1]q 2>/dev/null || stat -c %%a %[1]q 2>/dev/null)\nchmod 700 %[1]q\n\"$@\"\nrc=$?\nchmod \"$m\" %[1]q\nexit $rc\n", s.etc)
	}
	if err := os.WriteFile(filepath.Join(s.bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(s.etc, 0o700) })
	return s
}

func (s *policySandbox) run(script, stdin string) (string, string, int) {
	s.t.Helper()
	script = strings.ReplaceAll(script, "/etc/teploy", s.etc)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"HOME=" + s.home, "PATH=" + s.bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			s.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// Rotation through passwordless sudo: what set stored is what show and the run read,
// and unset removes it. It used to land in ~/.config while /etc kept winning.
func TestPolicyWriteUsesTheSudoPathReadsUse(t *testing.T) {
	s := newPolicySandbox(t, true)
	if err := os.Mkdir(s.etc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.etc, "backup-policy.json"), []byte(`{"v":"1","recipient":"OLD"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.etc, 0); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := s.run(writeFleetPolicyScript, `{"v":"1","recipient":"NEW"}`)
	if code != 0 || strings.Contains(stderr, "fallback") {
		t.Fatalf("write: %d %s %s", code, out, stderr)
	}
	if want := "written " + s.etc + "/backup-policy.json"; strings.TrimSpace(out) != want {
		t.Fatalf("%q, want %q", out, want)
	}
	out, _, code = s.run(readPolicyScript, "")
	if code != 0 || !strings.Contains(out, "NEW") || strings.Contains(out, "OLD") {
		t.Fatalf("read: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(s.home, ".config/teploy/backup-policy.json")); err == nil {
		t.Fatal("a second policy was written under ~/.config")
	}
	out, stderr, code = s.run(unsetFleetPolicyScript, "")
	if code != 0 || !strings.Contains(out, "removed "+s.etc+"/backup-policy.json") {
		t.Fatalf("unset: %d %s %s", code, out, stderr)
	}
	out, _, code = s.run(readPolicyScript, "")
	if code != 0 || strings.TrimSpace(out) != "absent" {
		t.Fatalf("after unset: %d %s", code, out)
	}
}

// A policy this user can see and not replace is a refusal, not a copy it never reads.
func TestPolicyWriteRefusesWhatItCannotReplace(t *testing.T) {
	s := newPolicySandbox(t, false)
	if err := os.Mkdir(s.etc, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(s.etc, "backup-policy.json")
	if err := os.WriteFile(f, []byte(`{"v":"1","recipient":"OLD"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.etc, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := s.run(writeFleetPolicyScript, `{"v":"1"}`); code != 3 || !strings.Contains(stderr, "cannot replace") {
		t.Fatalf("write: %d %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(s.home, ".config/teploy/backup-policy.json")); err == nil {
		t.Fatal("a policy nothing reads was written")
	}
	if out, stderr, code := s.run(unsetFleetPolicyScript, ""); code != 3 || strings.Contains(out, "removed") {
		t.Fatalf("unset said it worked: %d %s %s", code, out, stderr)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal(err)
	}
	// Hidden, no sudo, no policy of its own: unset cannot say there is none.
	if err := os.Chmod(s.etc, 0); err != nil {
		t.Fatal(err)
	}
	if out, stderr, code := s.run(unsetFleetPolicyScript, ""); code != 3 || strings.Contains(out, "removed") {
		t.Fatalf("hidden unset: %d %s %s", code, out, stderr)
	}
}

// With nothing in /etc to shadow it, a user with no way in keeps its own, and reads it.
func TestPolicyFallbackIsReadBack(t *testing.T) {
	s := newPolicySandbox(t, false)
	if err := os.Mkdir(s.etc, 0); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := s.run(writeFleetPolicyScript, `{"v":"1","recipient":"MINE"}`)
	if code != 0 || !strings.Contains(stderr, "fallback") {
		t.Fatalf("write: %d %s %s", code, out, stderr)
	}
	out, _, code = s.run(readPolicyScript, "")
	if code != 0 || !strings.Contains(out, "MINE") || !strings.Contains(out, s.home) {
		t.Fatalf("read: %d %s", code, out)
	}
	// XDG_CONFIG_HOME no longer sends the write somewhere reads do not look.
	if strings.Contains(writeFleetPolicyScript, "XDG_CONFIG_HOME") {
		t.Fatal("write and read disagree on where ~/.config is")
	}
	out, stderr, code = s.run(unsetFleetPolicyScript, "")
	if code != 0 || !strings.Contains(out, "removed "+s.home) {
		t.Fatalf("unset: %d %s %s", code, out, stderr)
	}
}

// Stored and not in force is an error, whatever the scripts think they did.
func TestWriteFleetPolicyReadsItselfBack(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "e=/etc/teploy/backup-policy.json", Output: "written /home/u/.config/teploy/backup-policy.json\n"},
		ssh.MockCommand{Match: policyReadMarker, Output: "path /etc/teploy/backup-policy.json\n" + `{"v":"1"}`},
	)
	if _, _, err := WriteFleetPolicy(context.Background(), mock, FleetPolicy{}); err == nil || !strings.Contains(err.Error(), "not in force") {
		t.Fatalf("err = %v", err)
	}
}

// Cron runs as the deploy user, and the policy it read through sudo sits in a
// directory it cannot open.
func TestRunLockPathWorksForAPolicyBehindSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("modes do not stop root")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(t.TempDir(), "etc")
	if err := os.Mkdir(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	p := RunLockPath(filepath.Join(dir, "backup-policy.json"))
	if want := filepath.Join(home, ".config/teploy/backup-run.lock"); p != want {
		t.Fatalf("%s, want %s", p, want)
	}
	l, _, err := AcquireRunLock(p, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	// A writable directory keeps the lock beside the policy.
	own := t.TempDir()
	if p := RunLockPath(filepath.Join(own, "backup-policy.json")); p != filepath.Join(own, "backup-run.lock") {
		t.Fatal(p)
	}
}

func TestPolicyRemedyIsNotChmod(t *testing.T) {
	if strings.Contains(readPolicyScript, "711") {
		t.Fatal("chmod 711 leaves a 0600 file unreadable: it is no remedy")
	}
	// A 711 directory and a sudoer: the file is reachable and unreadable, so it is
	// read through sudo like a hidden one.
	s := newPolicySandbox(t, true)
	if err := os.Mkdir(s.etc, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(s.etc, "backup-policy.json")
	if err := os.WriteFile(f, []byte(`{"v":"1","recipient":"X"}`), 0o200); err != nil {
		t.Fatal(err)
	}
	// The stand-in runs as this user, so it opens the file too.
	sudo := "#!/bin/sh\nshift\nchmod 600 " + f + "\n\"$@\"\nrc=$?\nchmod 200 " + f + "\nexit $rc\n"
	if err := os.WriteFile(filepath.Join(s.bin, "sudo"), []byte(sudo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.etc, 0o711); err != nil {
		t.Fatal(err)
	}
	if out, stderr, code := s.run(readPolicyScript, ""); code != 0 || !strings.Contains(out, `"recipient":"X"`) {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
}

// ---- owner markers ----

func TestClearingARecentMarkerNeedsForce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.Local)
	stamp := func(t time.Time) string { return t.Format("2006-01-02 15:04:05") }
	mk := func(newest time.Time) *ssh.MockExecutor {
		return newStoreMock("h",
			ssh.MockCommand{Match: "aws s3 ls 's3://b/app/'", Output: stamp(newest) + " 1000 app/volumes/20260101-000000.tar.gz\n" + stamp(now.AddDate(0, -3, 0)) + " 20 app/.owners/aaaaaaaaaaaaaaaa\n"},
			ssh.MockCommand{Match: "aws s3 ls", Output: stamp(now.AddDate(0, -3, 0)) + " 20 app/.owners/aaaaaaaaaaaaaaaa\n"},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
	}
	s3 := S3Config{Bucket: "b", Region: "r"}
	mock := mk(now.Add(-time.Hour))
	removed, err := NewClient(mock, io.Discard).ClearNamespaceMarkers(ctx, s3, "aaaaaaaaaaaaaaaa", false, now)
	if !errors.Is(err, ErrMarkerRecent) || len(removed) != 0 || len(rmKeys(mock)) != 0 {
		t.Fatalf("%v %v %v", err, removed, rmKeys(mock))
	}
	mock = mk(now.Add(-time.Hour))
	if removed, err = NewClient(mock, io.Discard).ClearNamespaceMarkers(ctx, s3, "aaaaaaaaaaaaaaaa", true, now); err != nil || len(removed) != 1 {
		t.Fatalf("--force: %v %v", err, removed)
	}
	mock = mk(now.AddDate(0, 0, -30))
	if removed, err = NewClient(mock, io.Discard).ClearNamespaceMarkers(ctx, s3, "aaaaaaaaaaaaaaaa", false, now); err != nil || len(removed) != 1 {
		t.Fatalf("a dead server: %v %v", err, removed)
	}
	// Cannot tell: not a yes.
	mock = newStoreMock("h",
		ssh.MockCommand{Match: "aws s3 ls 's3://b/app/'", Err: errors.New("exit status 255: Forbidden")},
		ssh.MockCommand{Match: "aws s3 ls", Output: stamp(now.AddDate(0, -3, 0)) + " 20 app/.owners/aaaaaaaaaaaaaaaa\n"},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	if _, err = NewClient(mock, io.Discard).ClearNamespaceMarkers(ctx, s3, "aaaaaaaaaaaaaaaa", false, now); !errors.Is(err, ErrMarkerRecent) || len(rmKeys(mock)) != 0 {
		t.Fatalf("%v %v", err, rmKeys(mock))
	}
}

// ---- the prune's brakes ----

func pruneListing(n int, keep string) string {
	var lines []string
	base := time.Now().UTC().AddDate(0, 0, -1)
	for i := 0; i < n; i++ {
		lines = append(lines, "d t 1000 "+keep+base.AddDate(0, 0, -i).Format("20060102-150405")+".tar.gz")
	}
	return strings.Join(lines, "\n")
}

// A store on the same host shares a wrong clock, so the skew check passes and 19 of
// 20 are selected. Tightening retention on purpose takes one explicit run.
func TestPruneBrake(t *testing.T) {
	ctx := context.Background()
	s3 := S3Config{Bucket: "b", Region: "r"}
	mock := treeMock(pruneListing(20, "a/volumes/"))
	_, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: 1})
	if !errors.Is(err, ErrPruneBrake) || !strings.Contains(err.Error(), "19 of the 20") || len(rmKeys(mock)) != 0 {
		t.Fatalf("%v, removed %v", err, rmKeys(mock))
	}
	mock = treeMock(pruneListing(20, "a/volumes/"))
	if del, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil || len(del) != 19 {
		t.Fatalf("%v %v", err, del)
	}
	// One database's series brakes too.
	mock = treeMock(pruneListing(1, "a/accessories/db/") + "\n" + pruneListing(10, "a/accessories/db/databases/x/"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "accessories/db", s3, RetentionPolicy{KeepLast: 2}); !errors.Is(err, ErrPruneBrake) || !strings.Contains(err.Error(), "database x") {
		t.Fatalf("%v", err)
	}
	// Ordinary retention does not: one over the line, and small series.
	for _, tc := range []struct{ n, keep int }{{8, 7}, {3, 1}, {2, 1}, {6, 3}} {
		mock = treeMock(pruneListing(tc.n, "a/volumes/"))
		if del, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: tc.keep}); err != nil || len(del) != tc.n-tc.keep {
			t.Fatalf("%+v: %v %v", tc, err, del)
		}
	}
	// Empty-output failures going is not most of anything.
	listing := pruneListing(1, "a/accessories/db/")
	for i := 0; i < 6; i++ {
		listing += fmt.Sprintf("\nd t 10 a/accessories/db/2024010%d-000000.pgdump", i+1)
	}
	mock = treeMock(strings.ReplaceAll(listing, ".tar.gz", ".pgdump"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "accessories/db", s3, RetentionPolicy{KeepLast: 1}); errors.Is(err, ErrPruneBrake) {
		t.Fatal(err)
	}
}

// A clock that cannot be read is a clock that cannot be trusted.
func TestPruneRefusesAnUnreadableStoreClock(t *testing.T) {
	ctx := context.Background()
	s3 := S3Config{Bucket: "b", Region: "r"}
	r := "a/volumes/"
	mk := func(head ssh.MockCommand) *ssh.MockExecutor {
		return ssh.NewMockExecutor("h",
			ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
			ssh.MockCommand{Match: "printf", Output: ""},
			ssh.MockCommand{Match: "for p in /etc/teploy/age-key", Output: ""},
			head,
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			ssh.MockCommand{Match: "aws s3 ls", Output: "d t 1000 " + r + "20240103-000000.tar.gz\nd t 1000 " + r + "20240102-000000.tar.gz\n"},
			ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		)
	}
	for _, head := range []ssh.MockCommand{
		{Match: "aws s3api head-object", Err: errors.New("exit status 254: Forbidden")},
		{Match: "aws s3api head-object", Output: `{"ContentLength":6}`},
	} {
		mock := mk(head)
		_, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: 1})
		if !errors.Is(err, ErrClockSkew) || !strings.Contains(err.Error(), "could not read") || len(rmKeys(mock)) != 0 {
			t.Fatalf("%v %v", err, rmKeys(mock))
		}
		mock = mk(head)
		if del, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: 1, IgnoreClock: true}); err != nil || len(del) != 1 {
			t.Fatalf("--ignore-clock: %v %v", err, del)
		}
	}
}

// A future-dated object does not stop retention, is never deleted, and does not take
// the real newest backup's place: keep_last 1 with --ignore-clock deleted that.
func TestFutureDatedObjectsAreOutOfSelection(t *testing.T) {
	ctx := context.Background()
	s3 := S3Config{Bucket: "b", Region: "r"}
	r := "a/volumes/"
	future := time.Now().UTC().AddDate(1, 0, 0).Format("20060102-150405")
	today := time.Now().UTC().Format("20060102-150405")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("20060102-150405")
	listing := "d t 1000 " + r + future + ".tar.gz\nd t 1000 " + r + today + ".tar.gz\nd t 1000 " + r + yesterday + ".tar.gz\n"
	for _, ignore := range []bool{false, true} {
		mock := treeMock(listing)
		del, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, RetentionPolicy{KeepLast: 1, IgnoreClock: ignore})
		if err == nil || !strings.Contains(err.Error(), "dated in the future") {
			t.Fatalf("not reported: %v", err)
		}
		got := strings.Join(rmKeys(mock), ",")
		if len(del) != 1 || !strings.Contains(got, yesterday) || strings.Contains(got, today) || strings.Contains(got, future) {
			t.Fatalf("ignore=%v deleted %v: %s", ignore, del, got)
		}
	}
}

// ---- deadlines and leftovers ----

func TestDeadlinesOverTheRunsAreRefused(t *testing.T) {
	if err := (FleetPolicy{TargetDeadline: "24h"}).Validate(); err == nil || !strings.Contains(err.Error(), "capped") {
		t.Fatalf("%v", err)
	}
	if err := (FleetPolicy{TargetDeadline: "23h"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeadline(24 * time.Hour); err == nil {
		t.Fatal("24h")
	}
	if err := ValidateDeadline(0); err != nil {
		t.Fatal(err)
	}
}

// The janitor goes by the workspace's own expiry, so a 20h manual run is safe from a
// run whose own deadline is 6h, in the shell that actually decides.
func TestJanitorRespectsAWorkspacesExpiry(t *testing.T) {
	base := t.TempDir()
	old := time.Now().Add(-10 * time.Hour)
	mk := func(name, expires string) string {
		d := filepath.Join(base, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if expires != "" {
			if err := os.WriteFile(filepath.Join(d, workspaceExpiry), []byte(expires+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(d, old, old); err != nil {
			t.Fatal(err)
		}
		return d
	}
	live := mk("teploy-backup.live", fmt.Sprint(time.Now().Add(10*time.Hour).Unix()))
	expired := mk("teploy-backup.expired", fmt.Sprint(time.Now().Add(-time.Minute).Unix()))
	unmarked := mk("teploy-backup.unmarked", "")
	fresh := filepath.Join(base, "teploy-backup.fresh")
	if err := os.Mkdir(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", cleanWorkspacesScript(ssh.ShellQuote(base)+"/teploy-backup.*"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	exists := func(d string) bool { _, err := os.Stat(d); return err == nil }
	if !exists(live) || !exists(fresh) || exists(expired) || exists(unmarked) {
		t.Fatalf("live %v fresh %v expired %v unmarked %v: %s", exists(live), exists(fresh), exists(expired), exists(unmarked), out)
	}
}

func TestWorkspaceCarriesItsDeadline(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "umask 077; mktemp -d", Output: "/var/tmp/teploy-backup.t\n"}, ssh.MockCommand{Match: "printf", Output: ""})
	c := NewClient(mock, io.Discard)
	c.SetDeadline(20 * time.Hour)
	if _, _, err := c.newBackupWorkspace(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("+ %d ))\" > '/var/tmp/teploy-backup.t/%s'", 21*3600, workspaceExpiry)
	if !strings.Contains(strings.Join(mock.Calls, "\n"), want) {
		t.Fatalf("%v, want %s", mock.Calls, want)
	}
}

// Thirty deploys in a week must not hold thirty full copies past retention.
func TestSafetyCopiesAreCapped(t *testing.T) {
	r := "a/accessories/db/"
	now := time.Now().UTC()
	var lines []string
	ids := make([]string, 0, safetyKeep+3)
	for i := 0; i < safetyKeep+3; i++ {
		id := now.Add(-time.Duration(i+1) * time.Hour).Format("20060102-150405")
		ids = append(ids, id)
		lines = append(lines, "d t 1000 "+r+id+".pgdump", "d t 8 "+r+"safety/"+id+".safety")
	}
	mock := treeMock(strings.Join(lines, "\n"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1, AllowMassDelete: true}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rmKeys(mock), ",")
	for i, id := range ids {
		pruned := strings.Contains(got, id+".pgdump")
		if want := i >= safetyKeep; pruned != want {
			t.Fatalf("copy %d (%s): pruned %v: %s", i, id, pruned, got)
		}
		if want := i >= safetyKeep; strings.Contains(got, "safety/"+id) != want {
			t.Fatalf("marker %d: %s", i, got)
		}
	}
}

// ---- previous prefixes ----

func TestAnUnreachablePreviousPrefixOnlyWarns(t *testing.T) {
	ctx := context.Background()
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "now", PreviousPrefixes: []PreviousPrefix{{Prefix: "gone"}}}
	mk := func(cur ssh.MockCommand) *ssh.MockExecutor {
		return newStoreMock("h",
			ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
			cur,
			ssh.MockCommand{Match: "aws s3 ls 's3://b/gone/a/'", Err: errors.New("exit status 255: AccessDenied")},
		)
	}
	var out strings.Builder
	rows, err := NewClient(mk(ssh.MockCommand{Match: "aws s3 ls 's3://b/now/a/'", Output: "2026-01-01 00:00:00 1000 now/a/volumes/20260102-000000.tar.gz\n"}), &out).ListBackupsRecursive(ctx, "a", s3)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", err, rows)
	}
	if !strings.Contains(out.String(), "previous prefix gone") {
		t.Fatalf("no warning: %s", out.String())
	}
	if _, err := NewClient(mk(ssh.MockCommand{Match: "aws s3 ls 's3://b/now/a/'", Err: errors.New("exit status 255: AccessDenied")}), io.Discard).ListBackupsRecursive(ctx, "a", s3); err == nil {
		t.Fatal("the current location failing must fail")
	}
}

// ---- smaller ones ----

func TestChecksumHeadRefusalFallsBackToSize(t *testing.T) {
	ctx := context.Background()
	big := func(head ssh.MockCommand) *Client {
		mock := putMock(head)
		c := NewClient(mock, &strings.Builder{})
		c.readBack = 1
		return c
	}
	match := "aws s3api head-object --bucket 'b' --key '.incoming/k/1.pgdump' --checksum-mode ENABLED"
	var out strings.Builder
	c := big(ssh.MockCommand{Match: match, Err: errors.New("exit status 2: Unknown options: --checksum-mode")})
	c.out = &out
	if _, err := c.put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified by size alone") {
		t.Fatalf("out: %s", out.String())
	}
	// Anything else is still no proof.
	c = big(ssh.MockCommand{Match: match, Err: errors.New("exit status 254: Forbidden")})
	if _, err := c.put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err == nil || !strings.Contains(err.Error(), "verifying the upload") {
		t.Fatalf("err = %v", err)
	}
}

func TestExcludeNameMatchesWhatExcludeTakes(t *testing.T) {
	for _, tc := range []struct {
		t    FleetTarget
		want string
	}{
		{FleetTarget{Kind: TargetAccessory, App: "a", Name: "db", Container: "a-db"}, "a/db"},
		{FleetTarget{Kind: TargetVolumes, App: "a"}, "a/volumes"},
		{FleetTarget{Kind: TargetAdopted, App: AdoptedApp, Name: "m", Container: "m"}, "m"},
	} {
		got := excludeName(tc.t)
		if got != tc.want {
			t.Fatalf("%+v: %s", tc.t, got)
		}
		if err := validateExclude(got); err != nil {
			t.Fatal(err)
		}
		p := FleetPolicy{Excludes: []string{got}}
		if !p.Excluded(tc.t.App, map[string]string{TargetVolumes: "volumes"}[tc.t.Kind]+tc.t.Name, tc.t.Container) {
			t.Fatalf("%s excludes nothing", got)
		}
	}
}

func TestAWSCLIMinimumVersion(t *testing.T) {
	for out, bad := range map[string]bool{
		"aws-cli/2.31.8 Python/3.13.7 Linux/6.8 exe/x86_64.ubuntu.24": false,
		"aws-cli/2.23.0 Python/3.12 Linux":                            false,
		"aws-cli/2.22.35 Python/3.12 Linux":                           true,
		"aws-cli/2.9.19 Python/3.11 Linux":                            true,
		"aws-cli/1.37.0 Python/3.12 Linux botocore/1.36.0":            false,
		"aws-cli/1.22.34 Python/3.10 Linux botocore/1.23.34":          true,
		"aws-cli/3.0.0 Python/3.14 Linux":                             false,
		"something else entirely":                                     false,
		"":                                                            false,
	} {
		if got := AWSCLIVersionProblem(out) != ""; got != bad {
			t.Fatalf("%q: %v", out, got)
		}
	}
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "which aws", Output: "aws-cli/1.22.34 Python/3.10\n"})
	err := NewClient(mock, io.Discard).ensureAWSCLI(context.Background())
	if err == nil || !strings.Contains(err.Error(), "older than") || !strings.Contains(err.Error(), "teploy setup") {
		t.Fatalf("err = %v", err)
	}
}

// The install script's shell check is the Go one: an old distribution package is not
// "installed".
func TestInstallScriptShellCheckAgrees(t *testing.T) {
	if !strings.Contains(AWSCLIInstallScript, fmt.Sprintf(`2) [ "$b" -ge %d ]`, awsCLIMinV2Minor)) || !strings.Contains(AWSCLIInstallScript, fmt.Sprintf(`1) [ "$b" -ge %d ]`, awsCLIMinV1Minor)) {
		t.Fatal("the script's numbers drifted")
	}
	bin := t.TempDir()
	for version, ok := range map[string]bool{"2.23.0": true, "2.31.8": true, "2.22.35": false, "1.37.0": true, "1.22.34": false, "3.1.0": true} {
		script := "#!/bin/sh\necho aws-cli/" + version + " Python/3.12 Linux\n"
		if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", awsCLIShellOK+"\naws_ok")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		if got := cmd.Run() == nil; got != ok {
			t.Fatalf("%s: %v", version, got)
		}
		if got := AWSCLIVersionProblem("aws-cli/"+version+" Python") == ""; got != ok {
			t.Fatalf("go disagrees on %s", version)
		}
	}
}

// The parent goes first, on a list taken before: a parent still alive would answer a
// dead child by starting its next step.
func TestStuckHolderIsKilledParentFirst(t *testing.T) {
	var order []int
	kill := func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			t.Fatalf("signal %v", sig)
		}
		order = append(order, pid)
		return nil
	}
	if err := killHolder(100, []int{101, 102}, kill); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(order) != "[100 -101 -102]" {
		t.Fatalf("%v", order)
	}
	// A parent that cannot be stopped keeps its children: nothing else was asked for.
	order = nil
	if err := killHolder(100, []int{101}, func(pid int, _ syscall.Signal) error {
		order = append(order, pid)
		return errors.New("EPERM")
	}); err == nil || fmt.Sprint(order) != "[100]" {
		t.Fatalf("%v %v", err, order)
	}
}
