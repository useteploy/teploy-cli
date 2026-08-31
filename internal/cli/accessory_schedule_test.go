package cli

import (
	"os"
	"strings"
	"testing"
)

// A generated cron line must invoke the binary by absolute path.
//
// cron runs with PATH=/usr/bin:/bin. The CLI installs itself to
// /usr/local/bin/teploy for exactly these scheduled jobs — and then, for a
// year, wrote a cron line that called a bare `teploy`, which resolves to
// nothing under that PATH. Because the generated line carries no output
// redirect either, "command not found" went to a mailbox nobody reads.
//
// Found live on 2026-08-31: a nightly Nucleus accessory backup scheduled on
// 2026-07-12 had produced its last artifact on 2026-07-13. The two objects in
// the bucket were both made by hand at setup. The schedule had never once run,
// and the failure was invisible from every surface — `crontab -l` showed the
// job, cron was active, and the command was correct in every respect except
// the one that mattered.
//
// A source test because the alternative is a live cron and a day of waiting.
func TestScheduledAccessoryBackupUsesAnAbsolutePath(t *testing.T) {
	src, err := os.ReadFile("accessory.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	i := strings.Index(body, "backupCmd := fmt.Sprintf(")
	if i < 0 {
		t.Fatal("the scheduled backup command is no longer built here; move this test with it")
	}
	// The whole statement, not the first line: the format string and its
	// arguments are on separate lines, and the path is an argument.
	line := body[i:]
	if j := strings.Index(line, ")\n"); j > 0 {
		line = line[:j]
	}
	if strings.Contains(line, `"teploy accessory backup`) {
		t.Error("the generated cron line invokes a bare `teploy`; cron's PATH is /usr/bin:/bin " +
			"and the binary is installed to /usr/local/bin, so the job silently does nothing")
	}
	if !strings.Contains(line, "serverTeployPath") {
		t.Error("the generated cron line must use serverTeployPath")
	}
	if serverTeployPath != "/usr/local/bin/teploy" {
		t.Errorf("serverTeployPath = %q; it must match where deployTeployBinaryToServer installs it", serverTeployPath)
	}
	// The install and the invocation must name the SAME path — that they
	// disagreed is the whole defect.
	if !strings.Contains(body, "deployTeployBinaryToServer(ctx, executor, serverTeployPath)") {
		t.Error("the binary is installed to a path the generated command does not reference")
	}
}
