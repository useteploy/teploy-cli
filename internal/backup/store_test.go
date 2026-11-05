package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
)

func entriesAt(stamps ...string) []backupEntry {
	out := make([]backupEntry, 0, len(stamps))
	for _, s := range stamps {
		e, ok := newBackupEntry(s + ".pgdump")
		if !ok {
			panic(s)
		}
		out = append(out, e)
	}
	return out
}

func kept(entries []backupEntry, p RetentionPolicy, now time.Time) []string {
	del := map[string]bool{}
	for _, d := range selectForDeletion(append([]backupEntry{}, entries...), p, now) {
		del[d] = true
	}
	var out []string
	for _, e := range entries {
		if !del[e.name] {
			out = append(out, strings.TrimSuffix(e.name, ".pgdump"))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// Restic forget's documented example shape: rules are a union, each rule takes
// the newest backup of each period that has one, a period with no backup does
// not use up the count, and one backup can satisfy several rules.
func TestGFSExactSets(t *testing.T) {
	now := mustTime(t, "20260701-120000")
	entries := entriesAt(
		"20260701-030000", "20260701-010000", // two on the newest day
		"20260630-030000",
		"20260628-030000", // Sunday: end of ISO week 26
		"20260622-030000", // Monday of week 26
		"20260621-030000", // Sunday: end of week 25
		"20260614-030000",
		"20260531-030000",
		"20260430-030000",
		"20260401-030000",
		"20250630-030000",
	)
	cases := []struct {
		name string
		p    RetentionPolicy
		want []string
	}{
		{"keep last 2", RetentionPolicy{KeepLast: 2}, []string{"20260701-030000", "20260701-010000"}},
		{"daily 3 skips the second backup of a day and an empty day", RetentionPolicy{KeepDaily: 3},
			[]string{"20260701-030000", "20260630-030000", "20260628-030000"}},
		{"weekly 3: newest of each ISO week", RetentionPolicy{KeepWeekly: 3},
			[]string{"20260701-030000", "20260628-030000", "20260621-030000"}},
		{"monthly 3: newest of each month, gaps do not count", RetentionPolicy{KeepMonthly: 3},
			[]string{"20260701-030000", "20260630-030000", "20260531-030000"}},
		{"union, one backup satisfies several rules", RetentionPolicy{KeepLast: 1, KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 2},
			[]string{"20260701-030000", "20260630-030000", "20260628-030000"}},
		{"more periods than exist keeps each period's newest", RetentionPolicy{KeepMonthly: 100},
			[]string{"20260701-030000", "20260630-030000", "20260531-030000", "20260430-030000", "20250630-030000"}},
		{"max age keeps everything younger", RetentionPolicy{MaxAgeDays: 2},
			[]string{"20260701-030000", "20260701-010000", "20260630-030000"}},
		{"deploy-ovh: 48h, 7 daily, 4 weekly, 12 monthly", RetentionPolicy{MaxAgeDays: 2, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12},
			[]string{"20260701-030000", "20260701-010000", "20260630-030000", "20260628-030000", "20260622-030000", "20260621-030000", "20260614-030000", "20260531-030000", "20260430-030000", "20250630-030000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := kept(entries, tc.p, now)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("kept\n %v\nwant\n %v", got, tc.want)
			}
		})
	}
}

// ISO weeks cross years: 2026-01-01 belongs to 2026 W01, 2025-12-29 too.
func TestGFSWeekAcrossYear(t *testing.T) {
	entries := entriesAt("20260101-000000", "20251229-000000", "20251228-000000")
	got := kept(entries, RetentionPolicy{KeepWeekly: 2}, mustTime(t, "20260102-000000"))
	if strings.Join(got, ",") != "20260101-000000,20251228-000000" {
		t.Fatalf("kept %v", got)
	}
}

func TestPruneNeverDeletesNewestGood(t *testing.T) {
	now := mustTime(t, "20300101-000000")
	entries := entriesAt("20200103-000000", "20200102-000000", "20200101-000000")
	if got := kept(entries, RetentionPolicy{MaxAgeDays: 1}, now); strings.Join(got, ",") != "20200103-000000" {
		t.Fatalf("everything past max age must still leave the newest: %v", got)
	}
}

// A below-floor object is not a good copy: it neither counts toward keep_last
// nor shields an older good one, and an all-bad series deletes nothing.
func TestPruneBadObjectsAreNotGoodCopies(t *testing.T) {
	now := mustTime(t, "20300101-000000")
	bad := func(s string) backupEntry {
		e, _ := newBackupEntry(s + ".pgdump")
		e.bad = true
		return e
	}
	entries := append(entriesAt("20200101-000000"), bad("20200102-000000"), bad("20200103-000000"))
	del := selectForDeletion(append([]backupEntry{}, entries...), RetentionPolicy{KeepLast: 1}, now)
	if len(del) != 0 {
		t.Fatalf("a bad object newer than the good one is left alone, the good one kept: %v", del)
	}
	entries = append(entriesAt("20200105-000000", "20200101-000000"), bad("20200102-000000"))
	del = selectForDeletion(append([]backupEntry{}, entries...), RetentionPolicy{KeepLast: 1}, now)
	sort.Strings(del)
	if strings.Join(del, ",") != "20200101-000000.pgdump,20200102-000000.pgdump" {
		t.Fatalf("del = %v", del)
	}
	if del := selectForDeletion([]backupEntry{bad("20200102-000000")}, RetentionPolicy{KeepLast: 1}, now); len(del) != 0 {
		t.Fatalf("with no good copy nothing goes: %v", del)
	}
}

func treeMock(listing string) *ssh.MockExecutor {
	return newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: listing},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
		ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
	)
}

func rmKeys(mock *ssh.MockExecutor) []string {
	var out []string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "aws s3 rm ") {
			f := strings.Fields(c)
			out = append(out, strings.Trim(f[3], "'"))
		}
	}
	sort.Strings(out)
	return out
}

// A pruned backup takes globals, meta, per-database objects and their meta; the
// kept backup keeps all of theirs; unknown objects are never touched.
func TestPruneTakesEverySidecarAndNothingElse(t *testing.T) {
	r := "a/accessories/db/"
	listing := strings.Join([]string{
		"d t 1000 " + r + "20240101-000000.pgdump.age",
		"d t 1000 " + r + "20240102-000000.pgdump.age",
		"d t 500 " + r + "globals/20240101-000000.sql.gz.age",
		"d t 500 " + r + "globals/20240102-000000.sql.gz.age",
		"d t 500 " + r + "meta/20240101-000000.json",
		"d t 500 " + r + "meta/20240102-000000.json.age",
		"d t 1000 " + r + "databases/x/20240101-000000.pgdump",
		"d t 1000 " + r + "databases/x/20240102-000000.pgdump.age",
		"d t 500 " + r + "databases/x/meta/20240101-000000.json.age",
		"d t 1000 " + r + "not-a-backup.txt",
		"d t 1000 " + r + "weird/20200101-000000.pgdump",
	}, "\n")
	mock := treeMock(listing)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"s3://b/" + r + "20240101-000000.pgdump.age",
		"s3://b/" + r + "databases/x/20240101-000000.pgdump",
		"s3://b/" + r + "databases/x/meta/20240101-000000.json.age",
		"s3://b/" + r + "globals/20240101-000000.sql.gz.age",
		"s3://b/" + r + "meta/20240101-000000.json",
	}
	sort.Strings(want)
	if got := rmKeys(mock); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("removed\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// With no primary objects (an adopted container), each database is its own
// series; globals stay as long as any database still needs that id.
func TestPruneAdoptedSeriesIndependently(t *testing.T) {
	r := "_adopted/accessories/m/"
	listing := strings.Join([]string{
		"d t 1000 " + r + "databases/a/20240101-000000.sql.gz",
		"d t 1000 " + r + "databases/a/20240102-000000.sql.gz",
		"d t 1000 " + r + "databases/b/20240101-000000.sql.gz", // b's dump has been failing: its only copy
		"d t 500 " + r + "globals/20240101-000000.sql.gz",
	}, "\n")
	mock := treeMock(listing)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), AdoptedApp, "accessories/m", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	got := rmKeys(mock)
	if strings.Join(got, ",") != "s3://b/"+r+"databases/a/20240101-000000.sql.gz" {
		t.Fatalf("removed %v", got)
	}
}

// A database whose dump keeps failing keeps its newest good object even while
// the primary series moves on.
func TestPruneKeepsADatabasesLastGoodCopy(t *testing.T) {
	r := "a/accessories/db/"
	listing := strings.Join([]string{
		"d t 1000 " + r + "20240103-000000.pgdump",
		"d t 1000 " + r + "20240102-000000.pgdump",
		"d t 1000 " + r + "20240101-000000.pgdump",
		"d t 1000 " + r + "databases/x/20240101-000000.pgdump",
		"d t 1000 " + r + "databases/y/20240101-000000.pgdump",
		"d t 1000 " + r + "databases/y/20240103-000000.pgdump",
		"d t 1000 " + r + "databases/y/20240104-000000.pgdump", // a killed run's: newer than any primary
	}, "\n")
	mock := treeMock(listing)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 2}); err != nil {
		t.Fatal(err)
	}
	want := []string{"s3://b/" + r + "20240101-000000.pgdump", "s3://b/" + r + "databases/y/20240101-000000.pgdump"}
	if got := rmKeys(mock); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("removed %v want %v", got, want)
	}
}

func TestPruneRefusesASharedNamespace(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/.owners/'", Output: "d t 20 0123456789abcdef\nd t 20 aaaaaaaaaaaaaaaa\n"},
		ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
	)
	_, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1})
	if !errors.Is(err, ErrSharedNamespace) {
		t.Fatalf("err = %v", err)
	}
	if len(rmKeys(mock)) != 0 {
		t.Fatal("nothing may be deleted")
	}
}

// A refused delete (bucket locks) does not stop the rest, and is reported.
func TestPruneToleratesRefusedDeletes(t *testing.T) {
	r := "a/volumes/"
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: "d t 1000 " + r + "20240103-000000.tar.gz\nd t 1000 " + r + "20240102-000000.tar.gz\nd t 1000 " + r + "20240101-000000.tar.gz\n"},
		ssh.MockCommand{Match: "aws s3 rm 's3://b/" + r + "20240102-000000.tar.gz'", Err: errors.New("exit status 1: AccessDenied: object is locked")},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	deleted, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1})
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("err = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "20240101-000000.tar.gz" {
		t.Fatalf("deleted = %v", deleted)
	}
}

// ---- put / fetch ----

func putMock(extra ...ssh.MockCommand) *ssh.MockExecutor {
	base := []ssh.MockCommand{
		{Match: "sha256sum", Output: "abc  f\n4096\n"},
		{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
		{Match: "{ ", Output: "abc  -\n"},
		{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"}}`},
		{Match: "aws s3 cp", Output: "ok\n"},
		{Match: "aws s3 rm", Output: ""},
		{Match: "rm -f", Output: ""},
		{Match: "command -v age", Output: "ok\n"},
		{Match: "if [ -r", Output: "yes\n"},
		{Match: "age-keygen -y", Output: testServerRecipient + "\n"},
		{Match: "age -r", Output: ""},
		{Match: "age -d", Output: ""},
	}
	return ssh.NewMockExecutor("h", append(extra, base...)...)
}

// The final key only ever receives a server-side copy of a verified .incoming/
// object, and plaintext never goes up when encrypting.
func TestPutUploadsToIncomingThenPublishes(t *testing.T) {
	mock := putMock()
	s3 := S3Config{Bucket: "b", Region: "r", OwnerRecipient: testOwnerRecipient}
	obj, err := NewClient(mock, io.Discard).put(context.Background(), s3, "/w/x.pgdump", "s3://b/a/accessories/db/1.pgdump")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Key != "s3://b/a/accessories/db/1.pgdump.age" || !obj.Encrypted {
		t.Fatalf("obj = %+v", obj)
	}
	var order []string
	for _, c := range mock.Calls {
		switch {
		case strings.HasPrefix(c, "age -r"):
			if !strings.Contains(c, testOwnerRecipient) || !strings.Contains(c, testServerRecipient) {
				t.Fatalf("must encrypt to owner and server: %s", c)
			}
			order = append(order, "encrypt")
		case strings.HasPrefix(c, "age -d"):
			order = append(order, "prove")
		case strings.HasPrefix(c, "rm -f '/w/x.pgdump'"):
			order = append(order, "rm-plain")
		case strings.HasPrefix(c, "aws s3 cp '/w/x.pgdump.age' 's3://b/a/accessories/db/.incoming/1.pgdump.age' --metadata 'sha256=abc'"):
			order = append(order, "upload")
		case strings.HasPrefix(c, "aws s3 cp '/w/x.pgdump'"):
			t.Fatalf("plaintext uploaded: %s", c)
		case strings.HasPrefix(c, "{ aws s3 cp"):
			order = append(order, "readback")
		case strings.HasPrefix(c, "aws s3 cp 's3://b/a/accessories/db/.incoming/1.pgdump.age' 's3://b/a/accessories/db/1.pgdump.age'"):
			order = append(order, "publish")
		case strings.HasPrefix(c, "aws s3 rm 's3://b/a/accessories/db/.incoming/1.pgdump.age'"):
			order = append(order, "clean")
		}
	}
	if strings.Join(order, ",") != "encrypt,prove,rm-plain,upload,readback,publish,clean" {
		t.Fatalf("order = %v", order)
	}
}

func TestPutRefusesWhatDoesNotVerify(t *testing.T) {
	for _, tc := range []struct {
		name string
		mock ssh.MockCommand
		want string
	}{
		{"short object", ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":100,"Metadata":{"sha256":"abc"}}`}, "100 bytes"},
		{"wrong checksum recorded", ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"zzz"}}`}, "checksum"},
		{"bytes differ", ssh.MockCommand{Match: "{ ", Output: "zzz  -\n"}, "checksum mismatch"},
		{"head refused", ssh.MockCommand{Match: "aws s3api head-object", Err: errors.New("exit status 254: Forbidden")}, "verifying the upload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := putMock(tc.mock)
			_, err := NewClient(mock, io.Discard).put(context.Background(), S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, c := range mock.Calls {
				if strings.HasPrefix(c, "aws s3 cp 's3://b/k/.incoming/1.pgdump' 's3://b/k/1.pgdump'") {
					t.Fatalf("an unverified object was published: %s", c)
				}
			}
			removed := false
			for _, c := range mock.Calls {
				removed = removed || strings.HasPrefix(c, "aws s3 rm 's3://b/k/.incoming/1.pgdump'")
			}
			if !removed {
				t.Fatal("the unverified upload was left behind")
			}
		})
	}
}

// Encryption failing never falls back to plaintext.
func TestPutEncryptFailureUploadsNothing(t *testing.T) {
	for _, tc := range []ssh.MockCommand{
		{Match: "age -r", Err: errors.New("exit status 1: no space")},
		{Match: "age -d", Err: errors.New("exit status 1: no identity matched")},
	} {
		mock := putMock(tc)
		_, err := NewClient(mock, io.Discard).put(context.Background(), S3Config{Bucket: "b", Region: "r", OwnerRecipient: testOwnerRecipient}, "/w/x", "s3://b/k/1.pgdump")
		if err == nil {
			t.Fatalf("%s: expected a failure", tc.Match)
		}
		for _, c := range mock.Calls {
			if strings.HasPrefix(c, "aws s3 cp") {
				t.Fatalf("%s: uploaded anyway: %s", tc.Match, c)
			}
		}
	}
}

func TestPrepareEncryptionRefusesEarly(t *testing.T) {
	mock := ssh.NewMockExecutor("h", ssh.MockCommand{Match: "command -v age", Err: errors.New("exit status 1")})
	err := NewClient(mock, io.Discard).prepareEncryption(context.Background(), S3Config{OwnerRecipient: testOwnerRecipient})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
	if err := NewClient(mock, io.Discard).prepareEncryption(context.Background(), S3Config{OwnerRecipient: "not-a-key"}); err == nil {
		t.Fatal("a malformed recipient must be refused")
	}
}

func TestFetchRefusesACorruptObject(t *testing.T) {
	mock := putMock(ssh.MockCommand{Match: "sha256sum", Output: "different  f\n4096\n"})
	err := NewClient(mock, io.Discard).fetch(context.Background(), S3Config{Bucket: "b", Region: "r"}, "s3://b/k/1.pgdump", "/w/x")
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchDecryptsWithServerIdentityAndNamesTheWayOut(t *testing.T) {
	mock := putMock(ssh.MockCommand{Match: "umask 077; age -d", Output: ""})
	if err := NewClient(mock, io.Discard).fetch(context.Background(), S3Config{Bucket: "b", Region: "r"}, "s3://b/k/1.pgdump.age", "/w/x"); err != nil {
		t.Fatal(err)
	}
	dec := false
	for _, c := range mock.Calls {
		dec = dec || strings.HasPrefix(c, "umask 077; age -d $(for p in /etc/teploy/age-key")
	}
	if !dec {
		t.Fatalf("no decrypt: %v", mock.Calls)
	}
	mock = putMock(ssh.MockCommand{Match: "umask 077; age -d", Err: errors.New("exit status 1: no identity matched")})
	err := NewClient(mock, io.Discard).fetch(context.Background(), S3Config{Bucket: "b", Region: "r"}, "s3://b/k/1.pgdump.age", "/w/x")
	if err == nil || !strings.Contains(err.Error(), "backup fetch --identity") {
		t.Fatalf("err = %v", err)
	}
}

func TestFindObjectPrefersCurrentThenPrevious(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "aws s3 ls 's3://b/new/a/volumes/1.'", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/volumes/1.'", Output: "d t 100 1.tar.gz.age\n"},
	)
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "new", PreviousPrefixes: []PreviousPrefix{{Prefix: ""}}}
	key, ext, loc, err := NewClient(mock, io.Discard).findObject(context.Background(), s3, "a/volumes/", "1", ".tar.gz")
	if err != nil || key != "s3://b/a/volumes/1.tar.gz.age" || ext != ".tar.gz" || loc.Prefix != "" {
		t.Fatalf("%q %q %+v %v", key, ext, loc, err)
	}
}

// The recursive listing's keys are relative to the bucket: a prefix must not be
// doubled in the URI nor hide the rows.
func TestListWithPrefixAndPreviousPrefix(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/srv1/a/' --recursive", Output: "2026-01-01 00:00:00 1000 srv1/a/accessories/db/20260102-000000.pgdump.age\n2026-01-01 00:00:00 1000 srv1/a/accessories/db/.incoming/20260103-000000.pgdump.age\n2026-01-01 00:00:00 10 srv1/a/accessories/db/20260104-000000.pgdump\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/' --recursive", Output: "2026-01-01 00:00:00 1000 a/accessories/db/databases/wp%20x/20260101-000000.sql.gz\n"},
	)
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "srv1", PreviousPrefixes: []PreviousPrefix{{Prefix: ""}}}
	rows, err := NewClient(mock, io.Discard).ListBackupsRecursive(context.Background(), "a", s3)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ListedBackup{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID["20260102-000000"]; r.Key != "s3://b/srv1/a/accessories/db/20260102-000000.pgdump.age" || !r.Encrypted || r.Previous || !r.Restorable {
		t.Fatalf("current: %+v", r)
	}
	if _, ok := byID["20260103-000000"]; ok {
		t.Fatal("a pending upload listed as a backup")
	}
	if r := byID["20260104-000000"]; r.Restorable {
		t.Fatalf("a below-floor object must not be restorable: %+v", r)
	}
	if r := byID["20260101-000000"]; !r.Previous || r.Database != "wp x" || r.Key != "s3://b/a/accessories/db/databases/wp%20x/20260101-000000.sql.gz" {
		t.Fatalf("previous: %+v", r)
	}
}

// `aws s3 ls` exits 1 with no output on an empty prefix: that is an empty list,
// and any other failure is not.
func TestListTreeEmptyIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "aws"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	exec := ssh.NewLocalExecutor()
	write("exit 1")
	keys, err := NewClient(exec, io.Discard).listTree(context.Background(), S3Config{Bucket: "b", Region: "r"}, "a/")
	if err != nil || len(keys) != 0 {
		t.Fatalf("%v %v", keys, err)
	}
	write("echo 'An error occurred (AccessDenied)' >&2; exit 1")
	if _, err := NewClient(exec, io.Discard).listTree(context.Background(), S3Config{Bucket: "b", Region: "r"}, "a/"); err == nil {
		t.Fatal("a refused listing must not read as empty")
	}
	write("exit 254")
	if _, err := NewClient(exec, io.Discard).listTree(context.Background(), S3Config{Bucket: "b", Region: "r"}, "a/"); err == nil {
		t.Fatal("another exit status must not read as empty")
	}
}

func TestLatestSkipsBelowFloorAndSearchesPrevious(t *testing.T) {
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "aws s3 ls 's3://b/new/a/accessories/db/'", Output: "2026-01-01 00:00:00 20 20260105-000000.pgdump\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/accessories/db/'", Output: "2026-01-01 00:00:00 1000 20260101-000000.pgdump\n"},
	)
	s3 := S3Config{Bucket: "b", Region: "r", Prefix: "new", PreviousPrefixes: []PreviousPrefix{{Prefix: ""}}}
	id, _, _, loc, err := NewClient(mock, io.Discard).latestBackup(context.Background(), "a", "db", "", s3)
	if err != nil || id != "20260101-000000" || loc.Prefix != "" {
		t.Fatalf("%q %+v %v", id, loc, err)
	}
}

// ---- addressing ----

func TestDBSegmentIsOneSafeKeySegment(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"wp_blog", "wp_blog"},
		{"a b", "a%20b"},
		{"a/b", "a%2Fb"},
		{"..", "%2E."},
		{"x'; rm -rf /; '", "x%27%3B%20rm%20-rf%20%2F%3B%20%27"},
		{"$(touch x)", "%24%28touch%20x%29"},
	} {
		got := dbSegment(tc.in)
		if got != tc.want {
			t.Errorf("dbSegment(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if back := decodeDBSegment(got); back != tc.in {
			t.Errorf("decode(%q) = %q, want %q", got, back, tc.in)
		}
	}
}

func TestAddressableDB(t *testing.T) {
	for _, ok := range []string{"wp_blog", "a b", "x'; touch y", "$(touch y)", "`z`", "日本語"} {
		if !addressableDB(ok) {
			t.Errorf("%q should be addressable once quoted", ok)
		}
	}
	for _, bad := range []string{"", "-rf", "--all-databases", "a=b", "a\nb", strings.Repeat("x", 65)} {
		if addressableDB(bad) {
			t.Errorf("%q must not be addressable", bad)
		}
	}
	if mysqlDumpableDB("x'y") || mysqlDumpableDB(`x\y`) || !mysqlDumpableDB("a b") {
		t.Error("mysqlDumpableDB wrong")
	}
}

func TestContainerName(t *testing.T) {
	if ContainerName("app", "db") != "app-db" || ContainerName(AdoptedApp, "omni_postgres") != "omni_postgres" {
		t.Fatal("wrong container names")
	}
}

// Hostile adopted names and schemas reach the shell only inside single quotes;
// non-addressable schemas are reported, never run.
func TestAdoptedMySQLHostileSchemasAreQuotedAndKeyed(t *testing.T) {
	hostile := "x y$(touch /tmp/p)"
	unaddressable := "--all-databases"
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "docker exec 'm_y' sh -c 'MYSQL_PWD=${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-${MYSQL_PASSWORD:-}}}; export MYSQL_PWD; exec mysql -u root -N -B -e '\"'\"'SELECT s.schema_name", Output: "wp_a\t0\n" + hostile + "\t0\n" + unaddressable + "\t0\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "df -Pk", Output: "Filesystem 1K-blocks Used Available Use% Mounted\n/dev/x 1 1 99999999 1% /\n"},
		ssh.MockCommand{Match: "aws s3", Output: ""},
		ssh.MockCommand{Match: "printf", Output: ""},
	)
	c := NewClient(mock, io.Discard)
	if err := c.SetEngineOverride("m_y", EngineMySQL); err != nil {
		t.Fatal(err)
	}
	res, err := c.AccessoryBackupWith(context.Background(), AdoptedApp, "m_y", "x", map[string]string{"MYSQL_ROOT_PASSWORD": "pw"}, S3Config{Bucket: "b", Region: "r"}, BackupOptions{AllDatabases: true})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	got := map[string]string{}
	for _, o := range res.Objects {
		got[o.Database] = o.Key
	}
	if got["wp_a"] == "" || got[hostile] == "" {
		t.Fatalf("objects: %+v", res.Objects)
	}
	if !strings.Contains(got[hostile], "/_adopted/accessories/m_y/databases/x%20y%24%28touch%20%2Ftmp%2Fp%29/") {
		t.Fatalf("key: %s", got[hostile])
	}
	if len(res.UncoveredDatabases) != 1 || res.UncoveredDatabases[0] != unaddressable {
		t.Fatalf("uncovered: %v", res.UncoveredDatabases)
	}
	for _, call := range mock.Calls {
		if strings.Contains(call, "mysqldump") && strings.Contains(call, unaddressable) {
			t.Fatalf("an option-shaped name reached the dump: %s", call)
		}
		if strings.Contains(call, "mysqldump") && strings.Contains(call, hostile) && !strings.Contains(call, ssh.ShellQuote(ssh.ShellQuote(hostile))) && !strings.Contains(call, ssh.ShellQuote(hostile)) {
			t.Fatalf("a hostile name is not quoted: %s", call)
		}
	}
}

// One database failing does not stop the next, fails the backup, and is named.
func TestOneDatabaseFailingDoesNotStopTheOthers(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "docker exec 'a-db' pg_dumpall", Output: ""},
		ssh.MockCommand{Match: "docker exec 'a-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'postgres' -c 'SELECT datname", Output: "app\nbad\ngood\n"},
		ssh.MockCommand{Match: "docker exec 'a-db' pg_dump -Fc -U 'postgres' 'bad'", Err: errors.New("exit status 1: pg_dump: error")},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "{ docker", Output: ""},
		ssh.MockCommand{Match: "mkdir -p", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "df -Pk", Output: "Filesystem 1K-blocks Used Available Use% Mounted\n/dev/x 1 1 99999999 1% /\n"},
		ssh.MockCommand{Match: "aws s3", Output: ""},
		ssh.MockCommand{Match: "printf", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).AccessoryBackupWith(context.Background(), "a", "db", "postgres:17", map[string]string{"POSTGRES_DB": "app"}, S3Config{Bucket: "b", Region: "r"}, BackupOptions{AllDatabases: true})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v", err)
	}
	if res.FailedDatabases["bad"] == "" {
		t.Fatalf("failed: %+v", res.FailedDatabases)
	}
	names := []string{}
	for _, o := range res.Objects {
		names = append(names, o.Database)
	}
	if strings.Join(names, ",") != "good," {
		t.Fatalf("objects stored: %q", names)
	}
	// Commit order: the primary object is stored after every sidecar and database.
	lastPut := -1
	for i, c := range mock.Calls {
		if strings.Contains(c, ".incoming/") && strings.Contains(c, "--metadata") {
			lastPut = i
		}
	}
	if !strings.Contains(mock.Calls[lastPut], "a/accessories/db/.incoming/") || strings.Contains(mock.Calls[lastPut], "databases/") {
		t.Fatalf("the primary must be the last upload: %s", mock.Calls[lastPut])
	}
}

// Preflight: measures the backup (plain + ciphertext), refuses before dumping.
func TestPreflightRefusesBeforeDumping(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "docker inspect -f", Output: "{}\n[]\n[]\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "docker exec 'a-db' psql -X -qtA -v ON_ERROR_STOP=1 -U 'postgres' -d 'app' -c 'SELECT pg_database_size", Output: "10737418240\n"},
		ssh.MockCommand{Match: "df -Pk", Output: "Filesystem 1K-blocks Used Available Use% Mounted\n/dev/x 1 1 1048576 1% /\n"},
		ssh.MockCommand{Match: "docker exec", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "aws s3", Output: ""},
	)
	_, err := NewClient(mock, io.Discard).AccessoryBackup(context.Background(), "a", "db", "postgres:17", map[string]string{"POSTGRES_DB": "app"}, S3Config{Bucket: "b", Region: "r"})
	if err == nil || !strings.Contains(err.Error(), "not enough free space") || !strings.Contains(err.Error(), "--ignore-space-check") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range mock.Calls {
		if strings.Contains(c, "pg_dump -Fc") {
			t.Fatalf("dumped anyway: %s", c)
		}
	}
}

// ---- volumes: .env ----

func TestUnencryptedVolumeBackupLeavesEnvOutAndSays(t *testing.T) {
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "umask 077", Output: "/var/tmp/teploy-backup.t\n"},
		ssh.MockCommand{Match: "du -sk", Output: "10\t/x\n"},
		ssh.MockCommand{Match: "df -Pk", Output: "Filesystem 1K-blocks Used Available Use% Mounted\n/dev/x 1 1 99999999 1% /\n"},
		ssh.MockCommand{Match: "if [ -f", Output: "yes\n"},
		ssh.MockCommand{Match: "tar -czf", Output: ""},
		ssh.MockCommand{Match: "wc -c", Output: "4096\n"},
		ssh.MockCommand{Match: "if [ -e", Output: "absent\n"},
		ssh.MockCommand{Match: "aws s3", Output: ""},
		ssh.MockCommand{Match: "rm -rf", Output: ""},
		ssh.MockCommand{Match: "printf", Output: ""},
	)
	res, err := NewClient(mock, io.Discard).BackupVolumes(context.Background(), "a", S3Config{Bucket: "b", Region: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Encrypted || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], ".env was left out") {
		t.Fatalf("res = %+v", res)
	}
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "tar -czf") && strings.Contains(c, ".env") {
			t.Fatalf(".env in an unencrypted archive: %s", c)
		}
	}
}

// ---- DR bundles ----

func TestBundleAgeKeyRefusedWithoutARecipient(t *testing.T) {
	mock := drBaseMock()
	_, err := NewClient(mock, io.Discard).CreateBundle(context.Background(), BundleOptions{App: "myapp", Config: drTestConfig(), IncludeSecrets: true, IncludeAgeKey: true}, DirBundleStore{Root: "/var/drstore"})
	if err == nil || !strings.Contains(err.Error(), "recovery key") {
		t.Fatalf("err = %v", err)
	}
}

// The private key goes from its file straight to ciphertext, to the owner only.
func TestBundleAgeKeyIsWrappedToTheOwnerOnly(t *testing.T) {
	mock := newStoreMock("1.2.3.4", ssh.MockCommand{Match: "umask 077; age -r", Output: ""})
	store := S3BundleStore{S3: S3Config{Bucket: "b", Region: "r", OwnerRecipient: testOwnerRecipient}}
	cmds := []ssh.MockCommand{
		{Match: "aws s3 cp", Output: ""}, {Match: "aws s3 rm", Output: ""}, {Match: "rm -f", Output: ""},
		{Match: "command -v age", Output: "ok\n"}, {Match: "if [ -r", Output: "yes\n"}, {Match: "age-keygen -y", Output: testServerRecipient + "\n"},
		{Match: "age -r", Output: ""}, {Match: "age -d", Output: ""},
	}
	mock = newStoreMock("1.2.3.4", cmds...)
	// Members: encrypted through put; a member already age ciphertext, and the
	// manifest, are not.
	for _, member := range []string{"accessories/db.pgdump", "secrets/K.age", "age-key.age", "manifest.json"} {
		if err := store.Upload(context.Background(), mock, "myapp", "20260101-000000", member, "/w/"+member); err != nil {
			t.Fatalf("%s: %v", member, err)
		}
	}
	joined := strings.Join(mock.Calls, "\n")
	for _, want := range []string{
		"dr/20260101-000000/accessories/db.pgdump.age'",
		"dr/20260101-000000/secrets/K.age'",
		"dr/20260101-000000/age-key.age'",
		"dr/20260101-000000/manifest.json'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no upload to %s:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"age-key.age.age", "secrets/K.age.age", "manifest.json.age"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("double wrapped: %s", unwanted)
		}
	}
}

func TestBundleDownloadFindsEncryptedAndLegacyMembers(t *testing.T) {
	store := S3BundleStore{S3: S3Config{Bucket: "b", Region: "r"}}
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "aws s3 ls", Output: "d t 100 db.pgdump.age\n"},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
		ssh.MockCommand{Match: "umask 077; age -d", Output: ""},
		ssh.MockCommand{Match: "rm -f", Output: ""},
	)
	if err := store.Download(context.Background(), mock, "a", "1", "accessories/db.pgdump", "/w/x"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(mock.Calls, "\n"), "accessories/db.pgdump.age") {
		t.Fatalf("did not fetch the encrypted member: %v", mock.Calls)
	}
	legacy := newStoreMock("h",
		ssh.MockCommand{Match: "aws s3 ls", Err: errors.New("exit status 1")},
		ssh.MockCommand{Match: "aws s3 cp", Output: ""},
	)
	if err := store.Download(context.Background(), legacy, "a", "1", "accessories/db.pgdump", "/w/x"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(legacy.Calls, "\n"), "age -d") {
		t.Fatalf("a plain member must not be decrypted: %v", legacy.Calls)
	}
}

// ---- review regressions ----

// A database failing in the last run of each day has its own history: keep_daily
// holds for it, not only for the primary's ids.
func TestPruneSelectsEachDatabaseSeriesOnItsOwn(t *testing.T) {
	r := "a/accessories/db/"
	var lines []string
	var wantKept []string
	for d := 1; d <= 9; d++ {
		early := "2026010" + string(rune('0'+d)) + "-020000"
		late := "2026010" + string(rune('0'+d)) + "-200000"
		lines = append(lines, "d t 1000 "+r+early+".pgdump", "d t 1000 "+r+late+".pgdump", "d t 1000 "+r+"databases/x/"+early+".pgdump")
		if d >= 7 {
			wantKept = append(wantKept, "s3://b/"+r+"databases/x/"+early+".pgdump")
		}
	}
	mock := treeMock(strings.Join(lines, "\n"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepDaily: 3}); err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{}
	for _, k := range rmKeys(mock) {
		removed[k] = true
	}
	for _, k := range wantKept {
		if removed[k] {
			t.Fatalf("keep_daily 3 must keep %s: %v", k, rmKeys(mock))
		}
	}
	if !removed["s3://b/"+r+"databases/x/20260106-020000.pgdump"] {
		t.Fatalf("the series past its keep_daily was not pruned: %v", rmKeys(mock))
	}
}

// A primary that reappears after a series-only period does not decide the
// databases' history, and one dropped long ago does not freeze it.
func TestPruneDatabaseSeriesIgnoresThePrimarysPeriods(t *testing.T) {
	r := "a/accessories/db/"
	lines := []string{"d t 1000 " + r + "20260201-000000.pgdump"} // reappeared today
	for d := 10; d <= 28; d++ {
		lines = append(lines, "d t 1000 "+r+"databases/x/202601"+strconv.Itoa(d)+"-000000.pgdump")
	}
	mock := treeMock(strings.Join(lines, "\n"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 5}); err != nil {
		t.Fatal(err)
	}
	if got := len(rmKeys(mock)); got != 14 {
		t.Fatalf("19 objects with keep_last 5 must lose 14, lost %d", got)
	}
	// The primary long gone: the series is still selected.
	lines[0] = "d t 1000 " + r + "20200101-000000.pgdump"
	mock = treeMock(strings.Join(lines, "\n"))
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 5}); err != nil {
		t.Fatal(err)
	}
	if got := len(rmKeys(mock)); got != 14 {
		t.Fatalf("an old primary froze the series: lost %d", got)
	}
}

func TestPruneOwnersFailClosed(t *testing.T) {
	ctx := context.Background()
	p := RetentionPolicy{KeepLast: 1}
	s3 := S3Config{Bucket: "b", Region: "r"}
	// A listing that fails is not "no owners".
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/.owners/'", Err: errors.New("exit status 254: SlowDown")},
		ssh.MockCommand{Match: "aws s3 ls", Output: "d t 1000 a/volumes/20240101-000000.tar.gz\nd t 1000 a/volumes/20240102-000000.tar.gz\n"},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, p); !errors.Is(err, ErrSharedNamespace) {
		t.Fatalf("err = %v", err)
	}
	if len(rmKeys(mock)) != 0 {
		t.Fatal("deleted on a failed owners listing")
	}
	// Empty machine-id and hostname hash to one value on every such host.
	mock = ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  -\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: ""},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, p); !errors.Is(err, ErrSharedNamespace) {
		t.Fatalf("an unidentifiable server: %v", err)
	}
	// A clone: same machine-id and hostname, another age identity.
	mock = ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/a/.owners/'", Output: "d t 20 a/.owners/0123456789abcdef\n"},
		ssh.MockCommand{Match: "aws s3 cp 's3://b/a/.owners/0123456789abcdef' -", Output: `{"recipient":"` + testOwnerRecipient + `"}`},
		ssh.MockCommand{Match: "for p in /etc/teploy/age-key", Output: testServerRecipient + "\n"},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", s3, p); !errors.Is(err, ErrSharedNamespace) || !strings.Contains(err.Error(), "--clear") {
		t.Fatalf("a clone: %v", err)
	}
}

// The marker is written before anything is deleted.
func TestPruneMarksBeforeItDeletes(t *testing.T) {
	r := "a/volumes/"
	mock := treeMock("d t 1000 " + r + "20240103-000000.tar.gz\nd t 1000 " + r + "20240102-000000.tar.gz\n")
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	marked, deleted := -1, -1
	for i, c := range mock.Calls {
		if marked < 0 && strings.Contains(c, ".owners/0123456789abcdef") && strings.HasPrefix(c, "printf") {
			marked = i
		}
		if deleted < 0 && strings.HasPrefix(c, "aws s3 rm") {
			deleted = i
		}
	}
	if marked < 0 || deleted < 0 || marked > deleted {
		t.Fatalf("marked at %d, deleted at %d: %v", marked, deleted, mock.Calls)
	}
}

func TestPruneRefusesAWrongClock(t *testing.T) {
	ctx := context.Background()
	r := "a/volumes/"
	listing := "d t 1000 " + r + "20240103-000000.tar.gz\nd t 1000 " + r + "20240102-000000.tar.gz\n"
	// The store says it is a year from now: this machine is slow, or the store fast.
	future := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
	mock := ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "{ cat /etc/machine-id", Output: "0123456789abcdef0123456789abcdef  -\n"},
		ssh.MockCommand{Match: "printf", Output: ""},
		ssh.MockCommand{Match: "for p in /etc/teploy/age-key", Output: ""},
		ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":1,"LastModified":"` + future + `"}`},
		ssh.MockCommand{Match: "which aws", Output: "/usr/bin/aws\n"},
		ssh.MockCommand{Match: "aws s3 ls", Output: listing},
		ssh.MockCommand{Match: "aws s3 rm", Output: ""},
	)
	_, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1})
	if !errors.Is(err, ErrClockSkew) || len(rmKeys(mock)) != 0 {
		t.Fatalf("err = %v, removed %v", err, rmKeys(mock))
	}
	if _, err := NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1, IgnoreClock: true}); err != nil {
		t.Fatalf("--ignore-clock: %v", err)
	}
	// A backup dated in the future would count as the newest: keep_last 1 would
	// then delete today's real one.
	mock = treeMock("d t 1000 " + r + time.Now().UTC().AddDate(1, 0, 0).Format("20060102-150405") + ".tar.gz\nd t 1000 " + r + time.Now().UTC().Format("20060102-150405") + ".tar.gz\n")
	_, err = NewClient(mock, io.Discard).PruneBackups(ctx, "a", "volumes", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1})
	if !errors.Is(err, ErrClockSkew) || len(rmKeys(mock)) != 0 {
		t.Fatalf("future-dated: err = %v, removed %v", err, rmKeys(mock))
	}
}

func TestPrunePendingUploadsAtAnyDepth(t *testing.T) {
	r := "a/accessories/db/"
	listing := strings.Join([]string{
		"d t 1000 " + r + "20240103-000000.pgdump",
		"d t 1000 " + r + "databases/x/.incoming/20240101-000000.pgdump",
		"d t 1000 " + r + "globals/.incoming/20240101-000000.sql.gz",
		"d t 1000 " + r + "meta/.incoming/20240101-000000.json",
		"d t 1000 " + r + ".incoming/20240101-000000.pgdump",
	}, "\n")
	mock := treeMock(listing)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	if got := rmKeys(mock); len(got) != 4 {
		t.Fatalf("removed %v", got)
	}
}

// A destructive operation's safety copy outlasts retention for its window.
func TestPruneProtectsSafetyCopies(t *testing.T) {
	r := "a/accessories/db/"
	recent := time.Now().UTC().Add(-time.Hour).Format("20060102-150405")
	newer := time.Now().UTC().Add(-time.Minute).Format("20060102-150405")
	old := time.Now().UTC().AddDate(0, 0, -30).Format("20060102-150405")
	listing := strings.Join([]string{
		"d t 1000 " + r + newer + ".pgdump",
		"d t 1000 " + r + recent + ".pgdump",
		"d t 8 " + r + "safety/" + recent + ".safety",
		"d t 1000 " + r + old + ".pgdump",
		"d t 8 " + r + "safety/" + old + ".safety",
	}, "\n")
	mock := treeMock(listing)
	if _, err := NewClient(mock, io.Discard).PruneBackups(context.Background(), "a", "accessories/db", S3Config{Bucket: "b", Region: "r"}, RetentionPolicy{KeepLast: 1}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rmKeys(mock), ",")
	if strings.Contains(got, recent) {
		t.Fatalf("a recent safety copy was pruned: %s", got)
	}
	if !strings.Contains(got, old+".pgdump") || !strings.Contains(got, "safety/"+old) {
		t.Fatalf("a safety copy past its window, with its marker, must go: %s", got)
	}
}

func TestPutLargeObjectUsesAProviderChecksum(t *testing.T) {
	ctx := context.Background()
	big := func(extra ...ssh.MockCommand) *ssh.MockExecutor {
		return putMock(append(extra, ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"},"ChecksumSHA256":"AAA=-3"}`})...)
	}
	c := func(m *ssh.MockExecutor) *Client { cl := NewClient(m, io.Discard); cl.readBack = 1; return cl }
	mock := big()
	if _, err := c(mock).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mock.Calls, "\n")
	if !strings.Contains(joined, "--checksum-algorithm SHA256") || strings.Contains(joined, "{ aws s3 cp") {
		t.Fatalf("calls: %s", joined)
	}
	// A whole-object digest that differs refuses.
	mock = putMock(
		ssh.MockCommand{Match: "aws s3api head-object", Output: `{"ContentLength":4096,"Metadata":{"sha256":"abc"},"ChecksumSHA256":"bm9wZQ=="}`},
	)
	if _, err := c(mock).put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	// A store or CLI without it falls back, and says so.
	var out strings.Builder
	mock = big(ssh.MockCommand{Match: "aws s3 cp '/w/x' 's3://b/k/.incoming/1.pgdump' --metadata 'sha256=abc' --checksum-algorithm", Err: errors.New("exit status 2: Unknown options: --checksum-algorithm")})
	cl := NewClient(mock, &out)
	cl.readBack = 1
	if _, err := cl.put(ctx, S3Config{Bucket: "b", Region: "r"}, "/w/x", "s3://b/k/1.pgdump"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "verified by size alone") {
		t.Fatalf("out: %s", out.String())
	}
}

func TestFetchPassesNoMissingIdentity(t *testing.T) {
	mock := putMock(ssh.MockCommand{Match: "umask 077; age -d", Output: ""})
	if err := NewClient(mock, io.Discard).fetch(context.Background(), S3Config{Bucket: "b", Region: "r", ExtraIdentity: "/tmp/owner"}, "s3://b/k/1.pgdump.age", "/w/x"); err != nil {
		t.Fatal(err)
	}
	var dec string
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "umask 077; age -d") {
			dec = c
		}
	}
	if !strings.Contains(dec, `[ -r "$p" ]`) || !strings.Contains(dec, "-i '/tmp/owner'") || strings.Contains(dec, `-i "$(`) {
		t.Fatalf("%s", dec)
	}
}

func TestAddressableDBRefusesAConnectionURI(t *testing.T) {
	if addressableDB("postgresql://host/x") || addressableDB("postgres://u@h/x") {
		t.Fatal("pg_dump reads these as conninfo")
	}
}

func TestStaleWorkspacesLookInTheConfiguredDir(t *testing.T) {
	t.Setenv("TEPLOY_BACKUP_TMPDIR", "/data/tmp")
	if !strings.Contains(tmpGlobs("", "teploy-backup.*"), "'/data/tmp'/teploy-backup.*") {
		t.Fatal(tmpGlobs("", "teploy-backup.*"))
	}
	t.Setenv("TEPLOY_BACKUP_TMPDIR", "")
	if !strings.Contains(LeftoverWorkspacesProbe("/mnt/big"), "'/mnt/big'/teploy-backup.*") {
		t.Fatal(LeftoverWorkspacesProbe("/mnt/big"))
	}
}

// A bucket-wide multipart listing would catch other servers' uploads.
func TestAbortStaleUploadsIsScopedToOwnedNamespaces(t *testing.T) {
	old := time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)
	mock := newStoreMock("h",
		ssh.MockCommand{Match: "aws s3 ls 's3://b/mine/.owners/'", Output: "d t 20 mine/.owners/0123456789abcdef\n"},
		ssh.MockCommand{Match: "aws s3 ls 's3://b/shared/.owners/'", Output: "d t 20 shared/.owners/aaaaaaaaaaaaaaaa\n"},
		ssh.MockCommand{Match: "aws s3api list-multipart-uploads", Output: `{"Uploads":[{"Key":"mine/x","UploadId":"u1","Initiated":"` + old + `"}]}`},
		ssh.MockCommand{Match: "aws s3api abort-multipart-upload", Output: ""},
	)
	if n := NewClient(mock, io.Discard).AbortStaleUploads(context.Background(), S3Config{Bucket: "b", Region: "r"}, []string{"mine", "shared"}, time.Now().UTC()); n != 1 {
		t.Fatalf("aborted %d", n)
	}
	lists := 0
	for _, c := range mock.Calls {
		if strings.HasPrefix(c, "aws s3api list-multipart-uploads") {
			lists++
			if !strings.Contains(c, "--prefix 'mine/'") {
				t.Fatalf("unscoped: %s", c)
			}
		}
	}
	if lists != 1 {
		t.Fatalf("lists = %d", lists)
	}
}
