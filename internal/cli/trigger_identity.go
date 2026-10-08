package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/state"
	"github.com/useteploy/teploy/internal/trigger"
)

// canonicalRepository records the forge origin and canonical repository path,
// with no authentication material. API origins are supplied by the source
// authority; a checkout can prove their repository mapping, not API credentials.
func canonicalRepository(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		if !ok {
			return "", fmt.Errorf("invalid repository origin")
		}
		raw = "https://" + host + "/" + path
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("repository requires a canonical forge origin and path")
	}
	if u.Scheme == "ssh" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("unsupported repository origin")
	}
	u.User = nil
	u.Scheme = strings.ToLower(u.Scheme)
	origin, err := trigger.CanonicalAPIBase(u.Scheme + "://" + u.Host)
	if err != nil {
		return "", err
	}
	normalized, _ := url.Parse(origin)
	u.Host = normalized.Host
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	u.RawPath = ""
	if u.Path == "" || strings.Contains(u.Path, "/../") || strings.Contains(u.Path, "/./") {
		return "", fmt.Errorf("invalid repository identity")
	}
	return u.String(), nil
}
func checkoutRepository(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return "", err
	}
	return canonicalRepository(string(out))
}
func checkoutRef(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "-q", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
func verifyCheckoutRequest(ctx context.Context, r trigger.Request) error {
	repo, err := checkoutRepository(".")
	if err != nil {
		return err
	}
	if err := verifyRepositoryMapping(ctx, r.RepositoryID, repo, http.DefaultTransport); err != nil {
		return err
	}
	// Detached checkouts carry no moving branch authority. Their exact ref is
	// supplied by the authenticated producer and their full commit is re-proven.
	ref, err := checkoutRef(".")
	if err == nil && ref != r.Ref {
		return fmt.Errorf("checkout ref differs from admitted exact ref")
	}
	return nil
}

// Native repository proof is read from an explicitly configured authenticated
// API. A caller cannot redirect API credentials by changing repository_id.
// The same resolver must run before fetch in automatic checkout producers.
func verifyRepositoryMapping(ctx context.Context, identity, clone string, transport http.RoundTripper) error {
	i, err := trigger.ParseRepositoryIdentity(identity)
	if err != nil {
		return err
	}
	if i.Forge == "generic" {
		locator, err := canonicalRepository(strings.TrimPrefix(i.ID, "url:"))
		if err != nil || locator != strings.TrimPrefix(i.ID, "url:") || locator != clone {
			return fmt.Errorf("checkout differs from generic repository locator")
		}
		return nil
	}
	configuredBase, err := trigger.CanonicalAPIBase(os.Getenv("TEPLOY_TRIGGER_API_BASE"))
	if err != nil || configuredBase != i.APIBase || os.Getenv("TEPLOY_TRIGGER_FORGE") != i.Forge {
		return fmt.Errorf("native repository requires configured API authority")
	}
	if strings.HasPrefix(i.APIBase, "http:") && os.Getenv("TEPLOY_TRIGGER_ALLOW_HTTP") != "1" {
		return fmt.Errorf("private HTTP API authority not admitted")
	}
	token := os.Getenv("TEPLOY_TRIGGER_API_TOKEN")
	if token == "" {
		return fmt.Errorf("native repository requires authenticated metadata")
	}
	var endpoint string
	switch i.Forge {
	case "github":
		endpoint = "/repositories/" + strings.TrimPrefix(i.ID, "id:")
	case "gitlab":
		endpoint = "/projects/" + strings.TrimPrefix(i.ID, "id:")
	default:
		return fmt.Errorf("native repository metadata resolver not implemented for configured forge")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.APIBase+endpoint, nil)
	if err != nil {
		return fmt.Errorf("invalid repository metadata request")
	}
	if i.Forge == "gitlab" {
		req.Header.Set("PRIVATE-TOKEN", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("authenticated repository metadata unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("authenticated repository metadata refused")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return fmt.Errorf("repository metadata exceeds bound or unreadable")
	}
	var metadata struct {
		ID       json.Number `json:"id"`
		CloneURL string      `json:"clone_url"`
		HTTPURL  string      `json:"http_url_to_repo"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil || "id:"+metadata.ID.String() != i.ID {
		return fmt.Errorf("authenticated native repository identity mismatch")
	}
	locator := metadata.CloneURL
	if i.Forge == "gitlab" {
		locator = metadata.HTTPURL
	}
	actual, err := canonicalRepository(locator)
	if err != nil || actual != clone {
		return fmt.Errorf("authenticated API repository does not map to checkout origin")
	}
	return nil
}
func newTriggerCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{Use: "trigger", Short: "Inspect immutable trigger admission identity"}
	var image string
	identity := &cobra.Command{Use: "identity", Args: cobra.NoArgs, Short: "Enroll target and read the opaque execution binding", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadApp(".")
		if err != nil {
			return err
		}
		if err = resolveDeployEnv(cmd.Context(), cfg, flags.StrictEnv); err != nil {
			return err
		}
		if image == "" {
			image = cfg.Image
		}
		exec, err := connectForApp(cmd.Context(), flags, cfg)
		if err != nil {
			return err
		}
		defer exec.Close()
		if err = docker.NewClient(exec).EnsureManagedDirectory(cmd.Context(), cfg.App, "", ""); err != nil {
			return err
		}
		lock, err := state.AcquireLockFenced(cmd.Context(), exec, cfg.App)
		if err != nil {
			return err
		}
		defer state.ReleaseLockFenced(exec, lock, cfg.App)
		lock.StartRenewal(exec)
		fenced := &state.FencedExecutor{Executor: exec, Lock: lock}
		target, err := trigger.EnrolledTarget(cmd.Context(), fenced, cfg.App)
		if err != nil {
			return err
		}
		binding, err := TriggerBindingDigest(cfg, image, target)
		if err != nil {
			return err
		}
		actual, err := state.Read(cmd.Context(), fenced, cfg.App)
		if err != nil {
			return err
		}
		// A successful absence read is explicit; no generation is synthesized.
		var generation *uint64
		if actual != nil {
			gen := actual.Generation
			generation = &gen
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			SchemaVersion int     `json:"schema_version"`
			TargetID      string  `json:"target_id"`
			Binding       string  `json:"execution_binding_digest"`
			Generation    *uint64 `json:"generation,omitempty"`
		}{1, target, binding, generation})
	}}
	identity.Flags().StringVar(&image, "image", "", "image reference used by the execution binding (empty for source build)")
	cmd.AddCommand(identity)
	return cmd
}
