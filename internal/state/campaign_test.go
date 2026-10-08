package state

import (
	"context"
	"errors"
	"fmt"
	"github.com/useteploy/teploy/internal/ssh"
	"regexp"
	"strings"
	"testing"
)

func TestCampaignPublicationRepairsSidecarWithoutCompensation(t *testing.T) {
	s := NewAppliedState(nil, "container", "external", "")
	s.CurrentHash = "v2"
	s.Generation = 2
	data, err := prepareState(s)
	if err != nil {
		t.Fatal(err)
	}
	exec := ssh.NewMockExecutor("test", ssh.MockCommand{Match: "if [ ! -e", Output: "present\n" + string(data)})
	if err := resolvePublication(exec, "app", data, 2, nil, errors.New("second rename failed")); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range exec.Calls {
		if c == "mv -fT -- '/deployments/app/.generation.tmp-commit' '/deployments/app/.generation'" {
			found = true
		}
	}
	if !found {
		t.Fatalf("repair not published: %v", exec.Calls)
	}
	_ = context.Background()
}
func TestCampaignPublicationUnknownPreservesAuthority(t *testing.T) {
	exec := ssh.NewMockExecutor("test", ssh.MockCommand{Match: "if [ ! -e", Err: errors.New("transport unavailable")})
	err := resolvePublication(exec, "app", []byte(`{}`), 2, nil, errors.New("lost reply"))
	if !PreservePublishedState(err) {
		t.Fatalf("unsafe compensation: %v", err)
	}
}

type campaignPartialPublication struct {
	*ssh.MockExecutor
	failRepair bool
}

func (e *campaignPartialPublication) Run(ctx context.Context, command string) (string, error) {
	if strings.Contains(command, " && mv -f -- ") && strings.Contains(command, "/state.json") {
		re := regexp.MustCompile(`mv -f -- '([^']+)' '([^']+/state.json)'`)
		match := re.FindStringSubmatch(command)
		if len(match) != 3 {
			return "", errors.New("unexpected commit shape")
		}
		e.Files[match[2]] = e.Files[match[1]]
		delete(e.Files, match[1])
		return "", errors.New("generation rename failed after authority published")
	}
	if e.failRepair && strings.Contains(command, "mv -fT -- ") {
		return "", errors.New("repair blocked")
	}
	return e.MockExecutor.Run(ctx, command)
}
func TestCampaignFencedWriterSecondRenameRecovery(t *testing.T) {
	for _, failRepair := range []bool{false, true} {
		t.Run(fmt.Sprint(failRepair), func(t *testing.T) {
			exec := &campaignPartialPublication{MockExecutor: ssh.NewMockExecutor("dummy", ssh.MockCommand{Match: "mkdir /deployments/demo/.lock"}), failRepair: failRepair}
			lk, err := AcquireLockFenced(context.Background(), exec, "demo")
			if err != nil {
				t.Fatal(err)
			}
			defer ReleaseLockFenced(exec, lk, "demo")
			s := NewAppliedState(nil, "container", "external", "")
			s.CurrentHash = "v2"
			s.Generation = 2
			err = WriteFencedGeneration(context.Background(), exec, "demo", s, lk, 1)
			if failRepair {
				if !PreservePublishedState(err) {
					t.Fatalf("compensation permitted after committed authority: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			actual, readErr := Read(context.Background(), exec, "demo")
			if readErr != nil || actual.CurrentHash != "v2" {
				t.Fatalf("authority lost: %+v %v", actual, readErr)
			}
		})
	}
}
