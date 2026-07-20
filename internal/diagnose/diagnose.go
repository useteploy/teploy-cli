// Package diagnose turns the facts gathered at a failed deploy into a
// human-actionable diagnosis. Rules are deterministic and run entirely
// locally — no AI, no network. Every field of Context may be missing
// (zero value); every rule must tolerate that, and wording stays
// suggestive ("likely", "try") rather than certain.
package diagnose

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Listener is one TCP listening socket observed inside the container.
type Listener struct {
	Port         int
	LoopbackOnly bool // bound to 127.0.0.1/::1 — unreachable from outside the container
}

// Context carries everything known about a failed deploy at the moment of
// failure.
type Context struct {
	Stage          string // "start" | "hook" | "health" | ""
	Err            error
	State          string // docker State.Status: running, exited, ...
	ExitCode       int    // State.ExitCode; -1 when unknown
	OOMKilled      bool
	Logs           string     // recent container logs
	ConfiguredPort int        // port: from teploy.yml (0 when unknown)
	Listeners      []Listener // nil = could not observe; empty = observed none
	ListenersKnown bool       // true when Listeners reflects a real observation
}

// Finding is one diagnosis with concrete next steps.
type Finding struct {
	Summary string
	Try     []string
}

var (
	envVarPattern = regexp.MustCompile(`(?i)(?:missing|required|not set|undefined|unset)[^\n]{0,40}?(?:env(?:ironment)?(?: variable)?)[:\s]*['"]?([A-Z][A-Z0-9_]{2,})?|(?:env(?:ironment)? variable )['"]?([A-Z][A-Z0-9_]{2,})['"]? (?:is )?(?:missing|required|not set|undefined|unset)`)
	dbConnPattern = regexp.MustCompile(`(?i)(connection refused|ECONNREFUSED|could not connect|connect: no route|getaddrinfo|no such host)[^\n]{0,80}?(postgres|mysql|redis|mongo|database|db|5432|3306|6379|27017)|(postgres|mysql|redis|mongo)[^\n]{0,60}?(connection refused|ECONNREFUSED|could not connect)`)
	diskPattern   = regexp.MustCompile(`(?i)no space left on device`)
	permPattern   = regexp.MustCompile(`(?i)permission denied`)
)

// Diagnose runs the rule table over the context and returns the findings,
// most-specific first, capped at two so the output stays readable.
func Diagnose(c Context) []Finding {
	var findings []Finding
	add := func(f *Finding) {
		if f != nil {
			findings = append(findings, *f)
		}
	}

	add(oomRule(c))
	add(diskFullRule(c))
	add(exitedRule(c))
	add(portMismatchRule(c))
	add(loopbackRule(c))
	add(nothingListeningRule(c))
	add(permissionRule(c))

	if len(findings) > 2 {
		findings = findings[:2]
	}
	return findings
}

func oomRule(c Context) *Finding {
	if !c.OOMKilled {
		return nil
	}
	return &Finding{
		Summary: "the container was killed by the kernel OOM killer — it ran out of memory",
		Try: []string{
			"raise (or remove) the `memory:` limit in teploy.yml",
			"check the app for a memory spike on boot (migrations, caches)",
		},
	}
}

func diskFullRule(c Context) *Finding {
	text := c.Logs
	if c.Err != nil {
		text += "\n" + c.Err.Error()
	}
	if !diskPattern.MatchString(text) {
		return nil
	}
	return &Finding{
		Summary: "the server is out of disk space",
		Try: []string{
			"free space on the server: `docker system prune -a` removes unused images",
			"prune old backups (`teploy backup prune`) and check big volumes with `df -h`",
		},
	}
}

func exitedRule(c Context) *Finding {
	if c.State != "exited" {
		return nil
	}
	// Specific log signatures first.
	if m := envVarPattern.FindStringSubmatch(c.Logs); m != nil {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		f := &Finding{Summary: "the app exited because a required environment variable is missing"}
		if name != "" {
			f.Summary = fmt.Sprintf("the app exited because the environment variable %s is missing", name)
			f.Try = append(f.Try, fmt.Sprintf("set it: add `%s` under `env:` in teploy.yml, or `teploy secret set %s=...` and reference it as `secret:%s`", name, name, name))
		} else {
			f.Try = append(f.Try, "add the variable under `env:` in teploy.yml, or use `teploy secret set` for sensitive values")
		}
		return f
	}
	if dbConnPattern.MatchString(c.Logs) {
		return &Finding{
			Summary: "the app exited because it could not reach its database",
			Try: []string{
				"if the DB runs as a teploy accessory, use the accessory name as the host (containers share the teploy network)",
				"check credentials — `secret:KEY` refs must exist (`teploy secret list`)",
				"confirm the accessory is running: `teploy accessory status <name>`",
			},
		}
	}
	switch c.ExitCode {
	case 126, 127:
		return &Finding{
			Summary: fmt.Sprintf("the container command could not be executed (exit %d) — the image's entrypoint/command is wrong or the binary is missing", c.ExitCode),
			Try: []string{
				"check the Dockerfile CMD/ENTRYPOINT (a musl/glibc mismatch also lands here)",
			},
		}
	}
	f := &Finding{
		Summary: "the app crashed on boot — the container logs above have the reason",
	}
	if c.ExitCode > 0 {
		f.Summary = fmt.Sprintf("the app crashed on boot with exit code %d — the container logs above have the reason", c.ExitCode)
	}
	f.Try = append(f.Try, "run the image locally to reproduce: `docker run --rm <image>`")
	return f
}

func portMismatchRule(c Context) *Finding {
	if c.State != "running" || !c.ListenersKnown || c.ConfiguredPort <= 0 {
		return nil
	}
	var reachable []int
	for _, l := range c.Listeners {
		if l.Port == c.ConfiguredPort {
			return nil // configured port is listening; not a mismatch
		}
		if !l.LoopbackOnly {
			reachable = append(reachable, l.Port)
		}
	}
	if len(reachable) == 0 {
		return nil // covered by loopback / nothing-listening rules
	}
	ports := make([]string, len(reachable))
	for i, p := range reachable {
		ports[i] = strconv.Itoa(p)
	}
	return &Finding{
		Summary: fmt.Sprintf("the app is listening on port %s, but teploy.yml says `port: %d`", strings.Join(ports, ", "), c.ConfiguredPort),
		Try: []string{
			fmt.Sprintf("set `port: %d` in teploy.yml (or make the app listen on %d, e.g. via a PORT env var)", reachable[0], c.ConfiguredPort),
		},
	}
}

func loopbackRule(c Context) *Finding {
	if c.State != "running" || !c.ListenersKnown {
		return nil
	}
	for _, l := range c.Listeners {
		if l.Port == c.ConfiguredPort && l.LoopbackOnly {
			return &Finding{
				Summary: fmt.Sprintf("the app binds 127.0.0.1 inside the container — port %d is unreachable from outside it", l.Port),
				Try: []string{
					"make the app listen on 0.0.0.0 (many frameworks default to localhost; e.g. HOST=0.0.0.0 or --host 0.0.0.0)",
				},
			}
		}
	}
	return nil
}

func nothingListeningRule(c Context) *Finding {
	if c.State != "running" || !c.ListenersKnown || len(c.Listeners) > 0 {
		return nil
	}
	return &Finding{
		Summary: "the container is running but nothing is listening on any TCP port yet",
		Try: []string{
			"if the app boots slowly (migrations, JIT warmup), raise the health timeout in teploy.yml (`health: { timeout: 90s }`)",
			"confirm the process actually starts an HTTP server (worker-only images should be a `processes:` entry, not the web process)",
		},
	}
}

func permissionRule(c Context) *Finding {
	if !permPattern.MatchString(c.Logs) {
		return nil
	}
	return &Finding{
		Summary: "the logs show a permission error — often a volume owned by a different UID than the container user",
		Try: []string{
			"chown the volume path on the server to the image's runtime UID, or run the container as the owning user",
		},
	}
}

// ParseListeners parses `ss -tlnH` or `netstat -tln` output from inside a
// container into listeners. ok is false when the output carries no usable
// evidence (tool missing / empty) — callers must then treat the listener
// set as unknown, not as "nothing is listening".
func ParseListeners(out string) (listeners []Listener, ok bool) {
	seen := map[int]*Listener{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// ss:      LISTEN 0 4096 0.0.0.0:8080 ...      (with -H, state first)
		// netstat: tcp 0 0 0.0.0.0:8080 0.0.0.0:* LISTEN
		if !strings.Contains(strings.ToUpper(line), "LISTEN") && fields[0] != "tcp" && fields[0] != "tcp6" {
			continue
		}
		for _, f := range fields {
			idx := strings.LastIndex(f, ":")
			if idx <= 0 || idx == len(f)-1 {
				continue
			}
			port, err := strconv.Atoi(f[idx+1:])
			if err != nil || port <= 0 || port > 65535 {
				continue
			}
			addr := f[:idx]
			loopback := addr == "127.0.0.1" || addr == "::1" || addr == "[::1]"
			if existing, dup := seen[port]; dup {
				// A port bound on both loopback and a public address is reachable.
				if !loopback {
					existing.LoopbackOnly = false
				}
			} else {
				l := &Listener{Port: port, LoopbackOnly: loopback}
				seen[port] = l
			}
			ok = true
			break // first host:port field per line is the local address
		}
	}
	for _, l := range seen {
		listeners = append(listeners, *l)
	}
	return listeners, ok
}
