package cli

import (
	"errors"
	"github.com/useteploy/teploy/internal/autodeploy"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type campaignGateLedger struct {
	entered chan struct{}
	release chan struct{}
	fail    bool
	once    sync.Once
}

func (l *campaignGateLedger) Append(_ autodeploy.AdmissionRecord) error {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	if l.fail {
		return errors.New("fsync failed")
	}
	return nil
}
func TestCampaignDuplicateWaitsForDurability(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "fail", false: "success"}[fail], func(t *testing.T) {
			l := &campaignGateLedger{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
			ledger := &memLedger{}
			run := newCountingRun()
			q := newAdmissionQueue(ledger, run.run, nil)
			h := newWebhookHandler(webhookHandlerConfig{secret: "dummy", branch: "main", app: "app", dedup: autodeploy.NewDeliveryDedup(), ledger: l, queue: q})
			done := make(chan int, 2)
			request := func() {
				body := `{"ref":"refs/heads/main","after":"86f7e437faa5a7fce15d1ddcb9eaeaea377667b8"}`
				r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				r.Header.Set("X-Hub-Signature-256", githubSign("dummy", []byte(body)))
				w := httptest.NewRecorder()
				h(w, r)
				done <- w.Code
			}
			go request()
			<-l.entered
			go request()
			select {
			case code := <-done:
				t.Fatalf("early ack %d", code)
			case <-time.After(30 * time.Millisecond):
			}
			close(l.release)
			for i := 0; i < 2; i++ {
				select {
				case code := <-done:
					if fail && code != 503 {
						t.Fatalf("failed admission acknowledged %d", code)
					}
					if !fail && code != 200 {
						t.Fatalf("successful admission %d", code)
					}
				case <-time.After(time.Second):
					t.Fatal("duplicate stuck")
				}
			}
			run.waitIdle(t, q)
		})
	}
}
func TestCampaignResidentContextContainment(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "api"), 0700)
	os.WriteFile(filepath.Join(root, "api", "Dockerfile"), []byte("FROM scratch"), 0600)
	got, err := buildContextDir(root, "api")
	if err != nil || filepath.Base(got) != "api" {
		t.Fatalf("%s %v", got, err)
	}
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "escape"))
	for _, p := range []string{"../outside", outside, "escape"} {
		if _, err := buildContextDir(root, p); err == nil {
			t.Fatalf("escape admitted %s", p)
		}
	}
}
func TestCampaignBuildVersionRefusesBeforeConfigEffects(t *testing.T) {
	for _, v := range []string{"../bad", strings.Repeat("a", 129), "a:b"} {
		err := runBuild(&Flags{}, v, "")
		if err == nil || !strings.Contains(err.Error(), "invalid --version") {
			t.Fatalf("%q %v", v, err)
		}
	}
}
