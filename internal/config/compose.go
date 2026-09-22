package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/ssh"
	"gopkg.in/yaml.v3"
)

// composeFile represents the relevant parts of a docker-compose.yml.
type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

// composeHealthcheck mirrors the Compose healthcheck block. test is kept
// raw (list or string): the only translatable forms are the exec-list
// HTTP probe and the disabling forms, and everything else is refused
// rather than guessed at.
type composeHealthcheck struct {
	Test        interface{} `yaml:"test"`
	Interval    interface{} `yaml:"interval"`
	Timeout     interface{} `yaml:"timeout"`
	Retries     interface{} `yaml:"retries"`
	StartPeriod interface{} `yaml:"start_period"`
	Disable     *bool       `yaml:"disable"`
}

// composeService is the subset of the Compose service schema the importer
// understands. Fields parsed here but deliberately NOT translated carry a
// comment saying where the decision lives; the full per-field
// classification table (preserve/translate/reject/tolerate/ignore, with
// the reasons) is declared in compose_test.go's
// TestLoadCompose_FieldClassificationInventory.
type composeService struct {
	Image       string        `yaml:"image"`
	Build       interface{}   `yaml:"build"`       // string or struct
	Ports       []interface{} `yaml:"ports"`       // short-form strings or bare numbers; anything else is refused in composeAppPort
	Command     interface{}   `yaml:"command"`     // string or []string
	Environment interface{}   `yaml:"environment"` // map or list
	Volumes     []string      `yaml:"volumes"`

	// DependsOn is startup ORDERING under Compose. Parsed but deliberately
	// not translated: teploy already ensures every accessory is running
	// before any app container starts (cli/deploy.go "Ensure accessories
	// are running", cli/singledeploy.go), which honors the common
	// app-after-database ordering by construction. Readiness conditions
	// (service_healthy) are not waited for — documented in the
	// classification table.
	DependsOn interface{} `yaml:"depends_on"` // list or map

	// Healthcheck translates for the web service (HTTP probe path/interval
	// -> AppConfig.Health; disable/test:NONE ->
	// Healthcheck["web"].Disable) and for worker disable forms; accessory
	// healthchecks are inert (teploy supervises accessories itself).
	Healthcheck *composeHealthcheck `yaml:"healthcheck"`

	Networks      interface{}            `yaml:"networks"`       // tolerated only as the implicit default
	Restart       string                 `yaml:"restart"`        // tolerated: always / unless-stopped (teploy's own policies)
	EnvFile       interface{}            `yaml:"env_file"`       // rejected when non-empty (opaque file reference)
	Secrets       []interface{}          `yaml:"secrets"`        // rejected when non-empty
	Configs       []interface{}          `yaml:"configs"`        // rejected when non-empty
	Profiles      []string               `yaml:"profiles"`       // non-empty: service skipped (not deployed by default compose up)
	Extends       interface{}            `yaml:"extends"`        // rejected when present
	Deploy        map[string]interface{} `yaml:"deploy"`         // tolerated only as no-op defaults
	Labels        interface{}            `yaml:"labels"`         // deliberately ignored (container metadata)
	ContainerName string                 `yaml:"container_name"` // rejected (teploy owns naming)
	Hostname      string                 `yaml:"hostname"`       // rejected (identity, no home)
	WorkingDir    string                 `yaml:"working_dir"`    // rejected (no home)
	Entrypoint    interface{}            `yaml:"entrypoint"`     // rejected (no home)
	Privileged    *bool                  `yaml:"privileged"`     // true rejected (security)
	CapAdd        []string               `yaml:"cap_add"`        // non-empty rejected (security)
}

// knownAccessoryImages maps image prefixes to default ports.
var knownAccessoryImages = map[string]int{
	"postgres":      5432,
	"redis":         6379,
	"mysql":         3306,
	"mariadb":       3306,
	"mongo":         27017,
	"clickhouse":    9000,
	"meilisearch":   7700,
	"elasticsearch": 9200,
	"memcached":     11211,
	"rabbitmq":      5672,
	"nats":          4222,
}

// composeFileNames are the filenames to check, in priority order.
var composeFileNames = []string{
	"docker-compose.yml",
	"docker-compose.yaml",
	"compose.yml",
	"compose.yaml",
}

// LoadCompose reads a docker-compose file and maps it to an AppConfig.
// Returns nil if no compose file is found.
//
// The importer treats Compose as a SUBSET: every supplied field it knows
// about is preserved, explicitly translated, or rejected with a named,
// actionable error — never silently dropped. The per-field classification
// (translate / reject / tolerate-the-default / ignore, with reasons and
// the model homes) is declared in compose_test.go's
// TestLoadCompose_FieldClassificationInventory; the port grammar is
// declared on composeAppPort. Widen either deliberately, never silently.
// Fields outside the classification are still ignored — full Compose
// breadth remains open under C05.
func LoadCompose(dir string) (*AppConfig, error) {
	var data []byte
	for _, name := range composeFileNames {
		path := filepath.Join(dir, name)
		d, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		data = d
		break
	}
	if data == nil {
		return nil, nil
	}

	var compose composeFile
	if err := yaml.Unmarshal(data, &compose); err != nil {
		return nil, fmt.Errorf("parsing compose file: %w", err)
	}

	return mapCompose(dir, compose)
}

func mapCompose(dir string, compose composeFile) (*AppConfig, error) {
	cfg := &AppConfig{
		App:         filepath.Base(dir),
		Processes:   make(map[string]string),
		Accessories: make(map[string]AccessoryConfig),
	}

	// Clean app name: replace underscores and spaces with hyphens, lowercase.
	cfg.App = strings.ToLower(strings.ReplaceAll(cfg.App, "_", "-"))
	cfg.App = strings.ReplaceAll(cfg.App, " ", "-")

	// Services under non-default profiles are skipped deliberately:
	// `docker compose up` without --profile does not deploy them, so
	// importing them would deploy something Compose itself would not.
	// Empty profiles (= no restriction) import normally.
	services := make(map[string]composeService, len(compose.Services))
	for name, svc := range compose.Services {
		if len(svc.Profiles) > 0 {
			continue
		}
		services[name] = svc
	}

	// Find the main web service: the single non-accessory service that
	// publishes ports. Known accessory images are excluded even when they
	// publish ports — previously this loop took the FIRST service with
	// ports out of Go's map iteration, so the same file could import
	// differently on every run, and a database with a published port could
	// be mispicked as the app itself (its known-accessory classification
	// only ever ran for services AFTER the break). Candidates are sorted,
	// and with more than one there is no principled automatic choice —
	// fail and name them rather than guess.
	var webCandidates []string
	for name, svc := range services {
		if len(svc.Ports) > 0 && !isAccessoryImage(svc.Image) {
			webCandidates = append(webCandidates, name)
		}
	}
	sort.Strings(webCandidates)

	var webServiceName string
	var webService composeService
	switch {
	case len(webCandidates) == 1:
		webServiceName = webCandidates[0]
		webService = services[webServiceName]
	case len(webCandidates) > 1:
		return nil, fmt.Errorf("ambiguous compose import: multiple non-accessory services publish ports (%s) — teploy cannot pick the web service automatically; remove ports from the services that are not the app, or write teploy.yml manually", strings.Join(webCandidates, ", "))
	default:
		anyPorts := false
		for _, svc := range services {
			if len(svc.Ports) > 0 {
				anyPorts = true
				break
			}
		}
		if anyPorts {
			return nil, fmt.Errorf("no non-accessory service with ports found in compose file (only known accessory images publish ports) — set the app service's ports, or write teploy.yml manually")
		}
		return nil, fmt.Errorf("no service with ports found in compose file")
	}
	// The web service's ports decide the application container port.
	// Compose host-side bindings are deliberately not preserved (teploy
	// allocates host ports itself and routes via Caddy), while non-TCP
	// publishes carry into Publish verbatim. Unsupported grammar —
	// ranges, long-form entries, multiple distinct container ports —
	// is refused with the reason instead of given arbitrary meaning
	// (previously every port entry was used only for web candidacy and
	// silently discarded, so '8080:3000' imported with Port=0 and
	// deployed as :80).
	webPort, extraPublish, err := composeAppPort(webServiceName, webService.Ports)
	if err != nil {
		return nil, err
	}

	webBuildContext := parseBuildContext(webService.Build)

	// Set domain placeholder — user must set this.
	cfg.Domain = cfg.App + ".example.com"

	// Web process gets empty command (use image CMD).
	cfg.Processes["web"] = ""

	// If web service has an image (not build), use it.
	if webService.Image != "" && webBuildContext == "" {
		cfg.Image = webService.Image
	}

	// The application port resolved from the web service's ports.
	cfg.Port = webPort
	if len(extraPublish) > 0 {
		cfg.Publish = extraPublish
	}

	// Web service field contract: the same preserve/translate/reject pass
	// every other service gets, plus the healthcheck translation that only
	// has a home for the web process.
	var violations []string
	violations = append(violations, composeFieldViolations(webServiceName, webService)...)
	if webService.Healthcheck != nil {
		if composeHealthcheckDisabled(webService.Healthcheck) {
			if cfg.Healthcheck == nil {
				cfg.Healthcheck = make(map[string]ProcessHealth)
			}
			cfg.Healthcheck["web"] = ProcessHealth{Disable: true}
		} else {
			path, interval, err := translateComposeHealthcheck(webServiceName, webService.Healthcheck, webPort)
			if err != nil {
				violations = append(violations, err.Error())
			} else {
				cfg.Health.Path = path
				cfg.Health.IntervalSeconds = interval
			}
		}
	}

	// Classify remaining services.
	var unsupportedBuilds []string
	for _, name := range sortedServiceNames(services) {
		if name == webServiceName {
			continue
		}
		svc := services[name]

		svcBuildContext := parseBuildContext(svc.Build)

		// Check if it's a known accessory image.
		if isAccessoryImage(svc.Image) {
			violations = append(violations, composeFieldViolations(name, svc)...)
			// An accessory healthcheck is inert under teploy: teploy
			// supervises accessories (--restart always + running-state
			// checks) and never queries docker health, so it is ignored
			// rather than translated or rejected (classification table).

			acc := AccessoryConfig{
				Image: svc.Image,
				Port:  accessoryPort(svc.Image),
			}

			// Map environment variables.
			env := parseEnvironment(svc.Environment)
			if len(env) > 0 {
				acc.Env = env
			}

			// Map volumes.
			vols := parseServiceVolumes(svc.Volumes)
			if len(vols) > 0 {
				acc.Volumes = vols
			}

			cfg.Accessories[name] = acc
			continue
		}

		// Same build context as web → worker process.
		if svcBuildContext != "" && svcBuildContext == webBuildContext {
			violations = append(violations, composeFieldViolations(name, svc)...)
			if svc.Healthcheck != nil {
				if composeHealthcheckDisabled(svc.Healthcheck) {
					// The one worker healthcheck form with a home: suppress
					// the image's HEALTHCHECK for this process (workers
					// that share the web image inherit its HTTP probe,
					// which fails forever for a process with no listener).
					if cfg.Healthcheck == nil {
						cfg.Healthcheck = make(map[string]ProcessHealth)
					}
					cfg.Healthcheck[name] = ProcessHealth{Disable: true}
				} else {
					violations = append(violations, fmt.Sprintf("compose service %q: 'healthcheck' has no home for a non-web process — teploy health-gates only the web process (teploy.yml 'health:'); only the disabling forms (disable: true / test: [\"NONE\"]) translate; remove it or write teploy.yml", name))
				}
			}
			cfg.Processes[name] = parseCommand(svc.Command)
			continue
		}

		// No image and no build → skip unknown service.
		if svc.Image == "" && svcBuildContext == "" {
			continue
		}

		// Has a standalone image that isn't a known DB → treat as accessory.
		if svc.Image != "" {
			violations = append(violations, composeFieldViolations(name, svc)...)
			acc := AccessoryConfig{Image: svc.Image}
			env := parseEnvironment(svc.Environment)
			if len(env) > 0 {
				acc.Env = env
			}
			vols := parseServiceVolumes(svc.Volumes)
			if len(vols) > 0 {
				acc.Volumes = vols
			}
			cfg.Accessories[name] = acc
			continue
		}

		// Build context different from web's with no image: the
		// single-image process model cannot preserve an independent
		// build. Collect and refuse below — flattening the service
		// into a worker of web's image used to deploy the wrong code
		// under the right command (`jobs: build ./jobs` ran web's
		// image).
		unsupportedBuilds = append(unsupportedBuilds, fmt.Sprintf("%s (build %q)", name, svcBuildContext))
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		return nil, fmt.Errorf("unsupported compose fields: %s", strings.Join(violations, "; "))
	}

	if len(unsupportedBuilds) > 0 {
		sort.Strings(unsupportedBuilds)
		webSrc := fmt.Sprintf("%q builds from %q", webServiceName, webBuildContext)
		if webBuildContext == "" {
			webSrc = fmt.Sprintf("%q runs image %q", webServiceName, webService.Image)
		}
		return nil, fmt.Errorf("unsupported independent build in compose import: %s while %s — teploy runs one image per app and cannot preserve a separately built service; use the same build context as the app, a prebuilt image, or write teploy.yml", strings.Join(unsupportedBuilds, ", "), webSrc)
	}

	// Clean up empty maps.
	if len(cfg.Accessories) == 0 {
		cfg.Accessories = nil
	}
	if len(cfg.Processes) == 1 && cfg.Processes["web"] == "" {
		cfg.Processes = nil
	}

	return cfg, nil
}

// composeFieldViolations returns one named, actionable refusal per supplied
// Compose field whose silent loss would change deployment semantics (the
// C05 contract: preserve, translate, or reject — never drop). Only exact
// no-op equivalents are tolerated; each check's reason names what teploy
// cannot preserve and the alternative. The full classification table is
// declared in compose_test.go.
func composeFieldViolations(name string, svc composeService) []string {
	var v []string
	add := func(field, why string) {
		v = append(v, fmt.Sprintf("compose service %q: %s — %s; remove it or write teploy.yml", name, field, why))
	}
	if !composeNetworksNoop(svc.Networks) {
		add("'networks'", "teploy runs every container on its own managed network and cannot preserve Compose networks (only the implicit default, networks: [default], is a no-op)")
	}
	if !composeEnvFileNoop(svc.EnvFile) {
		add("'env_file'", "an opaque file reference whose Compose-specific interpolation the importer cannot resolve — move the values into 'environment' or configure teploy.yml 'env_files' deliberately")
	}
	if len(svc.Secrets) > 0 {
		add("'secrets'", "no secret-file model in the Compose import — use 'teploy secret set' or mount via 'volumes'")
	}
	if len(svc.Configs) > 0 {
		add("'configs'", "no config-file model in the Compose import — mount the files via 'volumes'")
	}
	if svc.Extends != nil {
		add("'extends'", "service inheritance cannot be resolved losslessly at import — inline the inherited fields")
	}
	if !composeDeployNoop(svc.Deploy) {
		add("'deploy'", "resources/replicas/mode cannot be preserved — teploy sizes workloads via teploy.yml (replicas, memory, cpu); only the Compose defaults (replicas: 1, mode: replicated) are a no-op")
	}
	switch svc.Restart {
	case "", "always", "unless-stopped":
		// Tolerated: teploy runs app containers --restart unless-stopped
		// and accessories --restart always; "always" differs from the app
		// policy only after a manual stop + daemon restart, which teploy's
		// own lifecycle owns (classification table).
	default:
		add(fmt.Sprintf("'restart: %s'", svc.Restart), "teploy supervises containers itself (app containers --restart unless-stopped, accessories --restart always); only 'always'/'unless-stopped' are equivalent, other policies change crash semantics")
	}
	if svc.ContainerName != "" {
		add("'container_name'", "teploy owns container naming ({app}-{process}-{version}) for lifecycle management")
	}
	if svc.Hostname != "" {
		add("'hostname'", "no teploy.yml equivalent — teploy derives container identity itself (name {app}-{process}-{version}, network alias {app}); software deriving identity from its hostname would silently change behavior")
	}
	if svc.WorkingDir != "" {
		add("'working_dir'", "no teploy.yml equivalent — set WORKDIR in the image")
	}
	if !composeEntrypointNoop(svc.Entrypoint) {
		add("'entrypoint'", "no teploy.yml equivalent — bake it into the image's ENTRYPOINT")
	}
	if svc.Privileged != nil && *svc.Privileged {
		add("'privileged'", "security-relevant — teploy runs unprivileged containers and cannot preserve it")
	}
	if len(svc.CapAdd) > 0 {
		add(fmt.Sprintf("'cap_add' (%s)", strings.Join(svc.CapAdd, ", ")), "security-relevant capabilities teploy's unprivileged containers cannot preserve")
	}
	return v
}

// composeNetworksNoop reports whether a networks value is exactly the
// implicit default Compose attaches anyway: ["default"], {default: {}},
// or absent/empty. Anything else (named networks, aliases, addresses)
// changes network semantics.
func composeNetworksNoop(v interface{}) bool {
	switch n := v.(type) {
	case nil:
		return true
	case []interface{}:
		for _, e := range n {
			if s, ok := e.(string); ok && s == "default" {
				continue
			}
			return false
		}
		return true
	case map[string]interface{}:
		for k, val := range n {
			if k != "default" {
				return false
			}
			if val == nil {
				continue
			}
			if m, ok := val.(map[string]interface{}); ok && len(m) == 0 {
				continue
			}
			return false
		}
		return true
	}
	return false
}

// composeEnvFileNoop reports whether an env_file value is absent or empty.
func composeEnvFileNoop(v interface{}) bool {
	switch e := v.(type) {
	case nil:
		return true
	case string:
		return e == ""
	case []interface{}:
		for _, item := range e {
			if s, ok := item.(string); ok && s == "" {
				continue
			}
			return false
		}
		return true
	}
	return false
}

// composeEntrypointNoop reports whether an entrypoint value is absent or
// an empty list.
func composeEntrypointNoop(v interface{}) bool {
	if v == nil {
		return true
	}
	if e, ok := v.([]interface{}); ok && len(e) == 0 {
		return true
	}
	return false
}

// composeDeployNoop reports whether a deploy block contains only Compose's
// own defaults ({} / replicas: 1 / mode: replicated) — values teploy's
// model already implies. Any other key or value changes deployment
// semantics and is refused.
func composeDeployNoop(m map[string]interface{}) bool {
	if len(m) == 0 {
		return true
	}
	for k, v := range m {
		switch k {
		case "replicas":
			if n, ok := v.(int); ok && n == 1 {
				continue
			}
			return false
		case "mode":
			if s, ok := v.(string); ok && s == "replicated" {
				continue
			}
			return false
		default:
			return false
		}
	}
	return true
}

// composeHealthcheckDisabled reports whether a healthcheck block is the
// Compose disabling form: disable: true, or test: ["NONE"] (which
// suppresses the image's inherited HEALTHCHECK). Both translate to
// ProcessHealth.Disable (--no-healthcheck) — the faithful home.
func composeHealthcheckDisabled(hc *composeHealthcheck) bool {
	if hc.Disable != nil && *hc.Disable {
		return true
	}
	if elems, ok := hc.Test.([]interface{}); ok && len(elems) == 1 {
		if s, ok := elems[0].(string); ok && s == "NONE" {
			return true
		}
	}
	return false
}

// translateComposeHealthcheck maps the web service's healthcheck test to
// teploy's deploy health gate: AppConfig.Health.Path (what URL path means
// healthy) and IntervalSeconds (poll cadence). The supported grammar is
// deliberately narrow (the ParsePublishSpec precedent): exec form
// ["CMD", "curl"|"wget", ...flags..., "http://localhost:<app-port>[/path]"]
// with the URL on localhost and the application port, plain http, bare
// path. timeout/retries/start_period are deliberately NOT translated —
// Compose's timeout is per-probe while teploy's health.timeout_seconds is
// the TOTAL deploy-gate window, and setting it from a per-probe value
// would break slow-starting apps; retries/start_period are subsumed by
// that total window. Everything else is refused naming the service.
func translateComposeHealthcheck(service string, hc *composeHealthcheck, appPort int) (string, int, error) {
	untranslatable := func(reason string) (string, int, error) {
		return "", 0, fmt.Errorf("compose service %q: 'healthcheck.test' %s — only the exec form [\"CMD\", \"curl\"|\"wget\", ..., \"http://localhost:<app-port>/<path>\"] translates to teploy's deploy health gate (teploy.yml 'health:'); remove it or write teploy.yml", service, reason)
	}
	elems, ok := hc.Test.([]interface{})
	if !ok {
		return untranslatable("shell/string form is not imported")
	}
	strs := make([]string, 0, len(elems))
	for _, e := range elems {
		s, ok := e.(string)
		if !ok {
			return untranslatable("contains a non-string element")
		}
		strs = append(strs, s)
	}
	if len(strs) < 2 || strs[0] != "CMD" {
		return untranslatable("is not an exec-form command")
	}
	if strs[1] != "curl" && strs[1] != "wget" {
		return untranslatable(fmt.Sprintf("command %q is not an HTTP probe", strs[1]))
	}
	var rawURL string
	for _, a := range strs[2:] {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			if rawURL != "" {
				return untranslatable("contains more than one URL")
			}
			rawURL = a
		}
	}
	if rawURL == "" {
		return untranslatable("contains no URL")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return untranslatable(fmt.Sprintf("URL %q is not absolute", rawURL))
	}
	if u.Scheme != "http" {
		return untranslatable(fmt.Sprintf("URL %q must be plain http — teploy's gate probes plain HTTP through the published port", rawURL))
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return untranslatable(fmt.Sprintf("URL host %q must be localhost/127.0.0.1/[::1] — the container probing itself", u.Hostname()))
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n != appPort {
			return untranslatable(fmt.Sprintf("probes port %s, not the application port %d", p, appPort))
		}
	} else if appPort != 80 {
		return untranslatable(fmt.Sprintf("URL %q has no explicit port but the application port is %d", rawURL, appPort))
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return untranslatable(fmt.Sprintf("URL %q must be a bare path (no query or fragment)", rawURL))
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	interval := 0
	if hc.Interval != nil {
		secs, err := composeDurationSeconds(hc.Interval)
		if err != nil || secs < 1 {
			return "", 0, fmt.Errorf("compose service %q: 'healthcheck.interval' must be a duration of whole seconds >= 1s — teploy's health.interval_seconds has no sub-second home; remove it or write teploy.yml", service)
		}
		interval = secs
	}
	return path, interval, nil
}

// composeDurationSeconds parses a Compose duration value: bare numbers
// ("30", 30) are seconds, strings may be Go-style durations ("30s",
// "1m30s"). Fractional results are rejected by the caller (no sub-second
// home).
func composeDurationSeconds(v interface{}) (int, error) {
	switch d := v.(type) {
	case int:
		return d, nil
	case int64:
		return int(d), nil
	case float64:
		return int(d), nil
	case string:
		s := strings.TrimSpace(d)
		if s == "" {
			return 0, fmt.Errorf("empty duration")
		}
		if n, err := strconv.Atoi(s); err == nil {
			return n, nil
		}
		dur, err := time.ParseDuration(s)
		if err != nil {
			return 0, err
		}
		secs := dur.Seconds()
		if secs != float64(int(secs)) {
			return 0, fmt.Errorf("%s is not a whole number of seconds", s)
		}
		return int(secs), nil
	}
	return 0, fmt.Errorf("unsupported duration value %v", v)
}

func sortedServiceNames(m map[string]composeService) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// composeAppPort resolves the web service's application container port
// from its Compose ports entries, returning the port and any non-TCP
// entries that must be preserved verbatim as publishes. The supported
// grammar is the same narrow grammar as ParsePublishSpec: short-form
// "[host:]container[/proto]" strings (or bare numbers) with single
// numeric ports. Long-form ports objects, ranges and multiple distinct
// container ports are refused naming the reason — never given arbitrary
// meaning. The Compose HOST-side binding is not part of the returned
// value: it is host plumbing that teploy replaces with its own
// allocation and Caddy routing. This is the ports entry of the importer's
// overall field classification — see LoadCompose and the classification
// table in compose_test.go.
func composeAppPort(service string, raw []interface{}) (int, []string, error) {
	seen := map[int]bool{}
	var extra []string
	for _, entry := range raw {
		var s string
		switch v := entry.(type) {
		case string:
			s = v
		case int:
			s = strconv.Itoa(v)
		default:
			return 0, nil, fmt.Errorf("compose service %q ports: unsupported ports entry %v — only short \"[host:]container[/proto]\" strings or bare numbers import; write teploy.yml for long-form Compose ports", service, entry)
		}
		spec, err := ParsePublishSpec(s)
		if err != nil {
			return 0, nil, fmt.Errorf("compose service %q ports: %w", service, err)
		}
		if spec.Proto != "" && spec.Proto != "tcp" {
			extra = append(extra, s)
			continue
		}
		seen[spec.ContainerPort] = true
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	switch len(ports) {
	case 1:
		return ports[0], extra, nil
	case 0:
		if len(extra) > 0 {
			return 0, nil, fmt.Errorf("compose service %q publishes only non-TCP ports — teploy serves HTTP over TCP and cannot pick an application port; write teploy.yml", service)
		}
		return 0, nil, fmt.Errorf("compose service %q publishes no usable TCP application port", service)
	default:
		var names []string
		for _, p := range ports {
			names = append(names, strconv.Itoa(p))
		}
		return 0, nil, fmt.Errorf("ambiguous compose import: service %q publishes multiple container ports (%s) — teploy routes one application port; remove the extra ports or write teploy.yml", service, strings.Join(names, ", "))
	}
}

func parseBuildContext(build interface{}) string {
	switch v := build.(type) {
	case string:
		return v
	case map[string]interface{}:
		if ctx, ok := v["context"]; ok {
			if s, ok := ctx.(string); ok {
				return s
			}
		}
	}
	return ""
}

// safeCommandArg matches exec-form command arguments that need no shell
// quoting (the same character set shlex-style quoters treat as safe).
var safeCommandArg = regexp.MustCompile(`^[a-zA-Z0-9_@%+=:,./-]+$`)

// parseCommand converts a Compose command to the process command string.
// String-form commands pass through unchanged. Exec-form (list) commands
// are joined with each argument shell-quoted when needed: the process
// command later runs through `sh -c` inside the container, so a plain
// space-join loses argument boundaries — `["sh","-c","printf 'hello
// world'"]` came out as three-plus tokens after the shell re-split it,
// and arguments containing spaces, quotes, or "$" were silently
// reinterpreted. Safe arguments stay bare, keeping simple commands
// readable ("npm run worker").
func parseCommand(cmd interface{}) string {
	switch v := cmd.(type) {
	case string:
		return v
	case []interface{}:
		parts := make([]string, len(v))
		for i, p := range v {
			arg := fmt.Sprintf("%v", p)
			if !safeCommandArg.MatchString(arg) {
				arg = ssh.ShellQuote(arg)
			}
			parts[i] = arg
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func parseEnvironment(env interface{}) map[string]string {
	result := make(map[string]string)
	switch v := env.(type) {
	case map[string]interface{}:
		for key, val := range v {
			result[key] = fmt.Sprintf("%v", val)
		}
	case []interface{}:
		for _, item := range v {
			s := fmt.Sprintf("%v", item)
			parts := strings.SplitN(s, "=", 2)
			if len(parts) == 2 {
				result[parts[0]] = parts[1]
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func parseServiceVolumes(vols []string) map[string]string {
	result := make(map[string]string)
	for _, v := range vols {
		parts := strings.SplitN(v, ":", 2)
		if len(parts) == 2 {
			// Named volume or host path → container path.
			name := parts[0]
			// Use the last path component as the volume name key.
			if strings.Contains(name, "/") {
				name = filepath.Base(name)
			}
			result[name] = parts[1]
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// imageBaseName extracts the short name from a Docker image reference.
// "postgres:16" -> "postgres", "library/postgres:16" -> "postgres",
// "registry.example:5000/postgres:16" -> "postgres": the tag colon is the
// one AFTER the last slash, so a registry host's port colon must not be
// treated as the tag separator (it used to be, which silently disabled
// accessory classification for any registry-hosted image).
func imageBaseName(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		image = image[:lastColon]
	}
	if i := strings.LastIndex(image, "/"); i >= 0 {
		image = image[i+1:]
	}
	return image
}

func isAccessoryImage(image string) bool {
	if image == "" {
		return false
	}
	_, ok := knownAccessoryImages[imageBaseName(image)]
	return ok
}

func accessoryPort(image string) int {
	return knownAccessoryImages[imageBaseName(image)]
}
