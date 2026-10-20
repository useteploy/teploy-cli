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
// nothing under that PATH. Because the generated line carried no output
// redirect either, "command not found" went to a mailbox nobody reads.
//
// Found live on 2026-08-31: a nightly Nucleus accessory backup scheduled on
// 2026-07-12 had produced its last artifact on 2026-07-13. The two objects
// in the bucket were both made by hand at setup. The schedule had never
// run, and the failure was invisible from every surface — `crontab -l`
// showed the job, cron was active, and the command was correct in every
// respect except the one that mattered.
//
// A source test because the alternative is a live cron and a day of waiting.
// Since D3 the line is `backup run` (see backup_run.go); the same absolute
// path rule applies, plus: no bucket, no credentials.
func TestScheduledAccessoryBackupUsesAnAbsolutePath(t *testing.T) {
	src, err := os.ReadFile("backup_run.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	i := strings.Index(body, "func buildScheduledBackupRunCmd")
	if i < 0 {
		t.Fatal("the scheduled backup command is no longer built here; move this test with it")
	}
	line := body[i:]
	if j := strings.Index(line, "\n}\n"); j > 0 {
		line = line[:j]
	}
	if strings.Contains(line, `"teploy `) {
		t.Error("the generated cron line invokes a bare `teploy`; cron's PATH is /usr/bin:/bin " +
			"and the binary is installed to /usr/local/bin, so the job silently does nothing")
	}
	if serverTeployPath != "/usr/local/bin/teploy" {
		t.Errorf("serverTeployPath = %q; it must match where deployTeployBinaryToServer installs it", serverTeployPath)
	}
	// The install and the invocation must name the SAME path — that they
	// disagreed is the whole defect.
	if !strings.Contains(body, "deployTeployBinaryToServer(ctx, executor, serverTeployPath, \"backup run\")") {
		t.Error("the binary is installed to a path the generated command does not reference")
	}
}

// The generated accessory schedule line must not embed the bucket or the
// credentials — both come from the server's stored target at run time.
func TestScheduledAccessoryLineCarriesNoTarget(t *testing.T) {
	line := buildScheduledBackupRunCmd(serverTeployPath, "myapp", "db")
	for _, banned := range []string{"--bucket", "TEPLOY_S3", "AWS_SECRET", "AWS_ACCESS"} {
		if strings.Contains(line, banned) {
			t.Errorf("generated cron line embeds %s: %s", banned, line)
		}
	}
	if !strings.Contains(line, "backup run --app 'myapp' --accessory 'db'") {
		t.Errorf("generated cron line is not the run form: %s", line)
	}
}
