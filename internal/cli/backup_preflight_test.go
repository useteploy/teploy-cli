package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/useteploy/teploy/internal/ssh"
	"strings"
	"testing"
	"time"
)

var setupNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const (
	probeReady   = "binary_version\tteploy 0.1.55\nfleet\tyes\naws\taws-cli/2.30.1 Python/3.13 Linux\nage\tv1.2.1\npython3\tPython 3.12.3\ndisk\t104857600\nend\t1\n"
	probeBare    = "binary_version\tteploy 0.1.20\nfleet\tno\naws\taws-cli/2.15.0 Python/3.11 Linux\nage_missing\t1\npython3_missing\t1\ndisk\t2097152\nend\t1\n"
	targetStored = "path /etc/teploy/backup-target.json\n" + `{"v":"1","bucket":"b","region":"r","prefix":"teploy/srv1","credentials":"x"}`
	policyStored = "path /etc/teploy/backup-policy.json\n" + `{"v":"1","interval":"1h","recipient":"` + testRecipient + `","heartbeat_urls":["https://dash.example.internal/api/push/secret-token"]}`
	cronFleet    = "5 * * * * /usr/local/bin/teploy backup run --all # [teploy-backup-all]\n"
	managedPlain = "owner\troot\nwritable\t1\nme\troot\nend\t1\n"
)

func preflightCommands(probe, target, policy, cron, managed string) []ssh.MockCommand {
	return []ssh.MockCommand{
		{Match: "sh -c '" + preflightProbeMarker, Output: probe},
		{Match: "sh -c '" + preflightManagedMarker, Output: managed},
		{Match: "for p in /etc/teploy/backup-target.json", Output: target},
		{Match: "if [ -d /etc/teploy ]", Output: policy},
		{Match: "crontab -l", Output: cron},
	}
}

func collectFrom(mock *ssh.MockExecutor) backupPreflightDTO {
	return collectBackupPreflight(context.Background(), mock, serverTeployPath, "0.1.55", setupNow)
}

func TestPreflightReportsAReadyServer(t *testing.T) {
	mock := ssh.NewMockExecutor("192.0.2.10", preflightCommands(probeReady, targetStored, policyStored, cronFleet, managedPlain)...)
	d := collectFrom(mock)
	if !d.Reachable || d.Error != "" {
		t.Fatalf("%+v", d)
	}
	if d.Binary.State != PreflightOK || d.Binary.Version != "0.1.55" || d.Binary.FleetEngine == nil || !*d.Binary.FleetEngine || d.Binary.SameAsCLI == nil || !*d.Binary.SameAsCLI {
		t.Fatalf("%+v", d.Binary)
	}
	if d.Tools.AWS.State != PreflightOK || d.Tools.AWS.Version != "2.30.1" || d.Tools.Age.State != PreflightOK || d.Tools.Python3.State != PreflightOK {
		t.Fatalf("%+v", d.Tools)
	}
	if d.Disk.FreeBytes == nil || *d.Disk.FreeBytes != 104857600*1024 {
		t.Fatalf("%+v", d.Disk)
	}
	if d.Target.State != PreflightConfigured || d.Target.Bucket != "b" || !d.Target.HasCredentials {
		t.Fatalf("%+v", d.Target)
	}
	if d.Policy.State != PreflightConfigured || d.Policy.ManagedElsewhere != ManagedNo || !d.Policy.Encrypted || d.Policy.CronInstalled == nil || !*d.Policy.CronInstalled {
		t.Fatalf("%+v", d.Policy)
	}
	// Tokens stay out: the host alone.
	body, _ := json.Marshal(d)
	if strings.Contains(string(body), "secret-token") {
		t.Fatalf("heartbeat token in the report: %s", body)
	}
}

// Read-only: the plain report runs nothing that changes the server.
func TestPreflightIsReadOnly(t *testing.T) {
	mock := ssh.NewMockExecutor("h", preflightCommands(probeBare, targetStored, policyStored, cronFleet, managedPlain)...)
	d, err := backupPreflightOn(context.Background(), mock, preflightOptions{Binary: serverTeployPath, CLIVersion: "0.1.55", Now: setupNow, InstallRelease: func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
		t.Fatal("installed without being asked")
		return "", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d.ToolsInstall != nil || d.BinaryUpgrade != nil {
		t.Fatalf("%+v", d)
	}
	for _, c := range mock.Calls {
		for _, verb := range []string{"apt-get", "yum ", "crontab -\n", "mv ", "python3 -c", "aws s3", "ufw", "systemctl"} {
			if strings.Contains(c, verb) && !strings.Contains(c, preflightProbeMarker) && !strings.Contains(c, "python3 -c 'import") {
				t.Fatalf("%q ran: %s", verb, c)
			}
		}
	}
}

// A probe that fails reads as unknown, never as fine.
func TestPreflightFailedProbesAreUnknown(t *testing.T) {
	cases := []struct {
		name  string
		probe string
		check func(backupPreflightDTO) bool
	}{
		{"cut short", "binary_version\tteploy 0.1.55\n", func(d backupPreflightDTO) bool {
			return d.Error != "" && d.Binary.State == PreflightUnknown && d.Binary.FleetEngine == nil && d.Tools.AWS.State == PreflightUnknown && d.Tools.Age.State == PreflightUnknown && d.Tools.Python3.State == PreflightUnknown && d.Disk.FreeBytes == nil
		}},
		{"binary does not run", "binary_broken\t1\naws\taws-cli/2.30.0\nend\t1\n", func(d backupPreflightDTO) bool {
			return d.Binary.State == PreflightUnknown && d.Binary.FleetEngine == nil && d.Binary.SameAsCLI == nil && d.Binary.Detail != ""
		}},
		{"help does not run", "binary_version\tteploy 0.1.55\nend\t1\n", func(d backupPreflightDTO) bool {
			return d.Binary.State == PreflightUnknown && d.Binary.FleetEngine == nil && d.Binary.Version == "0.1.55"
		}},
		{"aws wrapper with no version", "aws\tsome wrapper\nend\t1\n", func(d backupPreflightDTO) bool {
			return d.Tools.AWS.State == PreflightUnknown && d.Tools.AWS.Detail != ""
		}},
		{"aws does not run", "aws_broken\tcannot execute\nend\t1\n", func(d backupPreflightDTO) bool {
			return d.Tools.AWS.State == PreflightUnknown && strings.Contains(d.Tools.AWS.Detail, "cannot execute")
		}},
		{"disk not a number", "disk\t\nend\t1\n", func(d backupPreflightDTO) bool {
			return d.Disk.FreeBytes == nil && d.Disk.Detail != ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := collectFrom(ssh.NewMockExecutor("h", preflightCommands(tc.probe, "absent\n", "absent\n", "", "")...))
			if !d.Reachable || !tc.check(d) {
				t.Fatalf("%+v", d)
			}
		})
	}
	t.Run("old and missing are facts, not unknowns", func(t *testing.T) {
		d := collectFrom(ssh.NewMockExecutor("h", preflightCommands(probeBare, "absent\n", "absent\n", "", "")...))
		if d.Binary.State != PreflightNoFleet || *d.Binary.FleetEngine || d.Tools.AWS.State != PreflightOld || d.Tools.AWS.Detail == "" || d.Tools.Age.State != PreflightMissing || d.Tools.Python3.State != PreflightMissing {
			t.Fatalf("%+v", d)
		}
		if d.Target.State != PreflightAbsent || d.Policy.State != PreflightAbsent {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("no binary", func(t *testing.T) {
		d := collectFrom(ssh.NewMockExecutor("h", preflightCommands("binary_missing\t1\nend\t1\n", "absent\n", "absent\n", "", "")...))
		if d.Binary.State != PreflightMissing || d.Binary.FleetEngine == nil || *d.Binary.FleetEngine {
			t.Fatalf("%+v", d.Binary)
		}
	})
	t.Run("connection lost", func(t *testing.T) {
		d := collectFrom(ssh.NewMockExecutor("h", ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Err: errors.New("ssh: connection reset")}, ssh.MockCommand{Match: "sh -c", Err: errors.New("ssh: connection reset")}))
		if d.Reachable || d.Error == "" || d.Binary.State != PreflightUnknown || d.Target.State != PreflightUnknown || d.Policy.State != PreflightUnknown || d.Policy.ManagedElsewhere != PreflightUnknown {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("target and policy unreadable", func(t *testing.T) {
		cmds := []ssh.MockCommand{
			{Match: "sh -c '" + preflightProbeMarker, Output: probeReady},
			{Match: "for p in /etc/teploy/backup-target.json", Output: "garbage"},
			{Match: "if [ -d /etc/teploy ]", Err: errors.New("exit status 3: cannot tell")},
		}
		d := collectFrom(ssh.NewMockExecutor("h", cmds...))
		if d.Target.State != PreflightUnknown || d.Policy.State != PreflightUnknown || d.Policy.ManagedElsewhere != PreflightUnknown || d.Policy.Detail == "" {
			t.Fatalf("%+v %+v", d.Target, d.Policy)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		d := unreachablePreflight("h", serverTeployPath, "0.1.55", errors.New("dial tcp: i/o timeout"), setupNow)
		if d.Reachable || !strings.Contains(d.Error, "timeout") || d.Tools.AWS.State != PreflightUnknown || d.Disk.FreeBytes != nil || d.Target.State != PreflightUnknown {
			t.Fatalf("%+v", d)
		}
	})
}

func TestPreflightPolicyManagedElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name, cron, managed, want, reason string
	}{
		{"teploy's own", cronFleet, managedPlain, ManagedNo, ""},
		{"symlink", cronFleet, "symlink\t1\nowner\troot\nwritable\t1\nme\troot\nend\t1\n", ManagedYes, "symlink"},
		{"another owner", cronFleet, "owner\tansible\nwritable\t1\nme\tdeploy\nend\t1\n", ManagedYes, "owned by ansible"},
		{"cannot replace", cronFleet, "owner\troot\nme\tdeploy\nend\t1\n", ManagedYes, "cannot replace"},
		{"no cron line for its interval", "", managedPlain, ManagedYes, "crontab"},
		{"probe cut short", cronFleet, "owner\troot\n", PreflightUnknown, ""},
		{"owner unreadable", cronFleet, "writable\t1\nme\tdeploy\nend\t1\n", PreflightUnknown, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := collectFrom(ssh.NewMockExecutor("h", preflightCommands(probeReady, targetStored, policyStored, tc.cron, tc.managed)...))
			if d.Policy.ManagedElsewhere != tc.want {
				t.Fatalf("%+v", d.Policy)
			}
			if tc.reason != "" && !strings.Contains(strings.Join(d.Policy.ManagedReasons, ";"), tc.reason) {
				t.Fatalf("%+v", d.Policy)
			}
		})
	}
	// Hidden from this user: unknown, not "no".
	d := collectFrom(ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "sh -c '" + preflightProbeMarker, Output: probeReady},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"},
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Err: errors.New("exit status 3: cannot tell")},
	))
	if d.Policy.ManagedElsewhere != PreflightUnknown {
		t.Fatalf("%+v", d.Policy)
	}
	// Probes that could not run mean unknown too.
	d = collectFrom(ssh.NewMockExecutor("h",
		ssh.MockCommand{Match: "sh -c '" + preflightProbeMarker, Output: probeReady},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"},
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: policyStored},
		ssh.MockCommand{Match: "crontab -l", Err: errors.New("ssh: connection reset")},
		ssh.MockCommand{Match: "sh -c '" + preflightManagedMarker, Err: errors.New("ssh: connection reset")},
	))
	if d.Policy.ManagedElsewhere != PreflightUnknown || d.Policy.CronInstalled != nil {
		t.Fatalf("%+v", d.Policy)
	}
}

func installMock(probeBefore, probeAfter string, extra ...ssh.MockCommand) *ssh.MockExecutor {
	cmds := []ssh.MockCommand{{Match: "sh -c '" + preflightProbeMarker, Output: probeBefore, Once: true}, {Match: "sh -c '" + preflightProbeMarker, Output: probeAfter}}
	cmds = append(cmds, extra...)
	cmds = append(cmds,
		ssh.MockCommand{Match: "id -u", Output: "0\n"},
		ssh.MockCommand{Match: "which age", Output: "/usr/bin/age\n"},
		ssh.MockCommand{Match: "sh -c '" + "DEBIAN_FRONTEND", Output: ""},
		ssh.MockCommand{Match: "sh -c 'set -e", Output: ""},
		ssh.MockCommand{Match: "for p in /etc/teploy/backup-target.json", Output: "absent\n"},
		ssh.MockCommand{Match: "if [ -d /etc/teploy ]", Output: "absent\n"},
		ssh.MockCommand{Match: "python3 -c", Output: `{"seq":1,"ts":"2026-01-01T00:00:00Z"}`},
	)
	return ssh.NewMockExecutor("h", cmds...)
}

func toolsOpts() preflightOptions {
	return preflightOptions{InstallTools: true, Binary: serverTeployPath, CLIVersion: "0.1.55", Now: setupNow}
}

// Tools and nothing else: the aws step is setup's script with its signature check, and
// no hardening or other setup step runs.
func TestInstallToolsInstallsOnlyTheTools(t *testing.T) {
	mock := installMock(probeBare, probeReady)
	d, err := backupPreflightOn(context.Background(), mock, toolsOpts())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ToolsInstall) != 3 {
		t.Fatalf("%+v", d.ToolsInstall)
	}
	for _, a := range d.ToolsInstall {
		if a.Action != ActionDone {
			t.Fatalf("%+v", d.ToolsInstall)
		}
	}
	if d.ToolsInstall[0].Tool != "python3" {
		t.Fatalf("python3 goes first, AWS's bundle is unpacked with it: %+v", d.ToolsInstall)
	}
	all := strings.Join(mock.Calls, "\n")
	for _, must := range []string{"VALIDSIG", "apt-get install -y -qq python3"} {
		if !strings.Contains(all, must) {
			t.Fatalf("no %q in %s", must, all)
		}
	}
	for _, never := range []string{"ufw", "fail2ban", "sshd", "get.docker", "tailscale", "docker network", "PasswordAuthentication"} {
		if strings.Contains(all, never) {
			t.Fatalf("%q ran", never)
		}
	}
	if !strings.Contains(all, "append '_fleet'") {
		t.Fatal("no event")
	}
	if d.Tools.AWS.State != PreflightOK {
		t.Fatalf("the report must describe the server after the install: %+v", d.Tools)
	}
}

func TestInstallToolsIsIdempotent(t *testing.T) {
	mock := installMock(probeReady, probeReady)
	d, err := backupPreflightOn(context.Background(), mock, toolsOpts())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range d.ToolsInstall {
		if a.Action != ActionAlreadyDone {
			t.Fatalf("%+v", d.ToolsInstall)
		}
	}
	all := strings.Join(mock.Calls, "\n")
	if strings.Contains(all, "apt-get") || strings.Contains(all, "append '_fleet'") {
		t.Fatalf("nothing to do, and it did: %s", all)
	}
}

// One failing does not stop the next, and the report says which.
func TestInstallToolsReportsEachResult(t *testing.T) {
	failing := ssh.MockCommand{Match: "sh -c 'set -e", Output: "boom", Err: errors.New("exit status 1")}
	after := strings.Replace(probeReady, "aws\taws-cli/2.30.1 Python/3.13 Linux", "aws_missing\t1", 1)
	mock := installMock(probeBare, after, failing)
	d, err := backupPreflightOn(context.Background(), mock, toolsOpts())
	if err == nil || !strings.Contains(err.Error(), "installing aws") {
		t.Fatalf("%v", err)
	}
	got := map[string]preflightToolAction{}
	for _, a := range d.ToolsInstall {
		got[a.Tool] = a
	}
	if got["aws"].Action != ActionFailed || !strings.Contains(got["aws"].Detail, "boom") || got["python3"].Action != ActionDone || got["age"].Action != ActionDone {
		t.Fatalf("%+v", d.ToolsInstall)
	}
	if !strings.Contains(strings.Join(mock.Calls, "\n"), "append '_fleet'") {
		t.Fatal("no event")
	}
}

// An exit status of 0 is not success: the tool has to read as fine afterwards.
func TestInstallToolsChecksTheResult(t *testing.T) {
	mock := installMock(probeBare, strings.Replace(probeReady, "aws-cli/2.30.1", "aws-cli/2.15.0", 1))
	d, err := backupPreflightOn(context.Background(), mock, toolsOpts())
	if err == nil {
		t.Fatal("no error")
	}
	for _, a := range d.ToolsInstall {
		if a.Tool == "aws" && (a.Action != ActionFailed || !strings.Contains(a.Detail, PreflightOld)) {
			t.Fatalf("%+v", a)
		}
	}
}

func upgradeOpts(cli string, install func(context.Context, ssh.Executor, string, string, ...string) (string, error)) preflightOptions {
	return preflightOptions{UpgradeBinary: true, Binary: serverTeployPath, CLIVersion: cli, Now: setupNow, InstallRelease: install}
}

func TestUpgradeBinary(t *testing.T) {
	noInstall := func(t *testing.T) func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
		return func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
			t.Helper()
			t.Fatal("must not install")
			return "", nil
		}
	}
	t.Run("installs the CLI's own version and verifies it", func(t *testing.T) {
		mock := installMock(probeBare, probeReady)
		var tag string
		var caps []string
		d, err := backupPreflightOn(context.Background(), mock, upgradeOpts("v0.1.55", func(_ context.Context, _ ssh.Executor, path, tg string, c ...string) (string, error) {
			if path != serverTeployPath {
				t.Fatal(path)
			}
			tag, caps = tg, c
			return "0.1.55", nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if tag != "v0.1.55" || len(caps) != 1 || caps[0] != "backup run" {
			t.Fatalf("%q %v", tag, caps)
		}
		if u := d.BinaryUpgrade; u == nil || u.Action != ActionDone || u.From != "0.1.20" || u.To != "0.1.55" {
			t.Fatalf("%+v", d.BinaryUpgrade)
		}
		if !strings.Contains(strings.Join(mock.Calls, "\n"), "append '_fleet'") {
			t.Fatal("no event")
		}
	})
	t.Run("already there", func(t *testing.T) {
		mock := installMock(probeReady, probeReady)
		d, err := backupPreflightOn(context.Background(), mock, upgradeOpts("0.1.55", noInstall(t)))
		if err != nil || d.BinaryUpgrade.Action != ActionAlreadyDone {
			t.Fatalf("%+v %v", d.BinaryUpgrade, err)
		}
		if strings.Contains(strings.Join(mock.Calls, "\n"), "append '_fleet'") {
			t.Fatal("nothing changed, and an event says something did")
		}
	})
	for _, tc := range []struct {
		name, cli, probe, want string
	}{
		{"a development build has no release", "dev", probeBare, "not a release"},
		{"never downgrades", "0.1.54", probeReady, "newer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := installMock(tc.probe, tc.probe)
			d, err := backupPreflightOn(context.Background(), mock, upgradeOpts(tc.cli, noInstall(t)))
			if err == nil || d.BinaryUpgrade.Action != ActionRefused || !strings.Contains(d.BinaryUpgrade.Detail, tc.want) {
				t.Fatalf("%+v %v", d.BinaryUpgrade, err)
			}
			if !strings.Contains(strings.Join(mock.Calls, "\n"), "append '_fleet'") {
				t.Fatal("a refusal is on the record too")
			}
		})
	}
	t.Run("a binary path it does not manage", func(t *testing.T) {
		o := upgradeOpts("0.1.55", noInstall(t))
		o.Binary = "/opt/test/teploy"
		mock := installMock(probeBare, probeBare)
		d, err := backupPreflightOn(context.Background(), mock, o)
		if err == nil || d.BinaryUpgrade.Action != ActionRefused {
			t.Fatalf("%+v %v", d.BinaryUpgrade, err)
		}
	})
	t.Run("the install fails", func(t *testing.T) {
		mock := installMock(probeBare, probeBare)
		d, err := backupPreflightOn(context.Background(), mock, upgradeOpts("0.1.55", func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
			return "", errors.New("checksum mismatch")
		}))
		if err == nil || d.BinaryUpgrade.Action != ActionFailed || !strings.Contains(d.BinaryUpgrade.Detail, "checksum mismatch") {
			t.Fatalf("%+v %v", d.BinaryUpgrade, err)
		}
	})
	t.Run("the server still reads old after the install", func(t *testing.T) {
		mock := installMock(probeBare, probeBare)
		d, err := backupPreflightOn(context.Background(), mock, upgradeOpts("0.1.55", func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
			return "0.1.55", nil
		}))
		if err == nil || d.BinaryUpgrade.Action != ActionFailed || !strings.Contains(d.BinaryUpgrade.Detail, "0.1.20") {
			t.Fatalf("%+v %v", d.BinaryUpgrade, err)
		}
	})
	t.Run("same version without the engine is not done", func(t *testing.T) {
		same := strings.Replace(probeReady, "fleet\tyes", "fleet\tno", 1)
		mock := installMock(same, same)
		d, err := backupPreflightOn(context.Background(), mock, upgradeOpts("0.1.55", func(context.Context, ssh.Executor, string, string, ...string) (string, error) {
			return "0.1.55", nil
		}))
		if err == nil || d.BinaryUpgrade.Action != ActionFailed {
			t.Fatalf("%+v %v", d.BinaryUpgrade, err)
		}
	})
}
