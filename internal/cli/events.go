package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/events"
	"github.com/useteploy/teploy/internal/ssh"
)

func newEventsCmd(flags *Flags) *cobra.Command {
	var appName string
	var sinceSeq int64
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the server's event history (deploys, rollbacks, env and secret changes, ...)",
		Long: `Every client that acts on an app — this CLI on any laptop, teploy-dash, CI,
the autodeploy listener — records what it did in one ordered history on the
app's server (/deployments/.teploy/apps/<app>/events.jsonl). This command
reads it.

With --app, events for that app in sequence order; --since-seq N returns only
events after N (for polling). Without --app, every app on the server, ordered
by time. Env and secret events carry key names only.

  teploy events                          # the app in ./teploy.yml
  teploy events --host prod              # every app on prod
  teploy events --host prod --app blog --since-seq 41 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEvents(flags, appName, sinceSeq, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "only this app (default: the app in teploy.yml, or every app with --host)")
	cmd.Flags().Int64Var(&sinceSeq, "since-seq", 0, "only events with a sequence number above this (requires --app)")
	return cmd
}

type eventsDTO struct {
	MachineInterface int            `json:"machine_interface"`
	Host             string         `json:"host"`
	App              string         `json:"app,omitempty"`
	Events           []events.Event `json:"events"`
}

func runEvents(flags *Flags, appName string, sinceSeq int64, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if appName != "" {
		if err := config.ValidateName(appName); err != nil {
			return err
		}
	}
	if sinceSeq != 0 && appName == "" && flags.Host != "" {
		return fmt.Errorf("--since-seq needs --app (sequence numbers are per app)")
	}

	var executor ssh.Executor
	if flags.Host != "" {
		host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
		if err != nil {
			return fmt.Errorf("resolving host: %w", err)
		}
		executor, err = ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", host, err)
		}
	} else {
		appCfg, err := config.LoadApp(".")
		if err != nil {
			return fmt.Errorf("%w (or pass --host)", err)
		}
		if appName == "" {
			appName = appCfg.App
		}
		executor, err = connectForApp(ctx, flags, appCfg)
		if err != nil {
			return err
		}
	}
	defer executor.Close()

	evs, err := events.Read(ctx, executor, appName, sinceSeq)
	if err != nil {
		return err
	}
	if evs == nil {
		evs = []events.Event{}
	}
	if flags.JSON {
		return writeEventsJSON(out, executor.Host(), appName, evs)
	}
	printEvents(out, evs, appName == "")
	return nil
}

func writeEventsJSON(out io.Writer, host, app string, evs []events.Event) error {
	return json.NewEncoder(out).Encode(eventsDTO{MachineInterface: MachineInterface, Host: host, App: app, Events: evs})
}

// printEvents renders the human table. A "started" line with no later
// terminal line for the same app, kind and release is shown as having no
// recorded result (the process died, or it is still running).
func printEvents(out io.Writer, evs []events.Event, showApp bool) {
	if len(evs) == 0 {
		fmt.Fprintln(out, "No events recorded")
		return
	}
	finished := map[string]bool{}
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		key := ev.App + "\x00" + ev.Kind + "\x00" + ev.Release
		if ev.Outcome != events.Started {
			finished[key] = true
			continue
		}
		if !finished[key] {
			evs[i].Outcome = "started (no result recorded)"
		}
		delete(finished, key)
	}
	for _, ev := range evs {
		who := ev.Actor.Client
		if ev.Actor.Label != "" {
			who += ":" + ev.Actor.Label
		}
		app := ""
		if showApp {
			app = fmt.Sprintf("%-20s  ", ev.App)
		}
		fmt.Fprintf(out, "%s  %s%5d  %-16s %-10s %-12s %s%s\n",
			ev.TS.Local().Format(time.DateTime), app, ev.Seq, ev.Kind, ev.Outcome, ev.Release, who, eventDetailSuffix(ev))
	}
}

func eventDetailSuffix(ev events.Event) string {
	if len(ev.Detail) == 0 {
		return ""
	}
	var parts []string
	for _, k := range []string{"keys", "to", "branch", "preview", "container", "message", "error"} {
		if v, ok := ev.Detail[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, " ")
}
