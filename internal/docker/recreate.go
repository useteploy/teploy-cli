package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// RecreateBinding is one host port binding of a container, normalized from
// docker's PortBindings map ("3000/tcp" keys) into comparable integers.
type RecreateBinding struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port,omitempty"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto,omitempty"` // "" and "tcp" are the same
}

// RecreateMount is one --mount-style mount (named volumes, tmpfs created via
// --mount, and anything else Binds does not cover).
type RecreateMount struct {
	Type     string `json:"type"` // "volume" | "bind" | "tmpfs" | "npipe"
	Source   string `json:"source,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// RecreateSpec is a container's complete docker-run-representable
// configuration, captured from docker inspect — the "full RecreateSpec" of
// audit F20. Recreation renders a docker run from this spec alone, so every
// field captured here is a field that survives recreate; the test suite
// pins one rendered command per field.
//
// Known, deliberate limits (documented rather than silent):
//   - Network aliases are captured for the primary network only; teploy
//     containers are single-network. Additional networks are preserved as
//     plain --network flags without their aliases.
//   - A multi-element entrypoint that differs from the image's own cannot be
//     represented through the docker CLI (which takes a single --entrypoint
//     string); recreate fails closed instead of silently mangling it.
type RecreateSpec struct {
	Name          string              `json:"name"`
	ImageID       string              `json:"image_id,omitempty"` // immutable sha256:… (top-level .Image)
	ImageRef      string              `json:"image_ref,omitempty"` // Config.Image original tag reference
	Entrypoint    []string            `json:"entrypoint,omitempty"`
	Cmd           []string            `json:"cmd,omitempty"`
	Env           []string            `json:"env,omitempty"`
	WorkingDir    string              `json:"working_dir,omitempty"`
	User          string              `json:"user,omitempty"`
	Labels        map[string]string   `json:"labels,omitempty"`
	NoHealthcheck bool                `json:"no_healthcheck,omitempty"`
	Networks      []string            `json:"networks,omitempty"` // non-default networks, primary first
	Aliases       []string            `json:"aliases,omitempty"`  // primary network's aliases, sorted
	PortBindings  []RecreateBinding   `json:"port_bindings,omitempty"`
	Binds         []string            `json:"binds,omitempty"`
	Mounts        []RecreateMount     `json:"mounts,omitempty"`
	MemoryBytes   int64               `json:"memory_bytes,omitempty"`
	NanoCPUs      int64               `json:"nano_cpus,omitempty"`
	RestartPolicy string              `json:"restart_policy,omitempty"`
	StopTimeout   int                 `json:"stop_timeout,omitempty"`
	StopSignal    string              `json:"stop_signal,omitempty"`
	LogDriver     string              `json:"log_driver,omitempty"`
	LogOpts       []string            `json:"log_opts,omitempty"` // sorted "k=v"
	ExtraHosts    []string            `json:"extra_hosts,omitempty"`
	Sysctls       map[string]string   `json:"sysctls,omitempty"`
	Tmpfs         map[string]string   `json:"tmpfs,omitempty"`
	CapAdd        []string            `json:"cap_add,omitempty"`
	CapDrop       []string            `json:"cap_drop,omitempty"`
	SecurityOpt   []string            `json:"security_opt,omitempty"`
	Privileged    bool                `json:"privileged,omitempty"`
	ReadonlyRootfs bool               `json:"readonly_rootfs,omitempty"`
}

// containerInspect mirrors the subset of `docker inspect` JSON used to build
// a RecreateSpec.
type containerInspect struct {
	// Image is the container's immutable, content-addressed image ID
	// (sha256:…). This — not Config.Image, which is the original (possibly
	// mutable tag) reference — is the identity a faithful recreation must
	// preserve: after a newer image is pulled under the same tag,
	// recreating from Config.Image silently runs the NEW bytes while
	// reporting the old version (audit F19).
	Image  string
	Config struct {
		Image       string
		Env         []string
		Cmd         []string
		Entrypoint  []string
		WorkingDir  string
		User        string
		Labels      map[string]string
		StopSignal  string
		StopTimeout int
		Healthcheck *struct {
			Test []string
		}
	}
	HostConfig struct {
		NetworkMode   string
		PortBindings  map[string][]struct {
			HostIp   string
			HostPort string
		}
		Binds         []string
		Mounts        []struct {
			Type     string // "bind" | "volume" | "tmpfs"
			Source   string
			Target   string
			ReadOnly bool
		}
		RestartPolicy struct {
			Name string
		}
		Memory        int64 // bytes
		NanoCpus      int64 // nano-CPUs (1 CPU = 1e9)
		LogConfig     struct {
			Type   string
			Config map[string]string
		}
		ExtraHosts   []string
		Sysctls      map[string]string
		Tmpfs        map[string]string
		CapAdd       []string
		CapDrop      []string
		SecurityOpt  []string
		Privileged   bool
		ReadonlyRootfs bool
	}
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases []string
		}
	}
}

// InspectRecreate reads a container's full inspect JSON and returns its
// RecreateSpec. One round trip; the same command Restart used to issue.
func (c *Client) InspectRecreate(ctx context.Context, name string) (*RecreateSpec, error) {
	raw, err := c.exec.Run(ctx, "docker inspect "+ssh.ShellQuote(name))
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", name, err)
	}
	var arr []containerInspect
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return nil, fmt.Errorf("parsing inspect of %s: %w", name, err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("container %s not found", name)
	}
	return specFromInspect(name, arr[0]), nil
}

// specFromInspect is the pure inspect-JSON -> RecreateSpec mapping, split out
// so tests can drive it without an executor.
func specFromInspect(name string, in containerInspect) *RecreateSpec {
	spec := &RecreateSpec{
		Name:          name,
		ImageID:       in.Image,
		ImageRef:      in.Config.Image,
		Entrypoint:    append([]string(nil), in.Config.Entrypoint...),
		Cmd:           append([]string(nil), in.Config.Cmd...),
		Env:           append([]string(nil), in.Config.Env...),
		WorkingDir:    in.Config.WorkingDir,
		User:          in.Config.User,
		Labels:        in.Config.Labels,
		StopSignal:    in.Config.StopSignal,
		StopTimeout:   in.Config.StopTimeout,
		Binds:         append([]string(nil), in.HostConfig.Binds...),
		MemoryBytes:   in.HostConfig.Memory,
		NanoCPUs:      in.HostConfig.NanoCpus,
		RestartPolicy: in.HostConfig.RestartPolicy.Name,
		LogDriver:     in.HostConfig.LogConfig.Type,
		ExtraHosts:    append([]string(nil), in.HostConfig.ExtraHosts...),
		Sysctls:       in.HostConfig.Sysctls,
		Tmpfs:         in.HostConfig.Tmpfs,
		CapAdd:        append([]string(nil), in.HostConfig.CapAdd...),
		CapDrop:       append([]string(nil), in.HostConfig.CapDrop...),
		SecurityOpt:   append([]string(nil), in.HostConfig.SecurityOpt...),
		Privileged:    in.HostConfig.Privileged,
		ReadonlyRootfs: in.HostConfig.ReadonlyRootfs,
	}
	if in.Config.Healthcheck != nil && len(in.Config.Healthcheck.Test) == 1 && in.Config.Healthcheck.Test[0] == "NONE" {
		spec.NoHealthcheck = true
	}

	// Networks: every non-default network, primary (HostConfig.NetworkMode)
	// first, the rest sorted for determinism.
	primary := in.HostConfig.NetworkMode
	isDefault := func(n string) bool { return n == "" || n == "default" || n == "bridge" }
	var others []string
	for n := range in.NetworkSettings.Networks {
		if !isDefault(n) && n != primary {
			others = append(others, n)
		}
	}
	sort.Strings(others)
	if !isDefault(primary) {
		spec.Networks = append(spec.Networks, primary)
	}
	spec.Networks = append(spec.Networks, others...)

	// Aliases on the primary network: skip the container-name auto-alias
	// (docker adds it itself) and dedupe; sorted for determinism.
	if netInfo, ok := in.NetworkSettings.Networks[primary]; ok {
		seenAlias := map[string]bool{name: true}
		aliases := append([]string(nil), netInfo.Aliases...)
		sort.Strings(aliases)
		for _, alias := range aliases {
			if alias == "" || seenAlias[alias] {
				continue
			}
			seenAlias[alias] = true
			spec.Aliases = append(spec.Aliases, alias)
		}
	}

	// Port bindings, normalized; sorted by container port then host port for
	// deterministic args ordering.
	for containerPort, bindings := range in.HostConfig.PortBindings {
		portStr, proto, _ := strings.Cut(containerPort, "/")
		cp := atoiOrZero(portStr)
		for _, b := range bindings {
			hostIP := b.HostIp
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			spec.PortBindings = append(spec.PortBindings, RecreateBinding{
				HostIP:        hostIP,
				HostPort:      atoiOrZero(b.HostPort),
				ContainerPort: cp,
				Proto:         proto,
			})
		}
	}
	sort.Slice(spec.PortBindings, func(i, j int) bool {
		if spec.PortBindings[i].ContainerPort != spec.PortBindings[j].ContainerPort {
			return spec.PortBindings[i].ContainerPort < spec.PortBindings[j].ContainerPort
		}
		return spec.PortBindings[i].HostPort < spec.PortBindings[j].HostPort
	})

	for _, m := range in.HostConfig.Mounts {
		if m.Type == "" || m.Target == "" {
			continue
		}
		spec.Mounts = append(spec.Mounts, RecreateMount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}

	for k, v := range in.HostConfig.LogConfig.Config {
		spec.LogOpts = append(spec.LogOpts, k+"="+v)
	}
	sort.Strings(spec.LogOpts)

	return spec
}

func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// RenderRecreateArgs renders the docker run arguments (excluding the leading
// "docker run") for the spec. Pure; every value is quoted exactly once at
// this shell boundary (inspect-derived values are DATA, not shell syntax —
// audit TCL-11). avoidPorts handling lives in Recreate, which adjusts the
// spec's host bindings before rendering.
func RenderRecreateArgs(spec *RecreateSpec) ([]string, error) {
	q := ssh.ShellQuote
	args := []string{"-d", "--name", q(spec.Name)}

	for _, n := range spec.Networks {
		args = append(args, "--network", q(n))
	}
	for _, alias := range spec.Aliases {
		args = append(args, "--network-alias", q(alias))
	}

	for _, b := range spec.PortBindings {
		containerPort := strconv.Itoa(b.ContainerPort)
		if b.Proto != "" {
			containerPort += "/" + b.Proto
		}
		hostPort := ""
		if b.HostPort > 0 {
			hostPort = strconv.Itoa(b.HostPort)
		}
		args = append(args, "-p", q(b.HostIP+":"+hostPort+":"+containerPort))
	}

	for _, e := range spec.Env {
		args = append(args, "-e", q(e))
	}

	for _, b := range spec.Binds {
		args = append(args, "-v", q(b))
	}

	for _, m := range spec.Mounts {
		parts := []string{"type=" + m.Type, "target=" + m.Target}
		if m.Source != "" {
			parts = append(parts, "source="+m.Source)
		}
		if m.ReadOnly {
			parts = append(parts, "readonly")
		}
		args = append(args, "--mount", q(strings.Join(parts, ",")))
	}

	if spec.MemoryBytes > 0 {
		args = append(args, "--memory", fmt.Sprintf("%db", spec.MemoryBytes))
	}
	if spec.NanoCPUs > 0 {
		// NanoCpus → fractional --cpus (1e9 nano = 1.0 cpu).
		cpus := float64(spec.NanoCPUs) / 1e9
		args = append(args, "--cpus", strconv.FormatFloat(cpus, 'f', -1, 64))
	}

	if len(spec.Labels) > 0 {
		labelKeys := make([]string, 0, len(spec.Labels))
		for k := range spec.Labels {
			labelKeys = append(labelKeys, k)
		}
		sort.Strings(labelKeys)
		for _, k := range labelKeys {
			args = append(args, "--label", q(k+"="+spec.Labels[k]))
		}
	}

	if spec.NoHealthcheck {
		args = append(args, "--no-healthcheck")
	}

	if spec.RestartPolicy != "" && spec.RestartPolicy != "no" {
		args = append(args, "--restart", q(spec.RestartPolicy))
	}

	if spec.WorkingDir != "" {
		args = append(args, "-w", q(spec.WorkingDir))
	}
	if spec.User != "" {
		args = append(args, "-u", q(spec.User))
	}

	// Entry point: only a single element is representable through the docker
	// CLI. The common cases are "no override" (empty) and a one-string
	// override; a multi-element entrypoint is usually just the image's own,
	// which the recreated container inherits from the (immutable) image
	// anyway — Recreate verifies that before calling here.
	switch {
	case len(spec.Entrypoint) == 1:
		args = append(args, "--entrypoint", q(spec.Entrypoint[0]))
	case len(spec.Entrypoint) > 1:
		return nil, fmt.Errorf("container %s has a multi-element entrypoint (%q) that the docker CLI cannot represent; verify it matches the image's own before recreating", spec.Name, spec.Entrypoint)
	}

	if spec.StopTimeout > 0 {
		args = append(args, "--stop-timeout", strconv.Itoa(spec.StopTimeout))
	}
	if spec.StopSignal != "" {
		args = append(args, "--stop-signal", q(spec.StopSignal))
	}

	if spec.LogDriver != "" && spec.LogDriver != "json-file" {
		args = append(args, "--log-driver", q(spec.LogDriver))
	}
	for _, opt := range spec.LogOpts {
		args = append(args, "--log-opt", q(opt))
	}

	for _, h := range spec.ExtraHosts {
		args = append(args, "--add-host", q(h))
	}
	if len(spec.Sysctls) > 0 {
		sysctlKeys := make([]string, 0, len(spec.Sysctls))
		for k := range spec.Sysctls {
			sysctlKeys = append(sysctlKeys, k)
		}
		sort.Strings(sysctlKeys)
		for _, k := range sysctlKeys {
			args = append(args, "--sysctl", q(k+"="+spec.Sysctls[k]))
		}
	}
	if len(spec.Tmpfs) > 0 {
		tmpfsPaths := make([]string, 0, len(spec.Tmpfs))
		for k := range spec.Tmpfs {
			tmpfsPaths = append(tmpfsPaths, k)
		}
		sort.Strings(tmpfsPaths)
		for _, p := range tmpfsPaths {
			mount := p
			if opts := spec.Tmpfs[p]; opts != "" {
				mount += ":" + opts
			}
			args = append(args, "--tmpfs", q(mount))
		}
	}
	for _, cap := range spec.CapAdd {
		args = append(args, "--cap-add", q(cap))
	}
	for _, cap := range spec.CapDrop {
		args = append(args, "--cap-drop", q(cap))
	}
	for _, opt := range spec.SecurityOpt {
		args = append(args, "--security-opt", q(opt))
	}
	if spec.Privileged {
		args = append(args, "--privileged")
	}
	if spec.ReadonlyRootfs {
		args = append(args, "--read-only")
	}

	// Image (last positional before cmd). Prefer the container's immutable
	// top-level image ID over the Config.Image tag reference so a tag that
	// has moved to newer bytes cannot silently change what the recreated
	// container runs (F19). Config.Image is kept only as a fallback for
	// odd daemons that report no top-level ID.
	imageRef := spec.ImageID
	if imageRef == "" || !strings.HasPrefix(imageRef, "sha256:") {
		imageRef = spec.ImageRef
	}
	args = append(args, q(imageRef))

	for _, cmdPart := range spec.Cmd {
		args = append(args, q(cmdPart))
	}

	return args, nil
}

// Recreate force-removes the named container and runs a fresh one from the
// spec. avoidPorts is the set of host ports currently held by containers
// this recreation must not collide with; a binding whose original port is
// in the set gets a freshly allocated one (see Restart's doc comment for the
// rollback collision this exists for).
func (c *Client) Recreate(ctx context.Context, spec *RecreateSpec, avoidPorts map[int]bool) error {
	if spec == nil || spec.Name == "" {
		return fmt.Errorf("recreate requires a spec with a container name")
	}

	claimed := make(map[int]bool, len(avoidPorts))
	for p := range avoidPorts {
		claimed[p] = true
	}
	for i, b := range spec.PortBindings {
		if b.HostPort > 0 && claimed[b.HostPort] {
			newPort, err := c.FindAvailablePortExcluding(ctx, claimed)
			if err != nil {
				return fmt.Errorf("port %d for %s is held by another running container and no replacement port is available: %w", b.HostPort, spec.Name, err)
			}
			spec.PortBindings[i].HostPort = newPort
			claimed[newPort] = true
		}
	}

	// A multi-element entrypoint cannot be rendered. Before failing, check
	// whether it is simply the image's own (the overwhelmingly common case
	// for containers created without --entrypoint): the recreated container
	// inherits it from the immutable image ID, so nothing is lost by
	// dropping it from the rendered command.
	if len(spec.Entrypoint) > 1 {
		same, err := c.entrypointMatchesImage(ctx, spec)
		if err != nil {
			return err
		}
		if same {
			spec.Entrypoint = nil
		}
	}

	args, err := RenderRecreateArgs(spec)
	if err != nil {
		return err
	}

	if _, err := c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(spec.Name)); err != nil {
		return fmt.Errorf("removing old %s: %w", spec.Name, err)
	}
	if _, err := c.exec.Run(ctx, "docker run "+strings.Join(args, " ")); err != nil {
		return fmt.Errorf("recreating %s: %w", spec.Name, err)
	}
	return nil
}

// entrypointMatchesImage reports whether the container's multi-element
// entrypoint equals its image's own entrypoint, comparing the JSON arrays.
func (c *Client) entrypointMatchesImage(ctx context.Context, spec *RecreateSpec) (bool, error) {
	image := spec.ImageID
	if image == "" || !strings.HasPrefix(image, "sha256:") {
		image = spec.ImageRef
	}
	if image == "" {
		return false, nil
	}
	out, err := c.exec.Run(ctx, "docker image inspect -f '{{json .Config.Entrypoint}}' "+ssh.ShellQuote(image))
	if err != nil {
		// Unreadable image (pruned while the container lingered): cannot
		// prove equality; fail closed.
		return false, fmt.Errorf("comparing entrypoint against image %s: %w", image, err)
	}
	var imageEntry []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &imageEntry); err != nil {
		return false, fmt.Errorf("parsing image entrypoint: %w", err)
	}
	if len(imageEntry) != len(spec.Entrypoint) {
		return false, nil
	}
	for i := range imageEntry {
		if imageEntry[i] != spec.Entrypoint[i] {
			return false, nil
		}
	}
	return true, nil
}

// Restart fully recreates a container with its original configuration:
// inspect the existing container, force-remove it, then `docker run` with
// the same name + extracted args.
//
// Use this in rollback flows where plain Start() is insufficient. Docker
// 29 silently fails to re-publish HostConfig.PortBindings if another
// container has taken + released the host port since the original stop
// (the bind also detaches from custom networks). Recreating from scratch
// sidesteps that landmine.
//
// avoidPorts is the set of host ports currently held by containers this
// restart must not collide with — critically, the live container(s) being
// rolled back FROM, which are still running (and still holding their port)
// at the point Restart is called, since rollback starts the target before
// stopping the current version for zero-downtime. A single-hop rollback
// (to the immediately-previous version) can never collide: that version's
// port was freed by the deploy that superseded it and nothing has claimed
// it since. A --to rollback reaching back further can: Docker's ephemeral
// port allocator reuses freed ports, so an older version's original port
// may since have been handed to what is now the live container. Found via
// live testing (v1→49152, v1-tls→49153, v2 reused 49152 after v1-tls
// stopped; `rollback --to v1` then collided with the still-running v2).
// When a binding's original port is in avoidPorts, a fresh one is
// allocated instead — safe because HostPort() (not persisted state) is
// what the rest of the rollback reads back afterward. Pass nil to
// preserve every port binding exactly.
//
// Preserved across the recreate (audit F20): image (immutable ID), network
// mode + primary-network aliases + additional networks, port bindings
// (subject to the above), env, bind mounts, named-volume + tmpfs + --mount
// mounts, command, working dir, user, labels, restart policy, memory + CPU
// limits, the --no-healthcheck NONE marker, single-element entrypoint
// overrides, stop timeout + signal, log driver + options, extra hosts,
// sysctls, tmpfs mounts, capabilities, security options, privileged and
// read-only flags.
func (c *Client) Restart(ctx context.Context, name string, avoidPorts map[int]bool) error {
	spec, err := c.InspectRecreate(ctx, name)
	if err != nil {
		return err
	}
	return c.Recreate(ctx, spec, avoidPorts)
}

// HostPortFor returns the host port bound to the given CONTAINER port — the
// primary-port-aware form of HostPort (audit TCL-14). A multi-port
// container's "first" binding is map-order luck; a caller that knows the
// release's primary container port (from the F14 record) gets the port the
// health check should actually probe.
func (c *Client) HostPortFor(ctx context.Context, name string, containerPort int) (int, error) {
	out, err := c.exec.Run(ctx, fmt.Sprintf(
		"docker inspect -f '{{json .NetworkSettings.Ports}}' %s", ssh.ShellQuote(name)))
	if err != nil {
		return 0, fmt.Errorf("inspecting container %s ports: %w", name, err)
	}
	var ports map[string][]struct {
		HostIp   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &ports); err != nil {
		return 0, fmt.Errorf("parsing ports of container %s: %w", name, err)
	}
	// docker keys the map "<port>/<proto>"; try tcp first, then any proto.
	for _, key := range []string{strconv.Itoa(containerPort) + "/tcp", strconv.Itoa(containerPort)} {
		for _, b := range ports[key] {
			if p, err := strconv.Atoi(strings.TrimSpace(b.HostPort)); err == nil && p > 0 {
				return p, nil
			}
		}
	}
	return 0, fmt.Errorf("container %s has no host binding for container port %d", name, containerPort)
}
