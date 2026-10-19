package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
	"github.com/useteploy/teploy/internal/state"
)

type machineError struct {
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

type releaseStatusDTO struct {
	Version string `json:"version"`
	Ports   []int  `json:"ports"`
}

// containerDTO: image_id/image_tags are set when the container was created
// by image ID; image then carries the first repo tag (docker.ResolveImageTags).
type containerDTO struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	ImageID   string   `json:"image_id,omitempty"`
	ImageTags []string `json:"image_tags,omitempty"`
	State     string   `json:"state"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	Process   string   `json:"process"`
	Version   string   `json:"version"`
}

type processDTO struct {
	Name       string   `json:"name"`
	Replicas   int      `json:"replicas"`
	Running    int      `json:"running"`
	Containers []string `json:"containers"`
}

type lockDTO struct {
	Type    string `json:"type"`
	User    string `json:"user"`
	Message string `json:"message"`
	TS      string `json:"ts"`
}

type appStatusDTO struct {
	App             string           `json:"app"`
	Domain          string           `json:"domain"`
	Type            string           `json:"type"`
	Ingress         string           `json:"ingress"`
	CurrentRelease  releaseStatusDTO `json:"current_release"`
	PreviousRelease releaseStatusDTO `json:"previous_release"`
	Containers      []containerDTO   `json:"containers"`
	Processes       []processDTO     `json:"processes"`
	Lock            *lockDTO         `json:"lock"`
	Maintenance     bool             `json:"maintenance"`
	ObservedAt      time.Time        `json:"observed_at"`
	Errors          []machineError   `json:"errors"`
}

type appListDTO struct {
	MachineInterface int            `json:"machine_interface"`
	Host             string         `json:"host"`
	Apps             []appStatusDTO `json:"apps"`
	ObservedAt       time.Time      `json:"observed_at"`
	Errors           []machineError `json:"errors"`
	// Undeployed (only with --include-undeployed) lists server-home apps
	// that have a config but no running release: never deployed, or
	// removed with their config kept (v3 tombstones).
	Undeployed []undeployedAppDTO `json:"undeployed,omitempty"`
}

type undeployedAppDTO struct {
	App        string    `json:"app"`
	Domain     string    `json:"domain,omitempty"`
	Generation uint64    `json:"generation"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func newAppListCmd(flags *Flags) *cobra.Command {
	var includeUndeployed bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List apps deployed on a server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppList(flags, cmd.OutOrStdout(), includeUndeployed)
		},
	}
	cmd.Flags().BoolVar(&includeUndeployed, "include-undeployed", false, "also list apps edited in Teploy that have no running release (in a separate `undeployed` list)")
	return cmd
}

func runAppList(flags *Flags, out io.Writer, includeUndeployed bool) error {
	if flags.Host == "" {
		return fmt.Errorf("--host is required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	host, user, key, err := config.ResolveServer(flags.Host, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving host: %w", err)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer executor.Close()

	result := collectAppList(ctx, executor, time.Now().UTC())
	if !includeUndeployed {
		result.Undeployed = nil
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(result)
	}
	return printAppList(out, result)
}

// printAppList renders the human table. One unreadable app (or a host-wide
// docker failure) no longer hides every other app: readable apps are listed,
// each failure is reported under the table with its scope, and the command
// still exits non-zero so scripts notice.
func printAppList(out io.Writer, result appListDTO) error {
	var problems []string
	for _, e := range result.Errors {
		problems = append(problems, fmt.Sprintf("%s: %s", e.Scope, e.Message))
	}
	if len(result.Apps) == 0 && len(problems) == 0 && len(result.Undeployed) == 0 {
		fmt.Fprintln(out, "No deployed apps")
		return nil
	}
	if len(result.Apps) > 0 {
		fmt.Fprintf(out, "%-24s  %-12s  %-16s  %-10s  %s\n", "APP", "TYPE", "CURRENT", "CONTAINERS", "DOMAIN")
	}
	for _, app := range result.Apps {
		current := app.CurrentRelease.Version
		for _, e := range app.Errors {
			problems = append(problems, fmt.Sprintf("app:%s %s: %s", app.App, e.Scope, e.Message))
			if current == "" {
				current = "(error)"
			}
		}
		fmt.Fprintf(out, "%-24s  %-12s  %-16s  %-10d  %s\n", app.App, app.Type, current, len(app.Containers), app.Domain)
	}
	if len(result.Undeployed) > 0 {
		fmt.Fprintln(out, "\nNot deployed (settings edited in Teploy):")
		for _, u := range result.Undeployed {
			fmt.Fprintf(out, "  %s\n", u.App)
		}
	}
	if len(problems) == 0 {
		return nil
	}
	fmt.Fprintln(out, "\nErrors:")
	for _, p := range problems {
		fmt.Fprintf(out, "  %s\n", p)
	}
	return fmt.Errorf("listing deployed apps on %s: %d problem(s), listed above", result.Host, len(problems))
}

func collectAppList(ctx context.Context, executor ssh.Executor, observedAt time.Time) appListDTO {
	result := appListDTO{
		MachineInterface: MachineInterface,
		Host:             executor.Host(),
		Apps:             []appStatusDTO{},
		ObservedAt:       observedAt,
		Errors:           []machineError{},
	}
	out, err := executor.Run(ctx, `for f in /deployments/*/state.json /deployments/*/state; do [ -f "$f" ] && basename "$(dirname "$f")"; done | sort -u`)
	if err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "apps", Message: err.Error()})
		return result
	}

	names := strings.Fields(out)
	sort.Strings(names)
	valid := make([]string, 0, len(names))
	for _, name := range names {
		if err := config.ValidateName(name); err != nil {
			result.Errors = append(result.Errors, machineError{Scope: "app:" + name, Message: err.Error()})
			continue
		}
		valid = append(valid, name)
	}

	// Batched observation: this used to run 5-6 sequential execs PER app
	// (state read, docker ps, image-tag resolve, lock read, two test
	// probes), which measured ~26s on a 41-app server — longer than any
	// dashboard's probe budget, so fleet views reported the box as unknown.
	// Now: one control-plane bundle per 100 apps, one docker ps for the
	// whole host, and at most one image-tag resolve. Scope note: a docker
	// failure is inherently host-wide, so it surfaces once at the top
	// level instead of being duplicated into every app's errors.
	snapshots, err := state.ReadAppSnapshots(ctx, executor, valid)
	snapByApp := make(map[string]state.AppSnapshot, len(snapshots))
	for _, snap := range snapshots {
		snapByApp[snap.App] = snap
	}
	if err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "apps", Message: err.Error()})
		// A wholesale failure is every app's state read failing at once;
		// keep the per-app state-scope errors the per-reader path produced
		// so consumers see which field is missing, not just an empty app.
		for _, name := range valid {
			snapByApp[name] = state.AppSnapshot{App: name, StateErr: err}
		}
	}

	var allContainers []docker.Container
	if len(valid) > 0 {
		dk := docker.NewClient(executor)
		var containersErr error
		allContainers, containersErr = dk.ListAllContainers(ctx)
		if containersErr == nil {
			allContainers = dk.ResolveImageTags(ctx, allContainers)
		} else {
			result.Errors = append(result.Errors, machineError{Scope: "containers", Message: containersErr.Error()})
			allContainers = nil
		}
	}

	for _, name := range valid {
		snap := snapByApp[name]
		if snap.State != nil && snap.State.Tombstone {
			result.Undeployed = append(result.Undeployed, undeployedAppDTO{App: name, Domain: snap.State.Domain, Generation: snap.State.Generation, UpdatedAt: snap.State.UpdatedAt})
			continue
		}
		result.Apps = append(result.Apps, assembleAppStatus(
			name, observedAt, snap, docker.ContainersByApp(allContainers, name)))
	}
	return result
}

// assembleAppStatus builds one app's DTO from an already-fetched snapshot
// and container set. It is the collection logic of the former per-app
// collectAppStatus, unchanged, with the transport removed so the batched
// fleet path and any future single-app path share it.
func assembleAppStatus(app string, observedAt time.Time, snap state.AppSnapshot, containers []docker.Container) appStatusDTO {
	result := appStatusDTO{
		App:             app,
		CurrentRelease:  releaseStatusDTO{Ports: []int{}},
		PreviousRelease: releaseStatusDTO{Ports: []int{}},
		Containers:      []containerDTO{},
		Processes:       []processDTO{},
		ObservedAt:      observedAt,
		Errors:          []machineError{},
	}
	if current := snap.State; current != nil {
		result.Domain = current.Domain
		result.Type = current.DeploymentType
		result.Ingress = current.IngressMode
		result.CurrentRelease = releaseStatusDTO{Version: current.CurrentHash, Ports: nonNilInts(current.CurrentPorts)}
		previousVersion := current.PreviousHash
		if current.PreviousRelease != nil && current.PreviousRelease.Hash != "" {
			previousVersion = current.PreviousRelease.Hash
		}
		result.PreviousRelease = releaseStatusDTO{Version: previousVersion, Ports: nonNilInts(current.PreviousPorts)}
	} else if snap.StateErr != nil {
		result.Errors = append(result.Errors, machineError{Scope: "state", Message: snap.StateErr.Error()})
	}

	result.Containers = containerDTOs(containers)
	result.Processes = processDTOs(result.Containers)
	if len(containers) > 0 && result.Type == "" {
		result.Type = config.TypeContainer
	}
	if result.Type == "" && snap.HasReleases {
		result.Type = config.TypeStatic
	}
	if snap.Lock != nil {
		result.Lock = &lockDTO{Type: snap.Lock.Type, User: snap.Lock.User, Message: snap.Lock.Message, TS: snap.Lock.TS}
	}
	result.Maintenance = snap.Maintenance
	return result
}

func nonNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}

func containerDTOs(containers []docker.Container) []containerDTO {
	result := make([]containerDTO, 0, len(containers))
	for _, container := range containers {
		result = append(result, containerDTO{
			ID: container.ID, Name: container.Name, Image: container.Image,
			ImageID: container.ImageID, ImageTags: container.ImageTags,
			State: container.State, Status: container.Status, CreatedAt: container.CreatedAt,
			Process: container.Labels["teploy.process"], Version: container.Labels["teploy.version"],
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func processDTOs(containers []containerDTO) []processDTO {
	byName := map[string]*processDTO{}
	for _, container := range containers {
		name := container.Process
		if name == "" {
			name = "unknown"
		}
		process := byName[name]
		if process == nil {
			process = &processDTO{Name: name, Containers: []string{}}
			byName[name] = process
		}
		process.Replicas++
		if container.State == "running" {
			process.Running++
		}
		process.Containers = append(process.Containers, container.Name)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]processDTO, 0, len(names))
	for _, name := range names {
		sort.Strings(byName[name].Containers)
		result = append(result, *byName[name])
	}
	return result
}

type uptimeDTO struct {
	Seconds float64 `json:"seconds"`
}

type loadDTO struct {
	One     float64 `json:"one"`
	Five    float64 `json:"five"`
	Fifteen float64 `json:"fifteen"`
}

type memoryDTO struct {
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type diskDTO struct {
	Filesystem     string `json:"filesystem"`
	Mountpoint     string `json:"mountpoint"`
	TotalBytes     uint64 `json:"total_bytes"`
	UsedBytes      uint64 `json:"used_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedPercent    string `json:"used_percent"`
}

type imageDTO struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Size       string `json:"size"`
	CreatedAt  string `json:"created_at"`
}

type dockerInventoryDTO struct {
	Installed  bool           `json:"installed"`
	Version    string         `json:"version"`
	Containers []containerDTO `json:"containers"`
	Images     []imageDTO     `json:"images"`
}

type caddyRouteDTO struct {
	Server     string   `json:"server"`
	ID         string   `json:"id"`
	Hosts      []string `json:"hosts"`
	Handlers   []string `json:"handlers"`
	Upstreams  []string `json:"upstreams"`
	StatusCode string   `json:"status_code"`
}

type caddyObservationDTO struct {
	Available bool            `json:"available"`
	Routes    []caddyRouteDTO `json:"routes"`
}

type serverStatusDTO struct {
	MachineInterface int                 `json:"machine_interface"`
	Server           string              `json:"server"`
	Host             string              `json:"host"`
	Uptime           uptimeDTO           `json:"uptime"`
	Load             loadDTO             `json:"load"`
	Memory           memoryDTO           `json:"memory"`
	Disks            []diskDTO           `json:"disks"`
	Docker           dockerInventoryDTO  `json:"docker"`
	Caddy            caddyObservationDTO `json:"caddy"`
	ObservedAt       time.Time           `json:"observed_at"`
	Errors           []machineError      `json:"errors"`
}

func newServerStatusCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "status <server-or-host>",
		Short: "Observe server resources, Docker inventory, and Caddy routes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerStatus(flags, args[0], cmd.OutOrStdout())
		},
	}
}

func runServerStatus(flags *Flags, target string, out io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	host, user, key, err := config.ResolveServer(target, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving server %q: %w", target, err)
	}
	if !flags.JSON {
		fmt.Fprintf(out, "Connecting to %s@%s...\n", user, host)
	}
	executor, err := ssh.Connect(ctx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", target, err)
	}
	defer executor.Close()

	result := collectServerStatus(ctx, executor, target, time.Now().UTC())
	if flags.JSON {
		return json.NewEncoder(out).Encode(result)
	}
	fmt.Fprintf(out, "Host: %s\nUptime: %.0fs\nLoad: %.2f %.2f %.2f\n", result.Host, result.Uptime.Seconds, result.Load.One, result.Load.Five, result.Load.Fifteen)
	fmt.Fprintf(out, "Memory: %d / %d bytes\nDocker: %d containers, %d images\nCaddy routes: %d\n", result.Memory.UsedBytes, result.Memory.TotalBytes, len(result.Docker.Containers), len(result.Docker.Images), len(result.Caddy.Routes))
	for _, observationErr := range result.Errors {
		fmt.Fprintf(out, "Warning (%s): %s\n", observationErr.Scope, observationErr.Message)
	}
	return nil
}

func collectServerStatus(ctx context.Context, executor ssh.Executor, server string, observedAt time.Time) serverStatusDTO {
	result := serverStatusDTO{
		MachineInterface: MachineInterface,
		Server:           server, Host: executor.Host(), ObservedAt: observedAt,
		Disks: []diskDTO{}, Errors: []machineError{},
		Docker: dockerInventoryDTO{Containers: []containerDTO{}, Images: []imageDTO{}},
		Caddy:  caddyObservationDTO{Routes: []caddyRouteDTO{}},
	}
	run := func(scope, command string) string {
		out, err := executor.Run(ctx, command)
		if err != nil {
			result.Errors = append(result.Errors, machineError{Scope: scope, Message: err.Error()})
			return ""
		}
		return strings.TrimSpace(out)
	}

	if fields := strings.Fields(run("uptime", "cat /proc/uptime")); len(fields) > 0 {
		result.Uptime.Seconds, _ = strconv.ParseFloat(fields[0], 64)
	}
	if fields := strings.Fields(run("load", "cat /proc/loadavg")); len(fields) >= 3 {
		result.Load.One, _ = strconv.ParseFloat(fields[0], 64)
		result.Load.Five, _ = strconv.ParseFloat(fields[1], 64)
		result.Load.Fifteen, _ = strconv.ParseFloat(fields[2], 64)
	}
	result.Memory = parseMemory(run("memory", "cat /proc/meminfo"))
	result.Disks = parseDisks(run("disks", "df -B1 -P"))

	version := run("docker.version", "docker version --format '{{.Server.Version}}'")
	result.Docker.Version = version
	result.Docker.Installed = version != ""
	if raw := run("docker.containers", "docker ps --all --format '{{json .}}'"); raw != "" {
		containers, err := docker.ParseContainers(raw)
		if err != nil {
			result.Errors = append(result.Errors, machineError{Scope: "docker.containers", Message: err.Error()})
		} else {
			containers = docker.NewClient(executor).ResolveImageTags(ctx, containers)
			result.Docker.Containers = containerDTOs(containers)
		}
	}
	if raw := run("docker.images", "docker image ls --format '{{json .}}'"); raw != "" {
		images, err := parseImages(raw)
		if err != nil {
			result.Errors = append(result.Errors, machineError{Scope: "docker.images", Message: err.Error()})
		} else {
			result.Docker.Images = images
		}
	}

	const caddyCommand = "docker exec caddy sh -c 'wget -qO- http://localhost:2019/config/apps/http 2>/dev/null || curl -sf http://localhost:2019/config/apps/http'"
	if raw := run("caddy.routes", caddyCommand); raw != "" {
		routes, err := parseCaddyRoutes(raw)
		if err != nil {
			result.Errors = append(result.Errors, machineError{Scope: "caddy.routes", Message: err.Error()})
		} else {
			result.Caddy.Available = true
			result.Caddy.Routes = routes
		}
	}
	return result
}

func parseMemory(raw string) memoryDTO {
	values := map[string]uint64{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = value * 1024
		}
	}
	total, available := values["MemTotal"], values["MemAvailable"]
	used := uint64(0)
	if total >= available {
		used = total - available
	}
	return memoryDTO{TotalBytes: total, UsedBytes: used, AvailableBytes: available}
}

func parseDisks(raw string) []diskDTO {
	result := []diskDTO{}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		total, totalErr := strconv.ParseUint(fields[1], 10, 64)
		used, usedErr := strconv.ParseUint(fields[2], 10, 64)
		available, availableErr := strconv.ParseUint(fields[3], 10, 64)
		if totalErr != nil || usedErr != nil || availableErr != nil {
			continue
		}
		result = append(result, diskDTO{Filesystem: fields[0], Mountpoint: strings.Join(fields[5:], " "), TotalBytes: total, UsedBytes: used, AvailableBytes: available, UsedPercent: fields[4]})
	}
	return result
}

func parseImages(raw string) ([]imageDTO, error) {
	type imageEntry struct {
		ID         string `json:"ID"`
		Repository string `json:"Repository"`
		Tag        string `json:"Tag"`
		Size       string `json:"Size"`
		CreatedAt  string `json:"CreatedAt"`
	}
	result := []imageDTO{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry imageEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parsing image inventory: %w", err)
		}
		result = append(result, imageDTO{ID: entry.ID, Repository: entry.Repository, Tag: entry.Tag, Size: entry.Size, CreatedAt: entry.CreatedAt})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repository == result[j].Repository {
			return result[i].Tag < result[j].Tag
		}
		return result[i].Repository < result[j].Repository
	})
	return result, nil
}

type rawCaddyRoute struct {
	ID    string `json:"@id"`
	Match []struct {
		Host []string `json:"host"`
	} `json:"match"`
	Handle []rawCaddyHandler `json:"handle"`
}

type rawCaddyHandler struct {
	Handler    string          `json:"handler"`
	StatusCode json.RawMessage `json:"status_code"`
	Upstreams  []struct {
		Dial string `json:"dial"`
	} `json:"upstreams"`
	Routes []rawCaddyRoute `json:"routes"`
}

func parseCaddyRoutes(raw string) ([]caddyRouteDTO, error) {
	var app struct {
		Servers map[string]struct {
			Routes []rawCaddyRoute `json:"routes"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(raw), &app); err != nil {
		return nil, fmt.Errorf("parsing Caddy routes: %w", err)
	}
	result := []caddyRouteDTO{}
	serverNames := make([]string, 0, len(app.Servers))
	for name := range app.Servers {
		serverNames = append(serverNames, name)
	}
	sort.Strings(serverNames)
	for _, server := range serverNames {
		flattenCaddyRoutes(server, app.Servers[server].Routes, nil, &result)
	}
	return result, nil
}

func flattenCaddyRoutes(server string, routes []rawCaddyRoute, inheritedHosts []string, result *[]caddyRouteDTO) {
	for _, route := range routes {
		hosts := append([]string(nil), inheritedHosts...)
		for _, match := range route.Match {
			hosts = append(hosts, match.Host...)
		}
		entry := caddyRouteDTO{Server: server, ID: route.ID, Hosts: uniqueStrings(hosts), Handlers: []string{}, Upstreams: []string{}}
		for _, handler := range route.Handle {
			if handler.Handler != "" {
				entry.Handlers = append(entry.Handlers, handler.Handler)
			}
			for _, upstream := range handler.Upstreams {
				entry.Upstreams = append(entry.Upstreams, upstream.Dial)
			}
			if len(handler.StatusCode) > 0 {
				var statusString string
				if json.Unmarshal(handler.StatusCode, &statusString) == nil {
					entry.StatusCode = statusString
				} else {
					var statusNumber int
					if json.Unmarshal(handler.StatusCode, &statusNumber) == nil {
						entry.StatusCode = strconv.Itoa(statusNumber)
					}
				}
			}
			flattenCaddyRoutes(server, handler.Routes, entry.Hosts, result)
		}
		entry.Handlers = uniqueStrings(entry.Handlers)
		entry.Upstreams = uniqueStrings(entry.Upstreams)
		if entry.ID != "" || len(entry.Hosts) > 0 || len(entry.Upstreams) > 0 || entry.StatusCode != "" {
			*result = append(*result, entry)
		}
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
