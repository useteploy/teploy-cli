package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// DefaultPruneNotice is how long automatic pruning under the default
// keep_versions waits after a deploy first finds versions beyond it, so
// `teploy server audit` and the deploy output warn before anything goes. An
// explicit keep_versions already asked for pruning and has no notice.
const DefaultPruneNotice = 7 * 24 * time.Hour

// PruneNoticePath records when an app's notice period started.
func PruneNoticePath(app string) string {
	return "/deployments/" + app + "/.prune-notice"
}

// ParsePruneNotice reads a notice file's timestamp.
func ParsePruneNotice(data []byte) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return t, err == nil
}

// defaultPruneDue says whether a default-policy prune may run now, starting the
// notice period the first time there is something to prune. Anything it cannot
// establish means no prune this deploy.
func (d *Deployer) defaultPruneDue(ctx context.Context, app string, keep int, protected []string) bool {
	containers, err := d.docker.ListContainers(ctx, app)
	if err != nil {
		fmt.Fprintf(d.out, "Warning: version prune skipped: %v\n", err)
		return false
	}
	prunable := docker.PrunableVersions(containers, keep, protected...)
	if len(prunable) == 0 {
		return false
	}
	data, present, err := state.ReadRemoteFile(ctx, d.exec, PruneNoticePath(app))
	if err != nil {
		fmt.Fprintf(d.out, "Warning: version prune skipped: %v\n", err)
		return false
	}
	now := time.Now().UTC()
	started, ok := ParsePruneNotice(data)
	if !present || !ok {
		started = now
		if err := ssh.UploadAtomic(ctx, d.exec, strings.NewReader(now.Format(time.RFC3339)+"\n"), PruneNoticePath(app), "0600"); err != nil {
			fmt.Fprintf(d.out, "Warning: version prune skipped: could not record the notice: %v\n", err)
			return false
		}
	}
	if due := started.Add(DefaultPruneNotice); now.Before(due) {
		fmt.Fprintf(d.out, "Note: %d version(s) are beyond the default keep_versions (%d): %s. Deploys after %s remove them; set keep_versions (0 keeps everything) or pin what you need.\n",
			len(prunable), config.DefaultKeepVersions, strings.Join(prunable, ", "), due.Format("2006-01-02"))
		return false
	}
	return true
}
