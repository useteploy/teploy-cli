// Package cloud is the HTTP client for the Teploy Cloud public API
// (/api/v1): managed servers, the provider catalogue and SSH keys.
//
// The API key is only ever placed in the Authorization header. It is never
// logged, printed, or included in an error string.
package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is the hosted Teploy Cloud.
const DefaultBaseURL = "https://cloud.teploy.com"

// BaseURLEnv overrides the base URL (e.g. a local instance).
const BaseURLEnv = "TEPLOY_CLOUD_URL"

// RequestTimeout bounds every request.
const RequestTimeout = 30 * time.Second

const maxResponseBytes = 8 << 20

// Server is a managed server as the API returns it.
type Server struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	IPAddress         string `json:"ipAddress"`
	Status            string `json:"status"`
	Provider          string `json:"provider"`
	ProviderRegion    string `json:"providerRegion"`
	ProviderSize      string `json:"providerSize"`
	PriceMonthlyCents int64  `json:"priceMonthlyCents"`
}

// Provider is one entry of the provider catalogue.
type Provider struct {
	Type             string `json:"type"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	ManagedAvailable bool   `json:"managedAvailable"`
}

// Region is a provider region.
type Region struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Country   string `json:"country"`
	Continent string `json:"continent"`
	Available bool   `json:"available"`
}

// Size is a provider plan with the price Teploy Cloud charges for it.
type Size struct {
	ID                string   `json:"id"`
	Slug              string   `json:"slug"`
	Name              string   `json:"name"`
	VCPUs             int      `json:"vcpus"`
	Memory            int      `json:"memory"` // MB
	Disk              int      `json:"disk"`   // GB
	Bandwidth         float64  `json:"bandwidth"`
	PriceMonthlyCents int64    `json:"priceMonthlyCents"`
	Available         bool     `json:"available"`
	Regions           []string `json:"regions,omitempty"`
}

// SSHKey is an SSH public key registered with the organization.
type SSHKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
}

// CreateServerRequest is the POST /api/v1/servers body.
type CreateServerRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Region   string `json:"region"`
	Size     string `json:"size"`
	SSHKeyID string `json:"sshKeyId"`
}

// APIError is a failure envelope ({"success":false,"error":{...}}) or a
// non-2xx response without one.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	switch e.Code {
	case "trust_limit", "platform_capacity":
		return e.Message
	}
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	s := fmt.Sprintf("teploy cloud: %s (code %s, HTTP %d)", msg, e.Code, e.Status)
	if hint := e.Hint(); hint != "" {
		s += "\n" + hint
	}
	return s
}

// Hint is the next step for the failures a user can fix themselves.
func (e *APIError) Hint() string {
	low := strings.ToLower(e.Message)
	switch {
	case e.Status == http.StatusUnauthorized:
		return "Run `teploy cloud login` with a valid API key."
	case e.Status == http.StatusPaymentRequired || strings.Contains(low, "payment method"):
		return "Add a card at " + DefaultBaseURL + " and try again."
	}
	return ""
}

// Client talks to one Teploy Cloud organization.
type Client struct {
	baseURL string
	key     string
	orgID   string
	http    *http.Client
}

// ResolveBaseURL picks the base URL: $TEPLOY_CLOUD_URL, else stored, else
// the default, and validates it.
func ResolveBaseURL(stored string) (string, error) {
	u := strings.TrimSpace(os.Getenv(BaseURLEnv))
	if u == "" {
		u = strings.TrimSpace(stored)
	}
	if u == "" {
		u = DefaultBaseURL
	}
	return ValidateBaseURL(u)
}

// ValidateBaseURL refuses anything but https, except plain http to
// localhost or 127.0.0.1. It returns the URL without a trailing slash.
func ValidateBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid Teploy Cloud URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if h != "localhost" && h != "127.0.0.1" {
			return "", fmt.Errorf("refusing non-https Teploy Cloud URL %q (plain http is allowed only for localhost and 127.0.0.1)", raw)
		}
	default:
		return "", fmt.Errorf("refusing Teploy Cloud URL %q: scheme must be https", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("refusing Teploy Cloud URL with embedded credentials")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// New returns a client. baseURL must already be validated or it is
// validated here.
func New(baseURL, key, orgID string) (*Client, error) {
	base, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("teploy cloud: API key is empty")
	}
	if strings.TrimSpace(orgID) == "" {
		return nil, errors.New("teploy cloud: organization id is empty")
	}
	return &Client{
		baseURL: base,
		key:     strings.TrimSpace(key),
		orgID:   strings.TrimSpace(orgID),
		http:    &http.Client{Timeout: RequestTimeout},
	}, nil
}

// BaseURL is the validated base URL the client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// NewIdempotencyKey returns a random key for one logical create.
func NewIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating idempotency key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, headers map[string]string, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("X-Organization-Id", c.orgID)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error carries the method and URL, never headers.
		return fmt.Errorf("teploy cloud: %s %s: %w", method, path, unwrapURLError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("teploy cloud: reading response to %s %s: %w", method, path, err)
	}

	var env envelope
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || (jsonErr == nil && !env.Success) {
		ae := &APIError{Status: resp.StatusCode}
		if jsonErr == nil && env.Error != nil {
			ae.Code, ae.Message = env.Error.Code, env.Error.Message
		}
		if ae.Code == "" {
			ae.Code = "unknown"
		}
		return ae
	}
	if jsonErr != nil {
		return fmt.Errorf("teploy cloud: %s %s: response is not a JSON envelope", method, path)
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("teploy cloud: decoding %s %s: %w", method, path, err)
	}
	return nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// decodeList accepts "data" as an array or as {"items":[...],"total":n}.
func decodeList[T any](raw json.RawMessage) ([]T, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return []T{}, nil
	}
	var items []T
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
	} else {
		var wrapped struct {
			Items []T `json:"items"`
		}
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			return nil, err
		}
		items = wrapped.Items
	}
	if items == nil {
		items = []T{}
	}
	return items, nil
}

func list[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return nil, err
	}
	items, err := decodeList[T](raw)
	if err != nil {
		return nil, fmt.Errorf("teploy cloud: decoding GET %s: %w", path, err)
	}
	return items, nil
}

// ListServers returns the organization's servers.
func (c *Client) ListServers(ctx context.Context) ([]Server, error) {
	return list[Server](ctx, c, "/api/v1/servers")
}

// GetServer returns one server.
func (c *Client) GetServer(ctx context.Context, id string) (*Server, error) {
	var s Server
	if err := c.do(ctx, http.MethodGet, "/api/v1/servers/"+url.PathEscape(id), nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateServer provisions a server. idempotencyKey must be the same on
// every retry of one logical create.
func (c *Client) CreateServer(ctx context.Context, r CreateServerRequest, idempotencyKey string) (*Server, error) {
	var s Server
	h := map[string]string{"Idempotency-Key": idempotencyKey}
	if err := c.do(ctx, http.MethodPost, "/api/v1/servers", r, h, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteServer destroys a server.
func (c *Client) DeleteServer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/servers/"+url.PathEscape(id), nil, nil, nil)
}

// ListProviders returns the provider catalogue.
func (c *Client) ListProviders(ctx context.Context) ([]Provider, error) {
	return list[Provider](ctx, c, "/api/v1/providers")
}

// ListRegions returns the regions a provider offers.
func (c *Client) ListRegions(ctx context.Context, provider string) ([]Region, error) {
	return list[Region](ctx, c, "/api/v1/providers/"+url.PathEscape(provider)+"/regions")
}

// ListSizes returns the sizes a provider offers, optionally in one region.
func (c *Client) ListSizes(ctx context.Context, provider, region string) ([]Size, error) {
	p := "/api/v1/providers/" + url.PathEscape(provider) + "/sizes"
	if region != "" {
		p += "?region=" + url.QueryEscape(region)
	}
	return list[Size](ctx, c, p)
}

// ListSSHKeys returns the organization's registered SSH keys.
func (c *Client) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	return list[SSHKey](ctx, c, "/api/v1/ssh-keys")
}

// CreateSSHKey registers a public key.
func (c *Client) CreateSSHKey(ctx context.Context, name, publicKey string) (*SSHKey, error) {
	var k SSHKey
	body := map[string]string{"name": name, "publicKey": publicKey}
	if err := c.do(ctx, http.MethodPost, "/api/v1/ssh-keys", body, nil, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

// IsRetryable reports whether a failed create can be retried with the same
// idempotency key: a transport failure or a 5xx response.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return !errors.Is(err, context.Canceled)
}
