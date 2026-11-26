// Package sandbox is the HTTP client for one teploy-sandbox daemon (the
// sandbox engine). It speaks the wire contract documented in that
// repository's docs/wire-contract.md, which teploy-ship speaks as well:
// plain HTTP with a bearer token, the same routes and the same answers.
//
// Nothing here decides how the bytes travel. The caller supplies the dial
// function: net/http's own, or (in the teploy CLI) an SSH channel to the
// engine's address on the server.
//
// The bearer token is only ever placed in the Authorization header. It is
// never logged, printed, or included in an error string.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RequestTimeout bounds a read: health, capacity, the run list.
const RequestTimeout = 30 * time.Second

// MutationTimeout bounds a create, stop, resume, fork, destroy or file
// transfer. Each can pull, commit or delete an image on the server, which a
// large one makes slow; the caller's context (Ctrl-C) still ends the wait.
const MutationTimeout = 10 * time.Minute

// MaxFileBytes is the engine's bound on one file in either direction.
const MaxFileBytes = 64 << 20

// MaxTTL is the longest a run may live: the engine's bound on a create's,
// resume's or fork's ttlSec. An exec's timeout has no such bound.
const MaxTTL = 24 * time.Hour

// DefaultExecTimeoutSec is the engine's timeout for an exec that names none.
const DefaultExecTimeoutSec = 120

// execHeaderGrace is how long the engine may take to answer an exec beyond
// the command's own timeout. Its 200 goes out with the first output, so a
// command that prints nothing for its whole timeout is answered at its exit.
// A variable for the one test that shortens it.
var execHeaderGrace = 30 * time.Second

// maxJSONBytes bounds every JSON answer (a run list is the largest).
const maxJSONBytes = 8 << 20

// DialFunc opens the connection an HTTP request travels on.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Client talks to one engine.
type Client struct {
	baseURL string
	token   string
	dial    DialFunc
}

// ErrOffLoopback marks an engine address that is not on the server's
// loopback. The bearer token is sent to the address in cleartext HTTP, so
// ParseAddr refuses anything else: the caller has to opt in, with
// ParseAddrRemote and NewRemote.
var ErrOffLoopback = errors.New("is not on the server's loopback, and the bearer token would be sent there in cleartext")

// ParseAddr validates an engine address as the SERVER sees it (host:port,
// or :port for the server's loopback) and returns it with the engine's
// base URL there. The host has to be the server's loopback, as an IP
// literal (127.0.0.0/8, ::1) or empty: the token travels to it in
// cleartext. It is never resolved, and a name is not taken, even
// `localhost`: with a tunnel the server's resolver decides where a name
// goes, and the client cannot see it. An address off the loopback is
// refused with an error that wraps ErrOffLoopback; ParseAddrRemote takes
// one.
func ParseAddr(raw string) (addr, baseURL string, err error) {
	return parseAddr(raw, false)
}

// ParseAddrRemote is ParseAddr for an address off the server's loopback:
// any host, a name included. Only for a user who opted in (the teploy
// CLI's --allow-remote-addr): the engine speaks plain HTTP, so the bearer
// token travels to that address as it is.
func ParseAddrRemote(raw string) (addr, baseURL string, err error) {
	return parseAddr(raw, true)
}

func parseAddr(raw string, remote bool) (addr, baseURL string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("the engine's address is empty")
	}
	if strings.Contains(raw, "://") || strings.Contains(raw, "/") || strings.Contains(raw, "?") || strings.Contains(raw, "#") {
		return "", "", fmt.Errorf("engine address %q is not host:port (no scheme, no path)", raw)
	}
	host, port, splitErr := net.SplitHostPort(raw)
	if splitErr != nil {
		return "", "", fmt.Errorf("engine address %q is not host:port", raw)
	}
	number, convErr := strconv.ParseUint(port, 10, 16)
	if convErr != nil || number == 0 {
		return "", "", fmt.Errorf("engine address %q has no valid port", raw)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !remote && !loopback(host) {
		return "", "", fmt.Errorf("engine address %q %w", raw, ErrOffLoopback)
	}
	addr = net.JoinHostPort(host, port)
	return addr, "http://" + addr, nil
}

// loopback says whether host is a loopback address written as one.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New returns a client for the engine at addr, as ParseAddr accepted it:
// the server's loopback. A nil dial uses net/http's own.
func New(addr, token string, dial DialFunc) (*Client, error) {
	return newClient(addr, token, dial, false)
}

// NewRemote is New for an engine off the server's loopback, as
// ParseAddrRemote accepted it. It is the one way this package sends the
// token anywhere but the loopback, so a caller cannot end up there
// without having asked.
func NewRemote(addr, token string, dial DialFunc) (*Client, error) {
	return newClient(addr, token, dial, true)
}

func newClient(addr, token string, dial DialFunc, remote bool) (*Client, error) {
	_, baseURL, err := parseAddr(addr, remote)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("the engine's token is empty")
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		dial = dialer.DialContext
	}
	return &Client{baseURL: baseURL, token: token, dial: dial}, nil
}

// Addr is the engine's host:port.
func (c *Client) Addr() string { return strings.TrimPrefix(c.baseURL, "http://") }

// Health is GET /health, the one route that needs no token.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// Capacity is what GET /v1/capacity says about the engine. Only what the
// CLI reads is decoded; the rest of the answer is ignored, as the contract
// asks of a client.
type Capacity struct {
	Server string `json:"server"`
	// Driver is "docker" or "vm". It is the engine's, not a run's: every
	// run on this engine is what the driver makes of it.
	Driver string `json:"driver"`
}

// Usage is a run's usage as the engine reports it. The sampled fields are
// absent until the engine samples, and a driver that cannot read one leaves
// it out: a nil pointer is "not known", never zero.
type Usage struct {
	RunningSec int        `json:"runningSec"`
	CPUSeconds *float64   `json:"cpuSeconds,omitempty"`
	RxBytes    *int64     `json:"rxBytes,omitempty"`
	TxBytes    *int64     `json:"txBytes,omitempty"`
	DiskBytes  *int64     `json:"diskBytes,omitempty"`
	SampledAt  *time.Time `json:"sampledAt,omitempty"`
}

// Lease is a run's writable lease while one is held.
type Lease struct {
	Holder     string    `json:"holder"`
	Generation uint64    `json:"generation"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Run is one sandbox as the engine reports it, in the list and on its own.
// The optional fields are absent when the engine cannot say.
type Run struct {
	ID          string            `json:"id"`
	Server      string            `json:"server,omitempty"` // only on a single-run answer
	Image       string            `json:"image"`
	Network     string            `json:"network"`
	User        string            `json:"user"`
	State       string            `json:"state"` // "running" or "stopped"; anything else is passed through
	CreatedAt   time.Time         `json:"createdAt"`
	ExpiresAt   time.Time         `json:"expiresAt"` // the running deadline; a stopped run ignores it
	OnExpire    string            `json:"onExpire"`  // "destroy" or "stop"
	Labels      map[string]string `json:"labels,omitempty"`
	Usage       Usage             `json:"usage"`
	StoppedAt   *time.Time        `json:"stoppedAt,omitempty"`   // only while stopped
	RetainUntil *time.Time        `json:"retainUntil,omitempty"` // only while stopped
	ForkedFrom  string            `json:"forkedFrom,omitempty"`
	Lease       *Lease            `json:"lease,omitempty"`
}

// Limits are a run's ceilings. Each is the engine's default when left at
// zero: 1 CPU, 1024 MiB and 256 pids.
type Limits struct {
	MemoryMB int     `json:"memoryMb,omitempty"`
	CPUs     float64 `json:"cpus,omitempty"`
	Pids     int     `json:"pids,omitempty"`
}

// CreateRequest is POST /v1/runs.
type CreateRequest struct {
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	TTLSec  int               `json:"ttlSec,omitempty"`  // the engine's default (1800) when 0
	Network string            `json:"network,omitempty"` // "none" when empty
	// Limits is nil for the engine's defaults.
	Limits   *Limits           `json:"limits,omitempty"`
	User     string            `json:"user,omitempty"` // "root", or empty for the sandbox user
	Labels   map[string]string `json:"labels,omitempty"`
	OnExpire string            `json:"onExpire,omitempty"` // "destroy" when empty
}

// ResumeRequest is POST /v1/runs/{id}/resume.
type ResumeRequest struct {
	TTLSec int `json:"ttlSec,omitempty"`
}

// ForkRequest is POST /v1/runs/{id}/fork.
type ForkRequest struct {
	TTLSec   int               `json:"ttlSec,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	OnExpire string            `json:"onExpire,omitempty"`
}

// ExecRequest is POST /v1/runs/{id}/exec.
type ExecRequest struct {
	Cmd        string            `json:"cmd"`
	Cwd        string            `json:"cwd,omitempty"` // relative to /work
	TimeoutSec int               `json:"timeoutSec,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// ExecResult is how an exec ended.
type ExecResult struct {
	ExitCode int // the command's; -1 when the sandbox itself failed (its stderr says why)
	TimedOut bool
}

// ErrNotEngine marks an answer from something that is not a sandbox engine:
// another status than 200 on /health, or a body that is not its JSON.
var ErrNotEngine = errors.New("what answers at the address is not a sandbox engine")

// Health asks the engine how it is. It needs no token.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var health Health
	err := c.send(ctx, request{method: http.MethodGet, path: "/health", timeout: RequestTimeout}, func(resp *http.Response) error {
		raw, err := readBounded(resp.Body, maxJSONBytes)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%w: it answered %d", ErrNotEngine, resp.StatusCode)
		}
		if err != nil {
			return err
		}
		if json.Unmarshal(raw, &health) != nil || health.Status == "" {
			return fmt.Errorf("%w: its answer is not the engine's health document", ErrNotEngine)
		}
		return nil
	})
	return health, err
}

// Capacity reads what the engine says about itself.
func (c *Client) Capacity(ctx context.Context) (Capacity, error) {
	var capacity Capacity
	err := c.callJSON(ctx, request{method: http.MethodGet, path: "/v1/capacity", auth: true, timeout: RequestTimeout}, &capacity)
	return capacity, err
}

// Create makes a run. It is not idempotent and is never sent twice: when
// its answer is lost, OutcomeKnown says so.
func (c *Client) Create(ctx context.Context, req CreateRequest) (Run, error) {
	var run Run
	err := c.callJSON(ctx, request{method: http.MethodPost, path: "/v1/runs", body: req, auth: true, timeout: MutationTimeout}, &run)
	return run, err
}

// List is every run on the engine, in whatever order the engine lists them.
func (c *Client) List(ctx context.Context) ([]Run, error) {
	var body struct {
		Server string `json:"server"`
		Runs   []Run  `json:"runs"`
	}
	if err := c.callJSON(ctx, request{method: http.MethodGet, path: "/v1/runs", auth: true, timeout: RequestTimeout}, &body); err != nil {
		return nil, err
	}
	return body.Runs, nil
}

// Find is one run, read from the list: the engine has no route for a run on
// its own. A run that is not there is an *APIError with Status 404.
func (c *Client) Find(ctx context.Context, id string) (Run, error) {
	id, err := ValidID(id)
	if err != nil {
		return Run{}, err
	}
	runs, err := c.List(ctx)
	if err != nil {
		return Run{}, err
	}
	for _, run := range runs {
		if run.ID == id {
			run.Server = "" // the list names the server once, at its root
			return run, nil
		}
	}
	return Run{}, notFound(id)
}

// Stop captures the run's filesystem, removes its container and keeps the
// run. A stopped run cannot be stopped again: the engine answers 409.
func (c *Client) Stop(ctx context.Context, id string) (Run, error) {
	path, err := runPath(id, "stop")
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = c.callJSON(ctx, request{method: http.MethodPost, path: path, body: struct{}{}, auth: true, timeout: MutationTimeout}, &run)
	return run, err
}

// Resume boots a stopped run under its own id with a new deadline.
func (c *Client) Resume(ctx context.Context, id string, req ResumeRequest) (Run, error) {
	path, err := runPath(id, "resume")
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = c.callJSON(ctx, request{method: http.MethodPost, path: path, body: req, auth: true, timeout: MutationTimeout}, &run)
	return run, err
}

// Fork starts a new run from this one's filesystem and settings. Like
// Create it is not idempotent and is never sent twice.
func (c *Client) Fork(ctx context.Context, id string, req ForkRequest) (Run, error) {
	path, err := runPath(id, "fork")
	if err != nil {
		return Run{}, err
	}
	var run Run
	err = c.callJSON(ctx, request{method: http.MethodPost, path: path, body: req, auth: true, timeout: MutationTimeout}, &run)
	return run, err
}

// Destroy removes a run now, snapshot included. A run that is not there is
// an *APIError with Status 404: this client does not retry, so a 404 means
// it was already gone before this call.
func (c *Client) Destroy(ctx context.Context, id string) error {
	path, err := runPath(id, "")
	if err != nil {
		return err
	}
	return c.callJSON(ctx, request{method: http.MethodDelete, path: path, auth: true, timeout: MutationTimeout}, nil)
}

// PutFile writes content to path inside the run's /work. The run must be
// running; the engine answers 409 for a stopped one and 404 for no such run.
func (c *Client) PutFile(ctx context.Context, id, path string, content []byte) error {
	if int64(len(content)) > MaxFileBytes {
		return notSent(fmt.Errorf("the file is %d bytes; the engine takes at most %d in one write", len(content), int64(MaxFileBytes)))
	}
	target, err := filePath(id, path)
	if err != nil {
		return err
	}
	return c.send(ctx, request{
		method: http.MethodPut, path: target, auth: true, timeout: MutationTimeout,
		body: bytes.NewReader(content), contentType: "application/octet-stream",
	}, func(resp *http.Response) error {
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		raw, readErr := readBounded(resp.Body, maxJSONBytes)
		return problemFrom(resp.StatusCode, raw, readErr)
	})
}

// GetFile reads path inside the run's /work. A missing file is an *APIError
// with Status 404.
func (c *Client) GetFile(ctx context.Context, id, path string) ([]byte, error) {
	target, err := filePath(id, path)
	if err != nil {
		return nil, err
	}
	var content []byte
	err = c.send(ctx, request{method: http.MethodGet, path: target, auth: true, timeout: MutationTimeout}, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			raw, readErr := readBounded(resp.Body, maxJSONBytes)
			return problemFrom(resp.StatusCode, raw, readErr)
		}
		raw, readErr := readBounded(resp.Body, MaxFileBytes)
		if readErr != nil {
			return readErr
		}
		content = raw
		return nil
	})
	return content, err
}

// request is one HTTP call as the client makes it.
type request struct {
	method      string
	path        string
	body        any    // a JSON value, an io.Reader sent as it is, or nil
	contentType string // for a body that is an io.Reader
	auth        bool   // GET /health is the one route without the token
	// timeout bounds the whole call through the context. Zero leaves the
	// caller's context as the only bound (an exec: the engine ends the
	// command itself).
	timeout time.Duration
	// headerTimeout is how long the engine may take to answer at all. An
	// exec's answer goes out with its first output, so it is longer there.
	headerTimeout time.Duration
}

func (c *Client) callJSON(ctx context.Context, req request, out any) error {
	return c.send(ctx, req, func(resp *http.Response) error {
		raw, err := readBounded(resp.Body, maxJSONBytes)
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return problemFrom(resp.StatusCode, raw, err)
		}
		if err != nil {
			return fmt.Errorf("reading the engine's answer to %s %s: %w", req.method, req.path, err)
		}
		if out == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("the engine's answer to %s %s is not the JSON the wire contract promises: %w", req.method, req.path, err)
		}
		return nil
	})
}

// send does one HTTP request, once, and hands the answer to read. Every
// request gets a transport of its own so that its timeouts are its own: the
// context bounds the whole call, and the header timeout bounds the wait for
// the engine's first byte.
func (c *Client) send(ctx context.Context, req request, read func(*http.Response) error) error {
	var body io.Reader
	if req.body != nil {
		if reader, ok := req.body.(io.Reader); ok {
			body = reader
			if empty, isReader := reader.(*bytes.Reader); isReader && empty.Len() == 0 {
				body = http.NoBody // an empty file is "no body", not a chunked nothing
			}
		} else {
			encoded, err := json.Marshal(req.body)
			if err != nil {
				return err
			}
			body = bytes.NewReader(encoded)
			req.contentType = "application/json"
		}
	}
	if req.headerTimeout <= 0 {
		req.headerTimeout = req.timeout
	}
	if req.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.timeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, c.baseURL+req.path, body)
	if err != nil {
		return notSent(err)
	}
	// Sent once, never again by net/http on its own: create and fork are not
	// idempotent, and a mutation whose answer is lost must not be repeated.
	httpReq.GetBody = nil
	if req.auth {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", req.contentType)
	}
	transport := &http.Transport{
		// No proxy, whatever the environment says: the connection goes
		// where the dial function sends it and nowhere else.
		Proxy:                 nil,
		DialContext:           c.dial,
		DisableCompression:    true,
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: req.headerTimeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// The engine never redirects. Following one would hand the token to
		// whatever sits at the other address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// *url.Error names the method and URL; the headers, and so the
		// token, are not in it.
		return fmt.Errorf("%s %s: %w", req.method, req.path, unwrapURLError(err))
	}
	defer resp.Body.Close()
	return read(resp)
}

func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// readBounded reads at most limit bytes, and no more than one past it: a
// longer answer is an error, never a silent truncation.
func readBounded(body io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return raw, err
	}
	if int64(len(raw)) > limit {
		return raw, fmt.Errorf("the engine's answer is larger than %d bytes", limit)
	}
	return raw, nil
}

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidID normalizes a run id: a 26-character ULID, in either case. The id
// is put in a URL path, so it is checked before it travels anywhere.
func ValidID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	normalized := strings.ToUpper(trimmed)
	if !ulidPattern.MatchString(normalized) {
		return "", notSent(fmt.Errorf("%q is not a sandbox id (26 characters, as `teploy sandbox ls` lists them)", trimmed))
	}
	return normalized, nil
}

// ValidWorkPath checks a path against the files API's rule before it is
// sent: relative to /work, with no empty segment and no traversal. A
// leading slash is accepted and dropped, so `/src/app.go` and `src/app.go`
// name the same file.
func ValidWorkPath(raw string) (string, error) {
	path := strings.TrimPrefix(strings.TrimSpace(raw), "/")
	if path == "" {
		return "", notSent(errors.New("the path is empty; it is relative to the sandbox's /work"))
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", notSent(fmt.Errorf("path %q must stay inside the sandbox's /work (no empty segment, no . or ..)", raw))
		}
	}
	return path, nil
}

func runPath(id, suffix string) (string, error) {
	id, err := ValidID(id)
	if err != nil {
		return "", err
	}
	path := "/v1/runs/" + id
	if suffix != "" {
		path += "/" + suffix
	}
	return path, nil
}

func filePath(id, path string) (string, error) {
	path, err := ValidWorkPath(path)
	if err != nil {
		return "", err
	}
	segments := make([]string, 0, strings.Count(path, "/")+1)
	for _, segment := range strings.Split(path, "/") {
		// Each segment on its own: a slash inside a name is a separator,
		// and the engine reads the path back segment by segment.
		segments = append(segments, url.PathEscape(segment))
	}
	base, err := runPath(id, "files")
	if err != nil {
		return "", err
	}
	return base + "/" + strings.Join(segments, "/"), nil
}
