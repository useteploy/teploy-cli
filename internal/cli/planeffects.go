package cli

// Plan effect surfaces (C05): the known-effect diff between the planned
// config and the deployed release, per surface (routing, env, storage,
// resources, accessories). All functions are pure over their inputs —
// the deployed side comes from state.AppState (authoritative identity:
// domain, ingress mode, current port) and the applied manifest view
// (recorded config: env keys, volumes, resources, accessories). A
// missing manifest (pre-manifest release) degrades the diff to "current
// unknown" entries rather than fabricated "no change".

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/state"
)

// deployedRouting is the deployed routing identity used for diffing.
type deployedRouting struct {
	domain   string
	ingress  string
	port     int
	publish  []string
	deployed bool
}

// normalizeDomainForDiff lowercases/sorts a comma-separated domain list
// the same way the applied manifest records it, so ordering never reads
// as drift.
func normalizeDomainForDiff(domain string) string {
	if domain == "" {
		return ""
	}
	hosts := strings.Split(domain, ",")
	for i := range hosts {
		hosts[i] = strings.ToLower(strings.TrimSpace(hosts[i]))
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ",")
}

// routingEffects diffs the planned ingress/routing against the deployed
// release. Routing covers: domain set, ingress mode (caddy vs host
// publish vs external front), the application port (which the Caddy
// route AND the health gate probe), and extra publishes.
func routingEffects(appCfg *config.AppConfig, current *state.AppState, view *config.AppliedManifestView) []planEffect {
	var dep deployedRouting
	if current != nil {
		dep.deployed = true
		dep.domain = normalizeDomainForDiff(current.Domain)
		dep.ingress = current.IngressMode
		dep.port = current.CurrentPort
	}
	if view != nil && view.Container != nil {
		dep.publish = view.Container.Publish
	}

	wantIngress := appCfg.Ingress
	if wantIngress == "" {
		wantIngress = config.IngressCaddy
	}
	if dep.ingress == "" {
		dep.ingress = config.IngressCaddy
	}

	firstDeploy := detailFor(!dep.deployed, "first deploy", "")
	var effects []planEffect

	if d := normalizeDomainForDiff(appCfg.Domain); d != dep.domain {
		effects = append(effects, planEffect{
			Action: addRemoveChange(dep.domain != "", d != ""),
			Name:   "domain",
			From:   dep.domain, To: d,
			Detail: "routes served by this deployment" + firstDeploy,
		})
	}
	if wantIngress != dep.ingress {
		effects = append(effects, planEffect{
			Action: "change",
			Name:   "ingress mode",
			From:   dep.ingress, To: wantIngress,
			Detail: "how traffic reaches the app (caddy proxy / host port publish / external front)" + firstDeploy,
		})
	}
	wantPort := appCfg.Port
	if wantPort == 0 && (appCfg.Type == "" || appCfg.Type == config.TypeContainer) {
		wantPort = 80
	}
	if wantPort != dep.port {
		effects = append(effects, planEffect{
			Action: addRemoveChange(dep.port != 0, wantPort != 0),
			Name:   "application port",
			From:   portLabel(dep.port), To: portLabel(wantPort),
			Detail: "the Caddy route and health gate probe this port" + firstDeploy,
		})
	}

	// Extra publishes: recorded in the manifest only.
	wantPublish := append([]string(nil), appCfg.Publish...)
	sort.Strings(wantPublish)
	effects = append(effects, setDiffEffects("publish", dep.publish, wantPublish, "extra docker -p mapping")...)
	return effects
}

// envEffects diffs environment KEY presence (values are redacted by the
// manifest contract; a value change is not plannable — only key and
// env-file-reference changes are). Env-file NAMES diff separately: the
// files' contents are resolved at deploy time.
func envEffects(appCfg *config.AppConfig, view *config.AppliedManifestView) []planEffect {
	var haveKeys, haveFiles []string
	if view != nil && view.Container != nil {
		haveKeys = view.Container.EnvKeys
		haveFiles = view.Container.EnvFiles
	}
	wantKeys := make([]string, 0, len(appCfg.Env))
	for k := range appCfg.Env {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(wantKeys)

	effects := setDiffEffects("env", haveKeys, wantKeys, "environment variable (key presence planned; values resolved at deploy)")
	effects = append(effects, setDiffEffects("env_file", haveFiles, append([]string(nil), appCfg.EnvFiles...), "env file reference (contents resolved at deploy)")...)
	if len(appCfg.EnvFiles) > 0 {
		effects = append(effects, planEffect{
			Action: "change",
			Name:   "env values",
			Detail: fmt.Sprintf("%d env file(s) merge at deploy time — file VALUES are not planned, only key presence", len(appCfg.EnvFiles)),
		})
	}
	return effects
}

// storageEffects diffs volume declarations (config name -> container
// path). The detail names the host path teploy will manage for named
// volumes, using the same resolution the deploy path uses
// (plannedVolumeMounts — one home).
func storageEffects(app string, appCfg *config.AppConfig, view *config.AppliedManifestView) []planEffect {
	var have map[string]string
	if view != nil && view.Container != nil {
		have = view.Container.Volumes
	}
	want := appCfg.Volumes

	var names []string
	seen := map[string]bool{}
	for name := range have {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	for name := range want {
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	sort.Strings(names)

	var effects []planEffect
	for _, name := range names {
		havePath, haveOK := have[name]
		wantPath, wantOK := want[name]
		switch {
		case haveOK && !wantOK:
			effects = append(effects, planEffect{Action: "remove", Name: "volume " + name, From: havePath, Detail: "no longer mounted"})
		case !haveOK && wantOK:
			effects = append(effects, planEffect{Action: "add", Name: "volume " + name, To: wantPath, Detail: volumeMountDetail(app, name)})
		case havePath != wantPath:
			effects = append(effects, planEffect{Action: "change", Name: "volume " + name, From: havePath, To: wantPath, Detail: "container mount path changes"})
		}
	}
	return effects
}

// resourceEffects diffs container sizing: replicas, memory, cpu.
func resourceEffects(appCfg *config.AppConfig, view *config.AppliedManifestView) []planEffect {
	firstDeploy := view == nil || view.Container == nil
	firstNote := detailFor(firstDeploy, "first deploy or pre-manifest release — no recorded current value", "")

	wantReplicas := appCfg.Replicas
	if wantReplicas < 1 && (appCfg.Type == "" || appCfg.Type == config.TypeContainer) {
		wantReplicas = 1
	}

	var haveReplicas int
	var haveMemory, haveCPU string
	if !firstDeploy {
		haveReplicas = view.Container.Replicas
		haveMemory = view.Container.Memory
		haveCPU = view.Container.CPU
	}

	var effects []planEffect
	if haveReplicas != wantReplicas {
		effects = append(effects, planEffect{
			Action: addRemoveChange(haveReplicas != 0 && !firstDeploy, wantReplicas != 0),
			Name:   "replicas",
			From:   intLabel(haveReplicas), To: intLabel(wantReplicas),
			Detail: "web containers kept serving" + firstNote,
		})
	}
	if haveMemory != appCfg.Memory {
		effects = append(effects, planEffect{
			Action: addRemoveChange(haveMemory != "", appCfg.Memory != ""),
			Name:   "memory limit",
			From:   haveMemory, To: appCfg.Memory,
			Detail: "docker --memory" + firstNote,
		})
	}
	if haveCPU != appCfg.CPU {
		effects = append(effects, planEffect{
			Action: addRemoveChange(haveCPU != "", appCfg.CPU != ""),
			Name:   "cpu limit",
			From:   haveCPU, To: appCfg.CPU,
			Detail: "docker --cpus" + firstNote,
		})
	}
	return effects
}

// accessoryEffects diffs the accessory set and each accessory's image.
func accessoryEffects(appCfg *config.AppConfig, view *config.AppliedManifestView) []planEffect {
	var have map[string]config.AppliedManifestAccessory
	if view != nil {
		have = view.Accessories
	}
	want := appCfg.Accessories

	names := make([]string, 0, len(have)+len(want))
	seen := map[string]bool{}
	for name := range have {
		names = append(names, name)
		seen[name] = true
	}
	for name := range want {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var effects []planEffect
	for _, name := range names {
		h, haveOK := have[name]
		w, wantOK := want[name]
		switch {
		case haveOK && !wantOK:
			effects = append(effects, planEffect{Action: "remove", Name: "accessory " + name, From: h.Image, Detail: "retired by this config"})
		case !haveOK && wantOK:
			effects = append(effects, planEffect{Action: "add", Name: "accessory " + name, To: w.Image, Detail: accessoryDetail(w, "new stateful service, ensured before app containers start")})
		case h.Image != w.Image:
			effects = append(effects, planEffect{Action: "change", Name: "accessory " + name, From: h.Image, To: w.Image, Detail: accessoryDetail(w, "image changes; data volumes persist")})
		}
	}
	return effects
}

// setDiffEffects diffs two sorted-or-unsorted string sets into
// add/remove effects with the given label prefix.
func setDiffEffects(label string, have, want []string, detail string) []planEffect {
	haveSet := make(map[string]bool, len(have))
	for _, v := range have {
		haveSet[v] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, v := range want {
		wantSet[v] = true
	}
	var all []string
	for v := range haveSet {
		all = append(all, v)
	}
	for v := range wantSet {
		if !haveSet[v] {
			all = append(all, v)
		}
	}
	sort.Strings(all)

	var effects []planEffect
	for _, v := range all {
		switch {
		case haveSet[v] && !wantSet[v]:
			effects = append(effects, planEffect{Action: "remove", Name: label + " " + v, From: v, Detail: detail})
		case !haveSet[v] && wantSet[v]:
			effects = append(effects, planEffect{Action: "add", Name: label + " " + v, To: v, Detail: detail})
		}
	}
	return effects
}

// addRemoveChange maps presence to the action vocabulary.
func addRemoveChange(have, want bool) string {
	switch {
	case have && !want:
		return "remove"
	case !have && want:
		return "add"
	default:
		return "change"
	}
}

func detailFor(cond bool, whenTrue, whenFalse string) string {
	if cond {
		return " (" + whenTrue + ")"
	}
	return whenFalse
}

func portLabel(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

func intLabel(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// volumeMountDetail names the host side a named volume will occupy,
// via the deploy path's own resolution.
func volumeMountDetail(app, name string) string {
	if config.IsHostBindVolume(name) {
		return "host bind " + name + " mounted as-is (teploy never manages its contents)"
	}
	return "data at /deployments/" + app + "/volumes/" + name
}

// accessoryDetail summarizes the non-image delta an accessory effect
// implies.
func accessoryDetail(w config.AccessoryConfig, base string) string {
	var extras []string
	if len(w.Env) > 0 {
		extras = append(extras, fmt.Sprintf("%d env var(s)", len(w.Env)))
	}
	if len(w.Volumes) > 0 {
		extras = append(extras, fmt.Sprintf("%d volume(s)", len(w.Volumes)))
	}
	if len(extras) == 0 {
		return base
	}
	return base + " (" + strings.Join(extras, ", ") + ")"
}
