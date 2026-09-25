package build

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultIgnore contains the ALWAYS-protected patterns: never uploaded to a
// build host, and a `!` allowlist line in .teployignore cannot re-include
// them. A custom .teployignore EXTENDS this list (audit T51): the old load
// replaced the defaults wholesale, so adding one harmless custom pattern
// silently shipped .env, .env.* and .git to the build host — where a broad
// Dockerfile COPY bakes them into the image.
//
// teploy's own config is protected too (L14): the remote build never reads
// it (the Dockerfile is the only build input, and autodeploy reads its own
// git checkout), and destination overlays (teploy.<dest>.yml) are where
// operators keep per-infra credentials — live on 2026-09-24 an overlay
// carrying the dash admin password was found world-readable in build dirs.
var DefaultIgnore = []string{
	"node_modules",
	".git",
	".env",
	".env.*",
	".teployignore",
	"/teploy.yml",
	"/teploy.yaml",
	"/teploy.toml",
	"teploy.*.yml",
	"teploy.*.yaml",
	"teploy.*.toml",
	".secrets",
	"secrets.yml",
	"secrets.yaml",
	"secrets.json",
	"secrets.toml",
	"secrets.env",
	"*.secrets.env",
}

// Rules is the parsed selection policy for one source directory: the
// protected defaults, the .teployignore excludes, and the .teployignore
// `!` allowlist (paths .gitignore hides that the build genuinely needs).
type Rules struct {
	Protected []string
	Excludes  []string
	Includes  []string

	protected []rule
	excludes  []rule
	includes  []rule
}

// LoadRules reads .teployignore from dir. A missing file yields the
// protected defaults alone; an UNREADABLE one is an error — the old load
// folded read failures into "no custom rules" and transferred with
// defaults silently.
//
// Line grammar (rsync-style patterns): blank lines and `#` comments are
// skipped; `!pattern` allowlists a gitignored path; anything else is an
// exclude. A leading `/` anchors to the source root, a trailing `/`
// matches directories only, `*` and `?` stay within one path segment and
// `**` crosses segments. A pattern without a `/` matches a name at any
// depth.
func LoadRules(dir string) (*Rules, error) {
	r := &Rules{Protected: append([]string(nil), DefaultIgnore...)}
	data, err := os.ReadFile(filepath.Join(dir, ".teployignore"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading .teployignore: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if inc, ok := strings.CutPrefix(line, "!"); ok {
			if inc = strings.TrimSpace(inc); inc != "" {
				r.Includes = append(r.Includes, inc)
			}
			continue
		}
		r.Excludes = append(r.Excludes, line)
	}
	for _, set := range []struct {
		pats []string
		dst  *[]rule
	}{{r.Protected, &r.protected}, {r.Excludes, &r.excludes}, {r.Includes, &r.includes}} {
		for _, p := range set.pats {
			compiled, err := compileRule(p)
			if err != nil {
				return nil, fmt.Errorf(".teployignore pattern %q: %w", p, err)
			}
			*set.dst = append(*set.dst, compiled)
		}
	}
	return r, nil
}

// IsProtected reports whether rel (slash-separated, relative to the source
// root) or any of its ancestor directories matches a protected pattern.
func (r *Rules) IsProtected(rel string, isDir bool) bool {
	return matchesAny(r.protected, rel, isDir)
}

// IsExcluded reports whether rel is protected or .teployignore-excluded.
func (r *Rules) IsExcluded(rel string, isDir bool) bool {
	return matchesAny(r.protected, rel, isDir) || matchesAny(r.excludes, rel, isDir)
}

// IsAllowlisted reports whether rel (or an ancestor) matches a `!` line.
func (r *Rules) IsAllowlisted(rel string, isDir bool) bool {
	return matchesAny(r.includes, rel, isDir)
}

// rule is one compiled rsync-style pattern.
type rule struct {
	re       *regexp.Regexp
	anchored bool
	dirOnly  bool
	hasSlash bool
	// literal is the pattern itself when it is an anchored path with no
	// glob metacharacters — lets an allowlist walk go straight to it.
	literal string
}

func compileRule(p string) (rule, error) {
	var r rule
	if strings.HasSuffix(p, "/") {
		r.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	if strings.HasPrefix(p, "/") {
		r.anchored = true
		p = strings.TrimLeft(p, "/")
	}
	if p == "" {
		return r, fmt.Errorf("empty pattern")
	}
	r.hasSlash = strings.Contains(p, "/") || strings.Contains(p, "**")
	if r.anchored && !strings.ContainsAny(p, "*?[\\") {
		r.literal = p
	}
	re, err := regexp.Compile("^" + globToRegexp(p) + "$")
	if err != nil {
		return r, err
	}
	r.re = re
	return r, nil
}

// globToRegexp translates an rsync-style glob: `**/` is zero or more
// directories, `**` anything, `*` and `?` stay within a segment, `[...]`
// is a character class (`[!...]` negated), `\x` escapes x.
func globToRegexp(p string) string {
	var sb strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				for i+1 < len(p) && p[i+1] == '*' {
					i++
				}
				if i+1 < len(p) && p[i+1] == '/' {
					sb.WriteString("(?:.*/)?")
					i++
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
			}
		case '?':
			sb.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				sb.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
		case '\\':
			if i+1 < len(p) {
				i++
				sb.WriteString(regexp.QuoteMeta(string(p[i])))
			} else {
				sb.WriteString(`\\`)
			}
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return sb.String()
}

// matchOne tests the pattern against one path s (an entry or ancestor).
func (r rule) matchOne(s string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if r.anchored {
		return r.re.MatchString(s)
	}
	if !r.hasSlash {
		return r.re.MatchString(path.Base(s))
	}
	// Unanchored with a slash: matches any trailing run of whole segments.
	for i := 0; ; {
		if r.re.MatchString(s[i:]) {
			return true
		}
		j := strings.IndexByte(s[i:], '/')
		if j < 0 {
			return false
		}
		i += j + 1
	}
}

// matchesAny reports whether rel or any ancestor directory of rel matches
// one of rules — excluding a directory excludes everything beneath it.
func matchesAny(rules []rule, rel string, isDir bool) bool {
	if len(rules) == 0 {
		return false
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			for _, r := range rules {
				if r.matchOne(rel[:i], true) {
					return true
				}
			}
		}
	}
	for _, r := range rules {
		if r.matchOne(rel, isDir) {
			return true
		}
	}
	return false
}
