package autodeploy

import (
	"github.com/useteploy/teploy/internal/ssh"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCampaignLedgerTornTailRepairAndExclusiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	good := `{"kind":"admitted","id":"one"}` + "\n"
	if err := os.WriteFile(path, []byte(good+`{"kind":`), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenLedger(path); err == nil {
		second.Close()
		t.Fatal("concurrent writer accepted")
	}
	if err := l.Append(AdmissionRecord{Kind: AdmissionKindAdmitted, ID: "two"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	l, err = OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	b, _ := os.ReadFile(path)
	records, err := ParseLedger(b)
	if err != nil || len(records) != 2 || records[1].ID != "two" {
		t.Fatalf("replay: %+v %v", records, err)
	}
}
func TestCampaignLedgerInteriorCorruptionPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	bad := []byte("invalid\n")
	os.WriteFile(path, bad, 0600)
	if l, err := OpenLedger(path); err == nil {
		l.Close()
		t.Fatal("corruption admitted")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(bad) {
		t.Fatal("corrupt history modified")
	}
}

func TestCampaignScheduledScriptInvokesEngineForImmutableContainer(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "invocation")
	docker := filepath.Join(dir, "docker")
	os.WriteFile(docker, []byte("#!/bin/sh\n[ \"$1\" = ps ] || exit 90\nprintf 'demo-web-v1\\n'\n"), 0700)
	engine := filepath.Join(dir, "engine")
	os.WriteFile(engine, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+ssh.ShellQuote(capture)+"\n"), 0700)
	script := generateScheduledRedeployScript("demo", "main", engine)
	script = strings.ReplaceAll(script, `LOG="/deployments/$APP/scheduled-redeploy.log"`, `LOG="`+filepath.Join(dir, "log")+`"`)
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script %v %s", err, out)
	}
	b, err := os.ReadFile(capture)
	if err != nil || string(b) != "autodeploy\nredeploy\n--app\ndemo\n--branch\nmain\n" {
		t.Fatalf("engine invocation %s %v", b, err)
	}
}
