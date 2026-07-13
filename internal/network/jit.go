package network

// JIT (just-in-time) scoped mesh access: time-boxed pre-auth keys that
// auto-revoke. "Grant the contractor 2 hours on the tailnet" — the key
// expires on its own, the node it enrolled is ephemeral (drops off when it
// disconnects), and what the node can REACH is decided by the tag you stamp
// it with plus your mesh ACL policy. Teploy mints and revokes the keys; it
// deliberately does not manage ACL policy files.
//
// Control-plane clients (HTTPS APIs, not SSH):
//   - Tailscale: api.tailscale.com, token from TAILSCALE_API_KEY (or
//     TS_API_KEY); tailnet from TAILSCALE_TAILNET, defaulting to "-" (the
//     token's own tailnet).
//   - Headscale: the self-hosted server's REST API, URL from the network
//     config (network.server in teploy.yml) or HEADSCALE_URL, token from
//     HEADSCALE_API_KEY (`headscale apikeys create`), user (namespace) from
//     HEADSCALE_USER.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Grant is a minted (or listed) time-boxed pre-auth key.
type Grant struct {
	ID        string
	Key       string // secret; only populated at creation
	Expires   time.Time
	Tags      []string
	Ephemeral bool
	Used      bool
}

// GrantClient mints, lists, and revokes JIT pre-auth keys against a mesh
// control plane.
type GrantClient interface {
	CreateGrant(ctx context.Context, ttl time.Duration, tags []string, reusable bool) (*Grant, error)
	ListGrants(ctx context.Context) ([]Grant, error)
	RevokeGrant(ctx context.Context, id string) error
}

// NewGrantClient builds the control-plane client for a provider. baseURL
// overrides the API endpoint (tests; Headscale server URL).
func NewGrantClient(provider, baseURL string) (GrantClient, error) {
	httpc := &http.Client{Timeout: 15 * time.Second}
	switch provider {
	case "tailscale":
		token := firstEnv("TAILSCALE_API_KEY", "TS_API_KEY")
		if token == "" {
			return nil, fmt.Errorf("tailscale API token not set — create one at https://login.tailscale.com/admin/settings/keys and export TAILSCALE_API_KEY")
		}
		tailnet := os.Getenv("TAILSCALE_TAILNET")
		if tailnet == "" {
			tailnet = "-" // the token's default tailnet
		}
		if baseURL == "" {
			baseURL = "https://api.tailscale.com"
		}
		return &tailscaleGrants{base: baseURL, tailnet: tailnet, token: token, client: httpc}, nil
	case "headscale":
		if baseURL == "" {
			baseURL = os.Getenv("HEADSCALE_URL")
		}
		if baseURL == "" {
			return nil, fmt.Errorf("headscale server URL not set — configure network.server in teploy.yml or export HEADSCALE_URL")
		}
		token := os.Getenv("HEADSCALE_API_KEY")
		if token == "" {
			return nil, fmt.Errorf("HEADSCALE_API_KEY not set — create one with `headscale apikeys create` on the headscale server")
		}
		user := os.Getenv("HEADSCALE_USER")
		if user == "" {
			return nil, fmt.Errorf("HEADSCALE_USER not set — the headscale user (namespace) the key enrolls into")
		}
		return &headscaleGrants{base: strings.TrimRight(baseURL, "/"), token: token, user: user, client: httpc}, nil
	case "netbird":
		return nil, fmt.Errorf("JIT grants are not implemented for netbird yet — use tailscale or headscale")
	default:
		return nil, fmt.Errorf("unknown network provider: %q", provider)
	}
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// --- Tailscale ---

type tailscaleGrants struct {
	base    string
	tailnet string
	token   string
	client  *http.Client
}

func (t *tailscaleGrants) CreateGrant(ctx context.Context, ttl time.Duration, tags []string, reusable bool) (*Grant, error) {
	body := map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      reusable,
					"ephemeral":     true,
					"preauthorized": true,
					"tags":          tags,
				},
			},
		},
		"expirySeconds": int(ttl.Seconds()),
	}
	var res struct {
		ID      string `json:"id"`
		Key     string `json:"key"`
		Expires string `json:"expires"`
	}
	if err := t.do(ctx, http.MethodPost, fmt.Sprintf("/api/v2/tailnet/%s/keys", t.tailnet), body, &res); err != nil {
		return nil, err
	}
	expires, _ := time.Parse(time.RFC3339, res.Expires)
	return &Grant{ID: res.ID, Key: res.Key, Expires: expires, Tags: tags, Ephemeral: true}, nil
}

func (t *tailscaleGrants) ListGrants(ctx context.Context) ([]Grant, error) {
	var res struct {
		Keys []struct {
			ID           string `json:"id"`
			Expires      string `json:"expires"`
			Capabilities *struct {
				Devices struct {
					Create struct {
						Ephemeral bool     `json:"ephemeral"`
						Tags      []string `json:"tags"`
					} `json:"create"`
				} `json:"devices"`
			} `json:"capabilities"`
		} `json:"keys"`
	}
	if err := t.do(ctx, http.MethodGet, fmt.Sprintf("/api/v2/tailnet/%s/keys", t.tailnet), nil, &res); err != nil {
		return nil, err
	}
	grants := make([]Grant, 0, len(res.Keys))
	for _, k := range res.Keys {
		g := Grant{ID: k.ID}
		g.Expires, _ = time.Parse(time.RFC3339, k.Expires)
		if k.Capabilities != nil {
			g.Ephemeral = k.Capabilities.Devices.Create.Ephemeral
			g.Tags = k.Capabilities.Devices.Create.Tags
		}
		grants = append(grants, g)
	}
	return grants, nil
}

func (t *tailscaleGrants) RevokeGrant(ctx context.Context, id string) error {
	return t.do(ctx, http.MethodDelete, fmt.Sprintf("/api/v2/tailnet/%s/keys/%s", t.tailnet, id), nil, nil)
}

func (t *tailscaleGrants) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("tailscale API %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- Headscale ---

type headscaleGrants struct {
	base   string
	token  string
	user   string
	client *http.Client
}

func (h *headscaleGrants) CreateGrant(ctx context.Context, ttl time.Duration, tags []string, reusable bool) (*Grant, error) {
	body := map[string]any{
		"user":       h.user,
		"reusable":   reusable,
		"ephemeral":  true,
		"aclTags":    tags,
		"expiration": time.Now().UTC().Add(ttl).Format(time.RFC3339),
	}
	var res struct {
		PreAuthKey struct {
			ID         string `json:"id"`
			Key        string `json:"key"`
			Expiration string `json:"expiration"`
		} `json:"preAuthKey"`
	}
	if err := h.do(ctx, http.MethodPost, "/api/v1/preauthkey", body, &res); err != nil {
		return nil, err
	}
	expires, _ := time.Parse(time.RFC3339, res.PreAuthKey.Expiration)
	return &Grant{ID: res.PreAuthKey.ID, Key: res.PreAuthKey.Key, Expires: expires, Tags: tags, Ephemeral: true}, nil
}

func (h *headscaleGrants) ListGrants(ctx context.Context) ([]Grant, error) {
	var res struct {
		PreAuthKeys []struct {
			ID         string   `json:"id"`
			Key        string   `json:"key"`
			Expiration string   `json:"expiration"`
			Ephemeral  bool     `json:"ephemeral"`
			Used       bool     `json:"used"`
			ACLTags    []string `json:"aclTags"`
		} `json:"preAuthKeys"`
	}
	if err := h.do(ctx, http.MethodGet, "/api/v1/preauthkey?user="+h.user, nil, &res); err != nil {
		return nil, err
	}
	grants := make([]Grant, 0, len(res.PreAuthKeys))
	for _, k := range res.PreAuthKeys {
		g := Grant{ID: k.ID, Ephemeral: k.Ephemeral, Used: k.Used, Tags: k.ACLTags}
		g.Expires, _ = time.Parse(time.RFC3339, k.Expiration)
		grants = append(grants, g)
	}
	return grants, nil
}

func (h *headscaleGrants) RevokeGrant(ctx context.Context, id string) error {
	// Headscale expires keys rather than deleting them; expired = revoked.
	body := map[string]any{"user": h.user, "key": id}
	return h.do(ctx, http.MethodPost, "/api/v1/preauthkey/expire", body, nil)
}

func (h *headscaleGrants) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("headscale API %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
