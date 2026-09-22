package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
	"gopkg.in/yaml.v3"
)

// composeFile represents the relevant parts of a docker-compose.yml.
type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string        `yaml:"image"`
	Build       interface{}   `yaml:"build"`       // string or struct
	Ports       []interface{} `yaml:"ports"`       // short-form strings or bare numbers; anything else is refused in composeAppPort
	Command     interface{}   `yaml:"command"`     // string or []string
	Environment interface{}   `yaml:"environment"` // map or list
	Volumes     []string      `yaml:"volumes"`
	DependsOn   interface{}   `yaml:"depends_on"` // list or map
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
	for name, svc := range compose.Services {
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
		webService = compose.Services[webServiceName]
	case len(webCandidates) > 1:
		return nil, fmt.Errorf("ambiguous compose import: multiple non-accessory services publish ports (%s) — teploy cannot pick the web service automatically; remove ports from the services that are not the app, or write teploy.yml manually", strings.Join(webCandidates, ", "))
	default:
		anyPorts := false
		for _, svc := range compose.Services {
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

	// Classify remaining services.
	var unsupportedBuilds []string
	for name, svc := range compose.Services {
		if name == webServiceName {
			continue
		}

		svcBuildContext := parseBuildContext(svc.Build)

		// Check if it's a known accessory image.
		if isAccessoryImage(svc.Image) {
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
			cfg.Processes[name] = parseCommand(svc.Command)
			continue
		}

		// No image and no build → skip unknown service.
		if svc.Image == "" && svcBuildContext == "" {
			continue
		}

		// Has a standalone image that isn't a known DB → treat as accessory.
		if svc.Image != "" {
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

// composeAppPort resolves the web service's application container port
// from its Compose ports entries, returning the port and any non-TCP
// entries that must be preserved verbatim as publishes. The supported
// grammar is the same narrow grammar as ParsePublishSpec: short-form
// "[host:]container[/proto]" strings (or bare numbers) with single
// numeric ports. Long-form ports objects, ranges and multiple distinct
// container ports are refused naming the reason — never given arbitrary
// meaning. The Compose HOST-side binding is not part of the returned
// value: it is host plumbing that teploy replaces with its own
// allocation and Caddy routing.
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
