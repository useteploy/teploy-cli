package confighome

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// ErrOrphaned: state.json says the app's config lives on the server
// (schema v3) but config.git is gone. Deploys are refused until the
// operator chooses a home again.
var ErrOrphaned = errors.New("app is marked as edited in Teploy but its server config repository is missing")

// GateStatus is the app's home as seen under its lock.
type GateStatus struct {
	// Server is true when config.git exists (Teploy home).
	Server bool
	// Head is the config branch head ("" for an empty repository).
	Head string
}

// CheckGate reads the app's home under its lock and repairs the two
// states an older CLI can leave behind (spec §2.2), keeping the invariant
// state v3 <=> config.git exists:
//
//   - config.git present, state v2 (an old CLI deployed from a folder):
//     re-mark v3 and warn; containers are untouched.
//   - config.git present, no state (an old CLI removed the app): write the
//     v3 tombstone.
//
// State v3 without config.git is ErrOrphaned. Each repair appends a
// gate.repair event.
func CheckGate(ctx context.Context, exec ssh.Executor, app string, lk *state.Lock, out io.Writer) (GateStatus, error) {
	exists, head, err := Head(ctx, exec, app)
	if err != nil {
		return GateStatus{}, fmt.Errorf("reading %s's config home: %w", app, err)
	}
	st, err := state.ReadIncludingTombstone(ctx, exec, app)
	if err != nil {
		return GateStatus{}, err
	}
	if !exists {
		if st != nil && st.SchemaVersion == state.SchemaVersionV3 {
			return GateStatus{}, fmt.Errorf("%w (%s) — run `teploy config home folder --app %s` to edit it from a folder again, or restore the repository from a backup", ErrOrphaned, app, app)
		}
		return GateStatus{}, nil
	}
	repair := func(kind, message string, fix func() error) error {
		if err := fix(); err != nil {
			return fmt.Errorf("repairing %s's config home: %w", app, err)
		}
		if out != nil {
			fmt.Fprintf(out, "Warning: %s\n", message)
		}
		events.Record(ctx, exec, events.Event{App: app, Kind: events.KindGateRepair, Outcome: events.Succeeded, ConfigCommit: head, Detail: map[string]any{"case": kind, "message": message}})
		return nil
	}
	switch {
	case st == nil:
		err = repair("missing-state", fmt.Sprintf("%s had no state (removed by an older teploy CLI, or a restore that did not finish); recorded it as not deployed — its Teploy config is kept", app), func() error {
			return state.WriteTombstone(ctx, exec, app, lk)
		})
	case st.SchemaVersion != state.SchemaVersionV3:
		err = repair("folder-deploy", fmt.Sprintf("%s was deployed from a folder by an older teploy CLI at %s; the running config may differ from its Teploy config — review it with `teploy config diff --applied --app %s`, or switch the app back with `teploy config home folder`", app, st.UpdatedAt.Format("2006-01-02 15:04 UTC"), app), func() error {
			return state.SetSchemaVersion(ctx, exec, app, state.SchemaVersionV3, lk)
		})
	}
	if err != nil {
		return GateStatus{}, err
	}
	return GateStatus{Server: true, Head: head}, nil
}

// RefuseFolderDeploy is the error for a deploy that did not come from the
// app's server config (a folder, an ad-hoc override, a listener checkout)
// on an app whose settings are edited in Teploy.
func RefuseFolderDeploy(app, head string) error {
	return fmt.Errorf("%s's settings are edited in Teploy (config %s), not in a folder — deploy it with `teploy deploy --app %s --host <server>`, push this folder's teploy.yml with `teploy config push`, or move the app back with `teploy config home folder --app %s`", app, short(head), app, app)
}
