package docker

// Second-round facts for sibling discovery (Programme B): every container's
// PUBLISHED port binds and every candidate image's config labels, fetched in
// ONE remote command so `teploy server siblings` costs at most two SSH round
// trips — the first being the shared batched `docker ps` of ListAllContainers
// (whose format already carries container labels). Read-only by construction:
// both reads are inspects.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/useteploy/teploy/internal/ssh"
)

// PortBinding is one published host bind of one container port, as docker
// inspect reports it: where the container's listener is reachable from the
// host's networks (0.0.0.0 = every interface, 127.0.0.1 = loopback only, a
// mesh/tailnet IP = that network only). Hints, not endpoints: the consumer
// probes and confirms.
type PortBinding struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Proto         string // "tcp" or "udp"
}

// ImageFacts is what one image's own metadata says: its config labels (the
// dev.teploy.* and org.opencontainers.image.* families) and its repo tags.
type ImageFacts struct {
	ID       string
	Labels   map[string]string
	RepoTags []string
}

// SiblingInspection is the batched second-round read. Ports is keyed by
// container name (docker inspect's .Name, leading slash stripped — the same
// form docker ps's .Names reports). Images is keyed BOTH by the refs the
// caller asked for (a name/tag or a short ID) and by each image's full
// sha256 ID, so a caller holding either form resolves.
type SiblingInspection struct {
	Ports  map[string][]PortBinding
	Images map[string]ImageFacts
}

// siblingsImageMarker separates the two reads inside one command. Both
// inspect formats start a line with a container name (/name) or an image ID
// (sha256:…), so the marker can never collide with a data line.
const siblingsImageMarker = "__TEPLOY_SIBLINGS_IMAGES__"

const (
	siblingsPortsLineFormat = `{{.Name}}` + "\t" + `{{json .NetworkSettings.Ports}}`
	siblingsImageLineFormat = `{{.Id}}` + "\t" + `{{json .RepoTags}}` + "\t" + `{{json .Config.Labels}}`
)

// InspectSiblings fetches, in ONE remote command, the published port binds
// for every named container and the labels + repo tags for every named
// image. Objects docker cannot find are skipped (absent from the result),
// the tolerance ResolveImageTags established: a container or image removed
// between the ps read and this one degrades the report, never fails it. A
// wholesale failure (docker down) surfaces as the caller's error — see the
// empty-result tripwire documented on the caller.
func (c *Client) InspectSiblings(ctx context.Context, names, images []string) (*SiblingInspection, error) {
	if len(names) == 0 && len(images) == 0 {
		return &SiblingInspection{Ports: map[string][]PortBinding{}, Images: map[string]ImageFacts{}}, nil
	}
	var b strings.Builder
	if len(names) > 0 {
		b.WriteString("docker inspect --format " + ssh.ShellQuote(siblingsPortsLineFormat))
		for _, n := range names {
			b.WriteString(" " + ssh.ShellQuote(n))
		}
		b.WriteString(" 2>/dev/null || true")
	}
	if len(images) > 0 {
		if b.Len() > 0 {
			b.WriteString("; printf '\\n" + siblingsImageMarker + "\\n'; ")
		}
		b.WriteString("docker image inspect --format " + ssh.ShellQuote(siblingsImageLineFormat))
		for _, i := range images {
			b.WriteString(" " + ssh.ShellQuote(i))
		}
		b.WriteString(" 2>/dev/null || true")
	}
	out, err := c.exec.Run(ctx, b.String())
	if err != nil {
		return nil, fmt.Errorf("inspecting sibling facts: %w", err)
	}
	return parseSiblingInspection(out, names, images)
}

// parseSiblingInspection splits the command output at the marker and decodes
// both halves. Malformed lines are skipped, not fatal: the surrounding
// command already swallows per-object errors, and one unparseable line must
// not cost the whole observation.
func parseSiblingInspection(output string, names, images []string) (*SiblingInspection, error) {
	portsHalf, imagesHalf := output, ""
	if i := strings.Index(output, "\n"+siblingsImageMarker+"\n"); i >= 0 {
		portsHalf = output[:i]
		imagesHalf = output[i+1+len(siblingsImageMarker)+1:]
	} else if strings.TrimSpace(output) == siblingsImageMarker {
		portsHalf, imagesHalf = "", ""
	} else if len(images) > 0 && len(names) == 0 {
		imagesHalf = output // ports-less command: everything is the image half
	}

	res := &SiblingInspection{Ports: map[string][]PortBinding{}, Images: map[string]ImageFacts{}}
	for _, line := range strings.Split(portsHalf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rawName, rawPorts, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(rawName, "/")
		var portMap map[string][]struct {
			HostIp   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		}
		if err := json.Unmarshal([]byte(rawPorts), &portMap); err != nil {
			continue
		}
		for spec, bindings := range portMap {
			port, proto, ok := splitPortSpec(spec)
			if !ok {
				continue
			}
			for _, b := range bindings {
				hostPort, err := strconv.Atoi(b.HostPort)
				if err != nil || hostPort <= 0 {
					continue
				}
				res.Ports[name] = append(res.Ports[name], PortBinding{
					HostIP: b.HostIp, HostPort: hostPort, ContainerPort: port, Proto: proto,
				})
			}
		}
		// docker's port map is a Go map on the wire: order it deterministically
		// so a pinned observation is reproducible. The container's key is
		// created even with no bindings — "inspect answered" and "inspect
		// never mentioned it" are different facts (the caller's wholesale-
		// failure tripwire depends on the distinction).
		if _, ok := res.Ports[name]; !ok {
			res.Ports[name] = nil
		}
		sort.Slice(res.Ports[name], func(i, j int) bool {
			a, b := res.Ports[name][i], res.Ports[name][j]
			if a.ContainerPort != b.ContainerPort {
				return a.ContainerPort < b.ContainerPort
			}
			if a.HostIP != b.HostIP {
				return a.HostIP < b.HostIP
			}
			if a.HostPort != b.HostPort {
				return a.HostPort < b.HostPort
			}
			return a.Proto < b.Proto
		})
	}

	var facts []ImageFacts
	for _, line := range strings.Split(imagesHalf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, rest, ok := strings.Cut(line, "\t")
		if !ok || !IsImageID(id) {
			continue
		}
		rawTags, rawLabels, _ := strings.Cut(rest, "\t")
		var tags []string
		if err := json.Unmarshal([]byte(rawTags), &tags); err != nil {
			continue
		}
		var labels map[string]string
		if err := json.Unmarshal([]byte(rawLabels), &labels); err != nil {
			continue
		}
		tags = slices.DeleteFunc(tags, func(t string) bool { return t == "" || t == "<none>:<none>" })
		facts = append(facts, ImageFacts{ID: id, Labels: labels, RepoTags: tags})
	}

	// Key by full ID and by every requested ref an image answers to: an
	// ID-form ref (docker ps's 12-hex short form) matches by prefix, a
	// name/tag ref matches its own RepoTags entry.
	for _, f := range facts {
		res.Images[f.ID] = f
	}
	for _, ref := range images {
		if _, done := res.Images[ref]; done {
			continue
		}
		for _, f := range facts {
			if imageRefMatches(ref, f) {
				res.Images[ref] = f
				break
			}
		}
	}
	return res, nil
}

// imageRefMatches reports whether docker image inspect's row f is the image
// the caller asked for by ref.
func imageRefMatches(ref string, f ImageFacts) bool {
	if IsImageID(ref) {
		return strings.HasPrefix(strings.TrimPrefix(f.ID, "sha256:"), strings.TrimPrefix(ref, "sha256:"))
	}
	return slices.Contains(f.RepoTags, ref)
}

// splitPortSpec splits docker's port key ("3000/tcp", "53/udp") into port
// and protocol.
func splitPortSpec(spec string) (int, string, bool) {
	portStr, proto, ok := strings.Cut(spec, "/")
	if !ok || (proto != "tcp" && proto != "udp") {
		return 0, "", false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return 0, "", false
	}
	return port, proto, true
}
