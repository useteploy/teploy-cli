package trigger

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// RepositoryIdentity is a repository authority, not a checkout locator. Native
// IDs must additionally be bound to a clone locator by authenticated metadata.
type RepositoryIdentity struct{ Forge, APIBase, ID string }

var nativeRepositoryID = regexp.MustCompile(`^id:[1-9][0-9]*$`)

func CanonicalAPIBase(raw string) (string, error) {
	if strings.ContainsAny(raw, "\x00\r\n\t ") {
		return "", fmt.Errorf("invalid API base")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("invalid API base")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("invalid API scheme")
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if host == "" {
		return "", fmt.Errorf("missing API host")
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("ambiguous API path")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func ParseRepositoryIdentity(raw string) (RepositoryIdentity, error) {
	var fields []string
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields) != 3 {
		return RepositoryIdentity{}, fmt.Errorf("repository_id must be a canonical three-string array")
	}
	b, _ := json.Marshal(fields)
	if string(b) != raw {
		return RepositoryIdentity{}, fmt.Errorf("noncanonical repository_id")
	}
	i := RepositoryIdentity{fields[0], fields[1], fields[2]}
	switch i.Forge {
	case "github", "gitlab", "gitea", "forgejo", "generic":
	default:
		return i, fmt.Errorf("unknown repository forge")
	}
	base, err := CanonicalAPIBase(i.APIBase)
	if err != nil || base != i.APIBase {
		return i, fmt.Errorf("noncanonical repository API base")
	}
	if i.Forge == "generic" {
		if !strings.HasPrefix(i.ID, "url:") {
			return i, fmt.Errorf("generic repository requires a locator")
		}
		u, err := url.Parse(strings.TrimPrefix(i.ID, "url:"))
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
			return i, fmt.Errorf("invalid generic locator")
		}
		origin, err := CanonicalAPIBase(u.Scheme + "://" + u.Host)
		if err != nil || origin != i.APIBase {
			return i, fmt.Errorf("generic locator origin mismatch")
		}
	} else if !nativeRepositoryID.MatchString(i.ID) {
		return i, fmt.Errorf("native repository ID must be an authenticated decimal ID")
	}
	return i, nil
}
