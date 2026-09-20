package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultIgnore contains the ALWAYS-protected patterns excluded from every
// source sync. A custom .teployignore EXTENDS this list (audit T51): the
// old load replaced the defaults wholesale, so adding one harmless custom
// pattern silently shipped .env, .env.* and .git to the build host — where
// a broad Dockerfile COPY bakes them into the image.
var DefaultIgnore = []string{
	"node_modules",
	".git",
	".env",
	".env.*",
	".teployignore",
}

// LoadIgnore reads .teployignore from the given directory and returns the
// protected defaults MERGED with the user's patterns (defaults first, so
// they cannot be shadowed by ordering). A missing ignore file yields the
// defaults; an UNREADABLE one is an error — the old load folded read
// failures into "no custom rules" and transferred with defaults silently.
func LoadIgnore(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".teployignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultIgnore, nil
		}
		return nil, fmt.Errorf("reading .teployignore: %w", err)
	}

	patterns := append([]string(nil), DefaultIgnore...)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}
