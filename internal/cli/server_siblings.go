package cli

// `teploy server siblings <server>`: the Programme B discovery read (plan
// section 10). Lists every CONTAINER-OBSERVED candidate sibling product on a
// server — teploy products by the dev.teploy.product label (their own or
// their image's), the forgejo family by its OCI image labels — with version
// hints and the published ports /.well-known/teploy.json might live behind.
//
// This command only OBSERVES: no DNS, no probing, no URL guessing, and never
// anything secret-adjacent (labels, ports and image refs only — no env, no
// credentials). Dash does the probing and confirming; credential minting and
// env writes need a confirmed instance_id plus admin consent, which is Dash's
// surface, not this one. Every remote command here only reads (a test pins
// that) and the whole observation costs at most two SSH round trips: the
// shared batched `docker ps`, then one batched inspect for port binds and
// image labels.
//
// An unrecognised dev.teploy.product value is passed through verbatim in
// unknown_labeled — a product newer than this CLI stays visible, and Dash
// decides what it knows.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/ssh"
)

// Label keys sibling detection reads. dev.teploy.* is the product contract
// (plan section 10.1); org.opencontainers.image.* is the OCI label set
// upstream images (forgejo, gitea) already carry.
const (
	labelTeployProduct = "dev.teploy.product"
	labelTeployVersion = "dev.teploy.version"
	ociLabelTitle      = "org.opencontainers.image.title"
	ociLabelSource     = "org.opencontainers.image.source"
	ociLabelVersion    = "org.opencontainers.image.version"
)

// Where a row's product came from. Stable vocabulary: consumers code
// against it (contracts/schema/siblings-envelope.schema.json).
const (
	// the container's own dev.teploy.product label
	siblingSourceLabel = "label"
	// the image's dev.teploy.product label, when the container lacks it
	siblingSourceImageLabel = "image_label"
	// the image's org.opencontainers.image.title/source names the forgejo
	// family (forgejo/forgejo or gitea)
	siblingSourceOCI = "oci_label"
)

// siblingProducts are the products this CLI recognises on sight: the teploy
// family plus forgejo (recognised through its OCI labels — upstream forgejo
// images do not carry the teploy label). Everything else with a
// dev.teploy.product value passes through verbatim in unknown_labeled.
var siblingProducts = map[string]bool{"dash": true, "observe": true, "ship": true, "forgejo": true}

const siblingsTimeout = 2 * time.Minute

// siblingPortHint is one published host bind: a hint where the product's
// /.well-known/teploy.json (or forgejo's /api/v1/version) MAY live. Loopback
// and tailnet/mesh binds are included as-is — a consumer on the same network
// reaches them.
type siblingPortHint struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Proto         string `json:"proto"`
}

// siblingDTO is one discovered candidate. Version and app are hints ("" when
// unknown); product and source are the detection verdict; port_hints carries
// every published bind docker reports for the container.
type siblingDTO struct {
	Container string            `json:"container"`
	Product   string            `json:"product"`
	Version   string            `json:"version,omitempty"`
	App       string            `json:"app,omitempty"`
	State     string            `json:"state"`
	Image     string            `json:"image"`
	PortHints []siblingPortHint `json:"port_hints"`
	Source    string            `json:"source"`
}

// serverSiblingsEnvelope is `server siblings --json`
// (contracts/schema/siblings-envelope.schema.json). State follows the audit
// vocabulary: a server that could not be listed is partial with empty arrays,
// never a clean "no siblings".
type serverSiblingsEnvelope struct {
	MachineInterface int          `json:"machine_interface"`
	Server           string       `json:"server"`
	Host             string       `json:"host"`
	ObservedAt       time.Time    `json:"observed_at"`
	State            string       `json:"state"`
	Error            string       `json:"error,omitempty"`
	Siblings         []siblingDTO `json:"siblings"`
	UnknownLabeled   []siblingDTO `json:"unknown_labeled"`
}

func newServerSiblingsCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "siblings <server>",
		Short: "Discover sibling teploy products and forgejo on a server (read-only)",
		Long: `Lists sibling product candidates on a server, as the containers and their
images report them: teploy products (dash, observe, ship) by the
dev.teploy.product label — the container's own, else its image's — and
forgejo by the OCI labels on its image. Each row carries the version hints
the labels provide, the teploy app name when the container is teploy-managed,
and the container's published ports as hints where its discovery endpoint may
live. A dev.teploy.product value this CLI does not recognise is passed
through verbatim under unknown_labeled.

Changes nothing and probes nothing: no DNS, no HTTP, no URL guessing, no
credentials or env — deciding which candidate is real (probing
/.well-known/teploy.json), and anything that follows from a confirmed
instance, is the caller's job. With --json the command exits 0 whenever the
envelope was produced; read state (ok / partial / unreachable).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerSiblings(flags, args[0], cmd.OutOrStdout())
		},
	}
}

func runServerSiblings(flags *Flags, server string, out io.Writer) error {
	host, user, key, err := config.ResolveServer(server, flags.Host, flags.User, flags.Key)
	if err != nil {
		return fmt.Errorf("resolving server %q: %w", server, err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, siblingsTimeout)
	defer stop()

	now := time.Now().UTC()
	var env serverSiblingsEnvelope
	dialCtx, dialCancel := context.WithTimeout(ctx, domainsConnectTimeout)
	exec, err := ssh.Connect(dialCtx, ssh.ConnectConfig{Host: host, User: user, KeyPath: key})
	dialCancel()
	if err != nil {
		env = serverSiblingsEnvelope{MachineInterface: MachineInterface, Server: server, Host: host, ObservedAt: now,
			State: auditStateUnreachable, Error: strings.Join(strings.Fields(err.Error()), " "),
			Siblings: []siblingDTO{}, UnknownLabeled: []siblingDTO{}}
	} else {
		defer exec.Close()
		env = collectServerSiblings(ctx, exec, server, host, now)
	}
	if flags.JSON {
		return json.NewEncoder(out).Encode(env)
	}
	return printServerSiblings(out, env)
}

func printServerSiblings(out io.Writer, env serverSiblingsEnvelope) error {
	if env.State == auditStateUnreachable {
		return fmt.Errorf("%s could not be observed: %s", env.Server, env.Error)
	}
	if len(env.Siblings) == 0 && len(env.UnknownLabeled) == 0 {
		fmt.Fprintf(out, "No sibling products found on %s\n", env.Server)
	} else {
		fmt.Fprintf(out, "Siblings on %s:\n", env.Server)
		for _, s := range env.Siblings {
			fmt.Fprintf(out, "  %-40s  %-8s  %-12s  %-12s %s\n", s.Container, s.Product, s.Version, s.App, describeSiblingHints(s))
		}
	}
	for _, s := range env.UnknownLabeled {
		fmt.Fprintf(out, "  %-40s  %-8s  (unrecognised dev.teploy.product, passed through)\n", s.Container, s.Product)
	}
	if env.State == auditStatePartial {
		fmt.Fprintf(out, "\nPartial observation: %s\n", env.Error)
	}
	return nil
}

func describeSiblingHints(s siblingDTO) string {
	var parts []string
	for _, h := range s.PortHints {
		parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", h.HostIP, h.HostPort, h.ContainerPort, h.Proto))
	}
	if len(parts) == 0 {
		return "no published ports"
	}
	return strings.Join(parts, ", ")
}

// collectServerSiblings performs the observation on one connected server.
// Split from runServerSiblings so the whole read surface is testable against
// a mock executor.
func collectServerSiblings(ctx context.Context, exec ssh.Executor, server, host string, now time.Time) serverSiblingsEnvelope {
	env := serverSiblingsEnvelope{MachineInterface: MachineInterface, Server: server, Host: host, ObservedAt: now,
		State: auditStateOK, Siblings: []siblingDTO{}, UnknownLabeled: []siblingDTO{}}
	client := docker.NewClient(exec)

	containers, err := client.ListAllContainers(ctx)
	if err != nil {
		// Nothing was observed: partial with empty arrays, never a clean
		// "no siblings".
		env.State, env.Error = auditStatePartial, err.Error()
		return env
	}
	if len(containers) == 0 {
		return env
	}

	// The one batched second round: published binds for every container,
	// labels for every distinct image (fallback detection needs the images
	// of unlabelled containers; version fallback and tag resolution need
	// the images of labelled ones — the union is every container's image,
	// deduplicated).
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	names := make([]string, 0, len(containers))
	imageSet := map[string]bool{}
	for _, c := range containers {
		names = append(names, c.Name)
		if c.Image != "" {
			imageSet[c.Image] = true
		}
	}
	images := make([]string, 0, len(imageSet))
	for ref := range imageSet {
		images = append(images, ref)
	}
	sort.Strings(images)

	var facts docker.SiblingInspection // zero value: empty maps, nil-safe on a failed second round
	if insp, err := client.InspectSiblings(ctx, names, images); err != nil {
		env.State, env.Error = auditStatePartial, err.Error()
	} else if len(names) > 0 && len(insp.Ports) == 0 {
		// Every named container produces an inspect line — even one that
		// publishes nothing yields its key with no binds. Zero keys means
		// the read failed wholesale (the || true swallowed it), which must
		// read as partial, not as "no ports, no image labels, all clean".
		env.State, env.Error = auditStatePartial, "docker inspect returned nothing"
	} else {
		facts = *insp
	}

	for _, c := range containers {
		imageFacts := facts.Images[c.Image]
		product, source, known := siblingProductOf(c, imageFacts)
		if !known {
			continue
		}
		row := siblingDTO{
			Container: c.Name,
			Product:   product,
			App:       c.Labels["teploy.app"],
			State:     c.State,
			Image:     resolveSiblingImage(c.Image, imageFacts),
			PortHints: []siblingPortHint{},
			Source:    source,
		}
		if row.Image == "" {
			row.Image = imageFacts.ID
		}
		row.Version = siblingVersionOf(c, imageFacts)
		for _, b := range facts.Ports[c.Name] {
			row.PortHints = append(row.PortHints, siblingPortHint{
				HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: b.ContainerPort, Proto: b.Proto,
			})
		}
		if siblingProducts[product] {
			env.Siblings = append(env.Siblings, row)
		} else {
			env.UnknownLabeled = append(env.UnknownLabeled, row)
		}
	}
	return env
}

// siblingProductOf decides what product a container is. known is false when
// the container is no sibling candidate at all (no teploy label anywhere and
// not forgejo). A dev.teploy.product value outside the recognised set is
// returned verbatim with known true: forward compatibility — the consumer
// decides what it knows.
func siblingProductOf(c docker.Container, imageFacts docker.ImageFacts) (product, source string, known bool) {
	if v := strings.TrimSpace(c.Labels[labelTeployProduct]); v != "" {
		return v, siblingSourceLabel, true
	}
	if v := strings.TrimSpace(imageFacts.Labels[labelTeployProduct]); v != "" {
		return v, siblingSourceImageLabel, true
	}
	if forgejoFromOCILabels(imageFacts.Labels) {
		return "forgejo", siblingSourceOCI, true
	}
	return "", "", false
}

// forgejoFromOCILabels reports whether an image's OCI labels name the
// forgejo family: upstream forgejo/gitea images carry no teploy label, but
// their org.opencontainers.image.source/title do name the project
// (codeberg.org/forgejo/forgejo, github.com/go-gitea/gitea). Substring
// matching is deliberately the rule here — this is a CANDIDATE for the
// consumer to probe (forgejo answers /api/v1/version), never a confirmed
// identity; a tool merely mentioning gitea in its source label costs one
// failed probe, not a wrong connection.
func forgejoFromOCILabels(labels map[string]string) bool {
	for _, v := range []string{labels[ociLabelTitle], labels[ociLabelSource]} {
		v = strings.ToLower(v)
		if strings.Contains(v, "forgejo/forgejo") || strings.Contains(v, "gitea") {
			return true
		}
	}
	return false
}

// siblingVersionOf picks the version hint, in priority order: the
// container's dev.teploy.version label, the teploy release label when the
// container belongs to a teploy app, then the image's
// org.opencontainers.image.version. "" when nothing says.
func siblingVersionOf(c docker.Container, imageFacts docker.ImageFacts) string {
	if v := strings.TrimSpace(c.Labels[labelTeployVersion]); v != "" {
		return v
	}
	if c.Labels["teploy.app"] != "" {
		if v := strings.TrimSpace(c.Labels["teploy.version"]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(imageFacts.Labels[ociLabelVersion])
}

// resolveSiblingImage maps docker ps's 12-hex image ID to the image's first
// repo tag when the facts hold one (A52 runs containers from the immutable
// ID); a name-form ref is returned unchanged. Best-effort: an untagged or
// missing image keeps the ID.
func resolveSiblingImage(psImage string, imageFacts docker.ImageFacts) string {
	if psImage == "" || !docker.IsImageID(psImage) || len(imageFacts.RepoTags) == 0 {
		return psImage
	}
	return imageFacts.RepoTags[0]
}
