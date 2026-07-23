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

type containerDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Process   string `json:"process"`
	Version   string `json:"version"`
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
	Host       string         `json:"host"`
	Apps       []appStatusDTO `json:"apps"`
	ObservedAt time.Time      `json:"observed_at"`
	Errors     []machineError `json:"errors"`
}

func newAppListCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List apps deployed on a server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAppList(flags, cmd.OutOrStdout())
		},
	}
}

func runAppList(flags *Flags, out io.Writer) error {
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
	if flags.JSON {
		return json.NewEncoder(out).Encode(result)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("listing deployed apps: %s", result.Errors[0].Message)
	}
	if len(result.Apps) == 0 {
		fmt.Fprintln(out, "No deployed apps")
		return nil
	}
	fmt.Fprintf(out, "%-24s  %-12s  %-16s  %-10s  %s\n", "APP", "TYPE", "CURRENT", "CONTAINERS", "DOMAIN")
	for _, app := range result.Apps {
		fmt.Fprintf(out, "%-24s  %-12s  %-16s  %-10d  %s\n", app.App, app.Type, app.CurrentRelease.Version, len(app.Containers), app.Domain)
	}
	return nil
}

func collectAppList(ctx context.Context, executor ssh.Executor, observedAt time.Time) appListDTO {
	result := appListDTO{
		Host:       executor.Host(),
		Apps:       []appStatusDTO{},
		ObservedAt: observedAt,
		Errors:     []machineError{},
	}
	out, err := executor.Run(ctx, `for f in /deployments/*/state.json /deployments/*/state; do [ -f "$f" ] && basename "$(dirname "$f")"; done | sort -u`)
	if err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "apps", Message: err.Error()})
		return result
	}

	names := strings.Fields(out)
	sort.Strings(names)
	for _, name := range names {
		if err := config.ValidateName(name); err != nil {
			result.Errors = append(result.Errors, machineError{Scope: "app:" + name, Message: err.Error()})
			continue
		}
		result.Apps = append(result.Apps, collectAppStatus(ctx, executor, name, observedAt))
	}
	return result
}

func collectAppStatus(ctx context.Context, executor ssh.Executor, app string, observedAt time.Time) appStatusDTO {
	result := appStatusDTO{
		App:             app,
		CurrentRelease:  releaseStatusDTO{Ports: []int{}},
		PreviousRelease: releaseStatusDTO{Ports: []int{}},
		Containers:      []containerDTO{},
		Processes:       []processDTO{},
		ObservedAt:      observedAt,
		Errors:          []machineError{},
	}
	if current, err := state.Read(ctx, executor, app); err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "state", Message: err.Error()})
	} else if current != nil {
		result.Domain = current.Domain
		result.Type = current.DeploymentType
		result.Ingress = current.IngressMode
		result.CurrentRelease = releaseStatusDTO{Version: current.CurrentHash, Ports: nonNilInts(current.CurrentPorts)}
		previousVersion := current.PreviousHash
		if current.PreviousRelease != nil && current.PreviousRelease.Hash != "" {
			previousVersion = current.PreviousRelease.Hash
		}
		result.PreviousRelease = releaseStatusDTO{Version: previousVersion, Ports: nonNilInts(current.PreviousPorts)}
	}

	containers, err := docker.NewClient(executor).ListContainers(ctx, app)
	if err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "containers", Message: err.Error()})
	} else {
		result.Containers = containerDTOs(containers)
		result.Processes = processDTOs(result.Containers)
		if len(containers) > 0 && result.Type == "" {
			result.Type = config.TypeContainer
		}
	}
	if result.Type == "" {
		if _, err := executor.Run(ctx, "test -d /deployments/"+app+"/releases"); err == nil {
			result.Type = config.TypeStatic
		}
	}
	if lock, err := state.ReadLock(ctx, executor, app); err != nil {
		result.Errors = append(result.Errors, machineError{Scope: "lock", Message: err.Error()})
	} else if lock != nil {
		result.Lock = &lockDTO{Type: lock.Type, User: lock.User, Message: lock.Message, TS: lock.TS}
	}
	if _, err := executor.Run(ctx, "test -f /deployments/"+app+"/.maintenance-block"); err == nil {
		result.Maintenance = true
	}
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
	Server     string              `json:"server"`
	Host       string              `json:"host"`
	Uptime     uptimeDTO           `json:"uptime"`
	Load       loadDTO             `json:"load"`
	Memory     memoryDTO           `json:"memory"`
	Disks      []diskDTO           `json:"disks"`
	Docker     dockerInventoryDTO  `json:"docker"`
	Caddy      caddyObservationDTO `json:"caddy"`
	ObservedAt time.Time           `json:"observed_at"`
	Errors     []machineError      `json:"errors"`
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
		Server: server, Host: executor.Host(), ObservedAt: observedAt,
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
