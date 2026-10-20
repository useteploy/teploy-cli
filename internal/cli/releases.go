package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/releasemeta"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

// newReleasesCmd lists an app's releases. Static apps keep the historical
// directory listing (newest first, current starred). Container apps list
// their release records (internal/releasemeta), newest first, each marked
// current and rollback-able (containers still present for that version —
// keep_versions retention decides how far back that reaches). With --app
// X --host S both work without a folder, reading state from the server.
func newReleasesCmd(flags *Flags) *cobra.Command {
	var appName string
	cmd := &cobra.Command{
		Use:   "releases",
		Short: "List the app's releases",
		Long: `Lists releases on the server for the current app, newest first; the
currently-active release is marked with *.

For type:static apps this is the retained release directories. For
container apps it is the release records teploy writes at each deploy:
release id, when it deployed, the image (and digest), the config commit
and actor, and whether it can be rolled back to (containers for that
version still present per keep_versions retention) with the reason when
not.

--app X --host S works without a folder. --json emits the machine
envelope {machine_interface, host, app, deployment_type, current,
releases[]} for every app (the folder-static legacy array shape is kept
for teploy.yml-in-a-folder invocations without --app).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReleases(flags, appName, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&appName, "app", "", "app name — read state from the server instead of teploy.yml (requires --host)")
	return cmd
}

// releaseRowDTO is one row of `releases --json`.
type releaseRowDTO struct {
	Release           string    `json:"release"`
	DeployedAt        time.Time `json:"deployed_at,omitempty"`
	Image             string    `json:"image,omitempty"`
	ImageDigest       string    `json:"image_digest,omitempty"`
	ConfigCommit      string    `json:"config_commit,omitempty"`
	Actor             string    `json:"actor,omitempty"`
	Current           bool      `json:"current"`
	RollbackAvailable bool      `json:"rollback_available"`
	Reason            string    `json:"reason,omitempty"`
}

// releasesDTO is the `releases --json` envelope.
type releasesDTO struct {
	MachineInterface int             `json:"machine_interface"`
	Host             string          `json:"host"`
	App              string          `json:"app"`
	DeploymentType   string          `json:"deployment_type"`
	Current          string          `json:"current,omitempty"`
	Releases         []releaseRowDTO `json:"releases"`
}

func runReleases(flags *Flags, appName string, out io.Writer) error {
	if appName == "" {
		return runReleasesFolder(flags, out)
	}
	return runReleasesByApp(flags, appName, out)
}

// runReleasesFolder is the historical path: requires teploy.yml in the
// cwd and only ever supported static apps. Its --json shape (a bare
// array) is kept verbatim for existing consumers.
func runReleasesFolder(flags *Flags, out io.Writer) error {
	appCfg, err := config.LoadApp(".")
	if err != nil {
		return err
	}
	if !appCfg.IsStatic() {
		return fmt.Errorf("'teploy releases' from a folder is only available for type:static apps (this app is type:%q) — container releases are listed with --app %s --host <server>", appCfg.Type, appCfg.App)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	executor, err := connectForApp(ctx, flags, appCfg)
	if err != nil {
		return err
	}
	defer executor.Close()

	releases, err := deploy.NewStaticDeployer(executor, out).ListReleases(ctx, appCfg.App, "")
	if err != nil {
		return fmt.Errorf("listing releases: %w", err)
	}
	current := ""
	if s, _ := state.Read(ctx, executor, appCfg.App); s != nil {
		current = s.CurrentHash
	}
	if flags.JSON {
		type releaseDTO struct {
			Version string `json:"version"`
			Current bool   `json:"current"`
		}
		result := make([]releaseDTO, 0, len(releases))
		for _, release := range releases {
			result = append(result, releaseDTO{Version: release, Current: release == current})
		}
		return json.NewEncoder(out).Encode(result)
	}
	return writeStaticReleaseTable(out, releases, current)
}

func writeStaticReleaseTable(out io.Writer, releases []string, current string) error {
	if len(releases) == 0 {
		fmt.Fprintln(out, "No releases yet — run 'teploy deploy' first.")
		return nil
	}
	for _, r := range releases {
		marker := "  "
		if r == current {
			marker = " *"
		}
		fmt.Fprintf(out, "%s %s\n", marker, r)
	}
	return nil
}

// connectAppServer validates --app/--host, connects and returns the
// server name plus executor. Shared by the folderless read commands.
func connectAppServer(ctx context.Context, flags *Flags, appName string) (string, ssh.Executor, error) {
	if err := config.ValidateName(appName); err != nil {
		return "", nil, err
	}
	if flags.Host == "" {
		return "", nil, fmt.Errorf("--host is required when using --app")
	}
	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return "", nil, err
	}
	if !flags.JSON {
		fmt.Printf("Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return "", nil, err
	}
	return flags.Host, executor, nil
}

// runReleasesByApp is the folderless path (dash, scripting): reads the
// app's state from the server and lists static directories or container
// release records accordingly. Exactly the facts each type keeps —
// nothing is reconstructed.
func runReleasesByApp(flags *Flags, appName string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	serverName, executor, err := connectAppServer(ctx, flags, appName)
	if err != nil {
		return err
	}
	defer executor.Close()

	st, err := state.Read(ctx, executor, appName)
	if err != nil {
		return fmt.Errorf("reading state for %q: %w", appName, err)
	}
	if st == nil {
		return fmt.Errorf("no state found for app %q on %s — has it been deployed?", appName, serverName)
	}

	if st.DeploymentType == "static" {
		releases, err := deploy.NewStaticDeployer(executor, out).ListReleases(ctx, appName, "")
		if err != nil {
			return fmt.Errorf("listing releases: %w", err)
		}
		current := st.CurrentHash
		if flags.JSON {
			rows := make([]releaseRowDTO, 0, len(releases))
			for _, r := range releases {
				rows = append(rows, staticReleaseRow(r, current))
			}
			return json.NewEncoder(out).Encode(releasesDTO{
				MachineInterface: MachineInterface, Host: executor.Host(), App: appName,
				DeploymentType: "static", Current: current, Releases: rows,
			})
		}
		return writeStaticReleaseTable(out, releases, current)
	}

	rows, err := containerReleaseRows(ctx, executor, appName, st)
	if err != nil {
		return err
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(releasesDTO{
			MachineInterface: MachineInterface, Host: executor.Host(), App: appName,
			DeploymentType: "container", Current: st.CurrentHash, Releases: rows,
		})
	}
	return writeContainerReleaseTable(out, rows)
}

func staticReleaseRow(release, current string) releaseRowDTO {
	row := releaseRowDTO{Release: release, Current: release == current}
	if row.Current {
		row.Reason = "current release"
	} else {
		row.RollbackAvailable = true
	}
	return row
}

// containerReleaseRows builds the container-app listing from release
// records plus the retained-container inventory: records carry what was
// deployed (image, digest, commit, actor, time), docker's teploy.version
// labels carry what is still rollback-able. Versions with containers but
// no record (deployed before the store existed) are listed from the
// container labels so a rollback-able release is never hidden.
func containerReleaseRows(ctx context.Context, executor ssh.Executor, app string, st *state.AppState) ([]releaseRowDTO, error) {
	records, warns, err := releasemeta.List(ctx, executor, app)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
	containers, err := docker.NewClient(executor).ListContainers(ctx, app)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	hasWeb := map[string]bool{}
	image := map[string]string{}
	created := map[string]time.Time{}
	for _, c := range containers {
		v := c.Labels["teploy.version"]
		if v == "" {
			continue
		}
		if c.Labels["teploy.process"] == "web" {
			hasWeb[v] = true
		}
		if image[v] == "" {
			image[v] = c.Image
		}
		if t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", c.CreatedAt); err == nil {
			if prev, ok := created[v]; !ok || t.After(prev) {
				created[v] = t
			}
		}
	}

	rows := make([]releaseRowDTO, 0, len(records))
	seen := map[string]bool{}
	for _, rec := range records {
		seen[rec.Hash] = true
		rows = append(rows, releaseRowDTO{
			Release:           rec.Hash,
			DeployedAt:        rec.CreatedAt,
			Image:             rec.ImageRef,
			ImageDigest:       rec.ImageDigest,
			ConfigCommit:      rec.ConfigCommit,
			Actor:             rec.Actor,
			Current:           rec.Hash == st.CurrentHash,
			RollbackAvailable: false,
		})
	}
	// Retained versions without a record.
	var extra []string
	for v := range hasWeb {
		if !seen[v] {
			extra = append(extra, v)
		}
	}
	sort.Strings(extra)
	for _, v := range extra {
		rows = append(rows, releaseRowDTO{
			Release:    v,
			DeployedAt: created[v],
			Image:      image[v],
			Current:    v == st.CurrentHash,
		})
	}
	for i := range rows {
		rows[i].RollbackAvailable, rows[i].Reason = rollbackAvailability(rows[i].Release, rows[i].Current, hasWeb)
	}
	return rows, nil
}

// rollbackAvailability mirrors deploy.Rollback's own target checks: the
// current release cannot be rolled back to, and a version whose web
// containers are gone (keep_versions retention pruned them) cannot
// either.
func rollbackAvailability(version string, current bool, hasWeb map[string]bool) (bool, string) {
	switch {
	case current:
		return false, "current release"
	case hasWeb[version]:
		return true, ""
	default:
		return false, "no containers retained for this version (keep_versions retention)"
	}
}

func writeContainerReleaseTable(out io.Writer, rows []releaseRowDTO) error {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No releases yet — run 'teploy deploy' first.")
		return nil
	}
	for _, r := range rows {
		marker := "  "
		note := "rollback available"
		if r.Current {
			marker = " *"
			note = "current"
		} else if !r.RollbackAvailable {
			note = r.Reason
		}
		when := ""
		if !r.DeployedAt.IsZero() {
			when = r.DeployedAt.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		line := fmt.Sprintf("%s %s", marker, r.Release)
		if when != "" {
			line += "\t" + when
		}
		if r.Image != "" {
			line += "\t" + r.Image
		}
		fmt.Fprintf(out, "%s\n\t%s\n", line, note)
	}
	return nil
}
