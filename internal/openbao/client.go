package openbao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/docker"
	"github.com/useteploy/teploy/internal/secret"
	"github.com/useteploy/teploy/internal/ssh"
)

// Age-store keys for the material Teploy manages on behalf of OpenBao.
const (
	secretSealKey    = "VAULT_SEAL_KEY"    // base64 32-byte static-seal key
	secretSealKeyID  = "VAULT_SEAL_KEY_ID" // stable UUID for the seal key
	secretRootToken  = "VAULT_ROOT_TOKEN"  // root token from operator init
	secretRecovery   = "VAULT_RECOVERY_KEYS"
	defaultAccessory = "openbao"
	defaultImage     = "openbao/openbao:latest"
	kvMount          = "secret"
	containerAPIAddr = "http://127.0.0.1:8200"
)

// Client drives the OpenBao lifecycle on a server via SSH.
type Client struct {
	exec    ssh.Executor
	docker  *docker.Client
	secrets *secret.Manager
	out     io.Writer
}

// NewClient builds a vault client bound to a server executor.
func NewClient(exec ssh.Executor, out io.Writer) *Client {
	return &Client{
		exec:    exec,
		docker:  docker.NewClient(exec),
		secrets: secret.NewManager(exec),
		out:     out,
	}
}

// SetupOptions configures a `vault setup`.
type SetupOptions struct {
	App       string
	Accessory string // default "openbao"
	Image     string // default openbao/openbao:latest
	// Seal selects the unseal mechanism. A zero-value (empty Type) means the
	// static-env-key default (Teploy generates + manages the key). For awskms/
	// transit, the caller fills the seal params + SealEnv credentials.
	Seal SealSpec
	// SealEnv is extra container env for the seal backend — never written to
	// disk in the config (AWS creds, transit token, …). Passed via the 0600
	// env-file like the static key.
	SealEnv map[string]string
}

func (o *SetupOptions) defaults() {
	if o.Accessory == "" {
		o.Accessory = defaultAccessory
	}
	if o.Image == "" {
		o.Image = defaultImage
	}
}

// Status is the parsed `bao status` output.
type Status struct {
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	SealType    string `json:"type"`
	Version     string `json:"version"`
}

// Setup provisions OpenBao and brings it to a ready (initialized + unsealed)
// state, idempotently: it can be re-run safely. Steps: ensure the age-stored
// seal key, write config, run the container, wait for the API, then (only if
// uninitialized) init + persist root/recovery + enable KV + enable audit.
func (c *Client) Setup(ctx context.Context, opts SetupOptions) error {
	opts.defaults()
	container := accessories.ContainerName(opts.App, opts.Accessory)

	// 1. Resolve the seal. Default (empty Type) = static-env-key: Teploy
	// generates + manages the 32-byte key in the age store. For awskms/transit
	// the caller supplies the seal params + credentials (nothing to generate).
	seal := opts.Seal
	sealEnv := map[string]string{}
	for k, v := range opts.SealEnv {
		sealEnv[k] = v
	}
	if seal.Type == "" || seal.Type == SealStatic {
		sealKey, err := c.ensureSecret(ctx, opts.App, secretSealKey, GenerateSealKey)
		if err != nil {
			return err
		}
		keyID, err := c.ensureSecret(ctx, opts.App, secretSealKeyID, GenerateKeyID)
		if err != nil {
			return err
		}
		seal = SealSpec{Type: SealStatic, KeyID: keyID}
		sealEnv["BAO_SEAL_KEY"] = sealKey
	}

	// 2. Render + upload the server config (file storage, private-mesh TCP,
	// the selected auto-unseal). Secrets (static key, AWS creds, transit token)
	// are NOT in the config — they ride the 0600 seal env-file.
	cfg := RenderServerConfig(ServerConfig{
		StoragePath: "/openbao/data",
		ListenAddr:  "0.0.0.0:8200",
		TLSDisable:  true, // reachable only on the private teploy network
		APIAddr:     "http://0.0.0.0:8200",
		Seal:        seal,
		// Audit device on from day one (declarative). Every secret access is
		// logged (values HMAC'd); `teploy secret audit ship` forwards these into
		// the observe tamper-evident trail.
		AuditFilePath: "/openbao/data/audit.log",
	})
	confPath := fmt.Sprintf("/deployments/%s/accessories/%s/config.hcl", opts.App, opts.Accessory)
	if err := c.exec.Upload(ctx, strings.NewReader(cfg), confPath, "0644"); err != nil {
		return fmt.Errorf("uploading openbao config: %w", err)
	}

	// 3. Run the container if not already running.
	if err := c.ensureContainer(ctx, opts, container, confPath, sealEnv); err != nil {
		return err
	}

	// 4. Wait for the API to answer.
	st, err := c.waitReady(ctx, container, 30*time.Second)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "OpenBao %s running (seal: %s)\n", st.Version, st.SealType)

	// 5. Initialize once. With auto-unseal, init yields recovery keys + a root
	// token and OpenBao unseals itself immediately.
	if !st.Initialized {
		fmt.Fprintln(c.out, "Initializing OpenBao...")
		root, err := c.initialize(ctx, opts.App, container)
		if err != nil {
			return err
		}
		if err := c.enableKV(ctx, container, root); err != nil {
			return err
		}
		fmt.Fprintln(c.out, "OpenBao initialized; KV enabled. Root token + recovery keys stored in the app secret store.")
	} else if st.Sealed {
		return fmt.Errorf("openbao is initialized but sealed — the seal key may have changed; check %s", secretSealKey)
	} else {
		fmt.Fprintln(c.out, "OpenBao already initialized and unsealed.")
	}
	return nil
}

// Status returns the live seal/init state.
func (c *Client) Status(ctx context.Context, app, accessory string) (*Status, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	return c.status(ctx, accessories.ContainerName(app, accessory))
}

// --- internals ---------------------------------------------------------------

// ensureSecret returns the age-stored value for key, generating + storing it
// via gen if absent.
func (c *Client) ensureSecret(ctx context.Context, app, key string, gen func() (string, error)) (string, error) {
	if v, err := c.secrets.Get(ctx, app, key); err == nil && v != "" {
		return v, nil
	}
	v, err := gen()
	if err != nil {
		return "", err
	}
	if err := c.secrets.Set(ctx, app, key, v); err != nil {
		return "", fmt.Errorf("storing %s: %w", key, err)
	}
	return v, nil
}

func (c *Client) ensureContainer(ctx context.Context, opts SetupOptions, container, confPath string, sealEnv map[string]string) error {
	// Idempotent: skip if already running.
	if out, err := c.exec.Run(ctx, fmt.Sprintf("docker inspect -f '{{.State.Status}}' %s 2>/dev/null", ssh.ShellQuote(container))); err == nil && strings.TrimSpace(out) == "running" {
		return nil
	}
	if _, err := c.exec.Run(ctx, "docker network inspect teploy >/dev/null 2>&1 || docker network create teploy"); err != nil {
		return fmt.Errorf("ensuring teploy network: %w", err)
	}
	// Remove any stopped remnant so the run doesn't name-collide.
	c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(container)+" >/dev/null 2>&1 || true")

	// The seal credentials ride in an env-file (mode 0600) rather than argv so
	// they aren't exposed in the process list during `docker run`, and never in
	// the (world-readable) config.hcl.
	var envBuf strings.Builder
	for k, v := range sealEnv {
		fmt.Fprintf(&envBuf, "%s=%s\n", k, v)
	}
	envFile := fmt.Sprintf("/deployments/%s/accessories/%s/.seal-env", opts.App, opts.Accessory)
	if err := c.exec.Upload(ctx, strings.NewReader(envBuf.String()), envFile, "0600"); err != nil {
		return fmt.Errorf("writing seal env: %w", err)
	}

	// OpenBao runs as uid 100 (openbao); a fresh named volume is root-owned, so
	// pre-chown it via a one-shot root container. This keeps OpenBao non-root
	// (no --user 0) without needing host-side sudo/chown.
	dataVol := container + "-data"
	if _, err := c.exec.Run(ctx, "docker volume create "+ssh.ShellQuote(dataVol)+" >/dev/null"); err != nil {
		return fmt.Errorf("creating data volume: %w", err)
	}
	chown := "docker run --rm --user 0:0 --entrypoint chown -v " + ssh.ShellQuote(dataVol) + ":/openbao/data " + ssh.ShellQuote(opts.Image) + " -R 100:1000 /openbao/data"
	if _, err := c.exec.Run(ctx, chown); err != nil {
		return fmt.Errorf("preparing data volume ownership: %w", err)
	}

	run := strings.Join([]string{
		"docker run --detach",
		"--restart always",
		"--name " + ssh.ShellQuote(container),
		"--network teploy",
		"--network-alias " + ssh.ShellQuote(container),
		"--label teploy.app=" + ssh.ShellQuote(opts.App),
		"--label teploy.role=accessory",
		"--label teploy.accessory=" + ssh.ShellQuote(opts.Accessory),
		"--cap-add IPC_LOCK",
		"--env-file " + ssh.ShellQuote(envFile),
		"-v " + ssh.ShellQuote(confPath) + ":/openbao/config.hcl:ro",
		"-v " + ssh.ShellQuote(dataVol) + ":/openbao/data",
		"--log-opt max-size=10m",
		ssh.ShellQuote(opts.Image),
		"server -config=/openbao/config.hcl",
	}, " ")
	if _, err := c.exec.Run(ctx, run); err != nil {
		return fmt.Errorf("starting openbao container: %w", err)
	}
	return nil
}

func (c *Client) bao(ctx context.Context, container, token, args string) (string, error) {
	env := "BAO_ADDR=" + containerAPIAddr
	if token != "" {
		env += " BAO_TOKEN=" + token
	}
	// docker.Exec wraps `docker exec <c> sh -c <cmd>`; we build the inner cmd.
	out, err := c.docker.Exec(ctx, container, env+" bao "+args)
	if err != nil {
		// The SSH executor discards stdout on a non-zero exit and folds bao's
		// stderr into the error. Return that text as the "output" so callers can
		// inspect one string for messages / idempotency markers ("already in
		// use"). On success, out is the real stdout (e.g. JSON).
		return err.Error(), err
	}
	return out, nil
}

func (c *Client) status(ctx context.Context, container string) (*Status, error) {
	// `bao status` exits non-zero when sealed/uninitialized but still prints the
	// JSON — append `|| true` so the exec succeeds and we keep stdout. A real
	// failure (container down, API not yet listening) yields no JSON and the
	// parse below fails, which waitReady treats as "not ready yet".
	out, err := c.docker.Exec(ctx, container, "BAO_ADDR="+containerAPIAddr+" bao status -format=json || true")
	if err != nil {
		return nil, err
	}
	out = extractJSON(out)
	var st Status
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return nil, fmt.Errorf("parsing bao status (%q): %w", truncate(out, 120), err)
	}
	return &st, nil
}

func (c *Client) waitReady(ctx context.Context, container string, timeout time.Duration) (*Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		if st, err := c.status(ctx, container); err == nil {
			return st, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("openbao did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *Client) initialize(ctx context.Context, app, container string) (rootToken string, err error) {
	out, err := c.bao(ctx, container, "", "operator init -format=json")
	if err != nil {
		return "", fmt.Errorf("operator init: %w", err)
	}
	var res struct {
		RootToken      string   `json:"root_token"`
		RecoveryKeysB64 []string `json:"recovery_keys_b64"`
		UnsealKeysB64   []string `json:"unseal_keys_b64"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return "", fmt.Errorf("parsing init output: %w", err)
	}
	if res.RootToken == "" {
		return "", fmt.Errorf("init did not return a root token")
	}
	if err := c.secrets.Set(ctx, app, secretRootToken, res.RootToken); err != nil {
		return "", fmt.Errorf("storing root token: %w", err)
	}
	keys := res.RecoveryKeysB64
	if len(keys) == 0 {
		keys = res.UnsealKeysB64
	}
	if len(keys) > 0 {
		_ = c.secrets.Set(ctx, app, secretRecovery, strings.Join(keys, ","))
	}
	return res.RootToken, nil
}

func (c *Client) enableKV(ctx context.Context, container, root string) error {
	// Idempotent: ignore "path is already in use".
	out, err := c.bao(ctx, container, root, "secrets enable -path="+kvMount+" kv-v2")
	if err != nil && !strings.Contains(out, "already in use") {
		return fmt.Errorf("enabling kv: %w (%s)", err, truncate(out, 120))
	}
	return nil
}


// extractJSON returns the JSON object embedded in output that may have leading
// log lines — from the first "{" to the last "}" (bao -format=json is
// pretty-printed across multiple lines).
func extractJSON(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j < 0 || j < i {
		return strings.TrimSpace(s)
	}
	return s[i : j+1]
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
