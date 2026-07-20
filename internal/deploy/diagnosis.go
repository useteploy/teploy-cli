package deploy

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/diagnose"
)

// printDiagnosis gathers post-mortem facts about the failed web container and
// prints rule-based findings under the log dump. Best-effort by design: every
// remote call is bounded, and any gather error just means fewer facts for the
// rules — a failed deploy must never hang or fail harder because of its own
// diagnosis.
func (d *Deployer) printDiagnosis(ctx context.Context, container string, configuredPort int, reason error, logs string) {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dc := diagnose.Context{
		Err:            reason,
		ExitCode:       -1,
		Logs:           logs,
		ConfiguredPort: configuredPort,
	}

	if out, err := d.exec.Run(gctx, fmt.Sprintf(
		"docker inspect -f '{{.State.Status}}|{{.State.ExitCode}}|{{.State.OOMKilled}}' %s", container,
	)); err == nil {
		parts := strings.Split(strings.TrimSpace(out), "|")
		if len(parts) == 3 {
			dc.State = parts[0]
			if n, err := strconv.Atoi(parts[1]); err == nil {
				dc.ExitCode = n
			}
			dc.OOMKilled = parts[2] == "true"
		}
	}

	if dc.State == "running" {
		out, _ := d.exec.Run(gctx, fmt.Sprintf(
			"docker exec %s sh -c 'ss -tlnH 2>/dev/null || netstat -tln 2>/dev/null || true'", container,
		))
		dc.Listeners, dc.ListenersKnown = diagnose.ParseListeners(out)
	}

	for _, f := range diagnose.Diagnose(dc) {
		fmt.Fprintf(d.out, "\nLikely cause: %s\n", f.Summary)
		for _, try := range f.Try {
			fmt.Fprintf(d.out, "  Try: %s\n", try)
		}
	}
}
