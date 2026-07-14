package openbao

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/accessories"
)

// Age-store keys for the per-app AppRole credentials.
const (
	secretRoleID   = "VAULT_ROLE_ID"
	secretSecretID = "VAULT_SECRET_ID"
)

// rootToken returns the age-stored root token, or a helpful error if setup
// hasn't run.
func (c *Client) rootToken(ctx context.Context, app string) (string, error) {
	t, err := c.secrets.Get(ctx, app, secretRootToken)
	if err != nil || t == "" {
		return "", fmt.Errorf("no OpenBao root token for %q — run 'teploy vault setup' first", app)
	}
	return t, nil
}

// appPath is the per-app KV prefix; secrets live under secret/<app>/… so the
// per-app policy can scope tightly to them.
func appPath(app, name string) string {
	if name == "" {
		return kvMount + "/" + app
	}
	return kvMount + "/" + app + "/" + name
}

// Put writes key=value pairs to secret/<app>/<name>. Values are shell-quoted
// for the inner container shell.
func (c *Client) Put(ctx context.Context, app, accessory, name string, kvs []string) error {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return err
	}
	container := accessories.ContainerName(app, accessory)
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("invalid key=value pair %q", kv)
		}
		parts = append(parts, k+"="+shellSingleQuote(v))
	}
	out, err := c.bao(ctx, container, root, "kv put "+appPath(app, name)+" "+strings.Join(parts, " "))
	if err != nil {
		return fmt.Errorf("kv put: %s", truncate(out, 160))
	}
	return nil
}

// Get returns the key/value data at secret/<app>/<name>.
func (c *Client) Get(ctx context.Context, app, accessory, name string) (map[string]any, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	out, err := c.bao(ctx, container, root, "kv get -format=json "+appPath(app, name))
	if err != nil {
		return nil, fmt.Errorf("kv get: %s", truncate(out, 160))
	}
	var res struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return nil, fmt.Errorf("parsing kv get: %w", err)
	}
	return res.Data.Data, nil
}

// List returns the secret names under secret/<app>/.
func (c *Client) List(ctx context.Context, app, accessory string) ([]string, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	out, err := c.bao(ctx, container, root, "kv list -format=json "+kvMount+"/"+app)
	if err != nil {
		// An empty prefix lists nothing rather than erroring.
		if strings.Contains(out, "No value found") {
			return nil, nil
		}
		return nil, fmt.Errorf("kv list: %s", truncate(out, 160))
	}
	var keys []string
	if err := json.Unmarshal([]byte(extractJSON(out)), &keys); err != nil {
		return nil, fmt.Errorf("parsing kv list: %w", err)
	}
	return keys, nil
}

// AppRoleCreds is a per-app AppRole login pair.
type AppRoleCreds struct {
	RoleID   string
	SecretID string
}

// EnsureAppRole provisions (idempotently) a least-privilege AppRole for the app:
// a policy granting read on only its own secrets, the approle auth method, and a
// role bound to that policy. Returns login credentials and stores them in the
// age secret store for the deploy/agent paths to use.
func (c *Client) EnsureAppRole(ctx context.Context, app, accessory string) (*AppRoleCreds, error) {
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	policyName := app + "-read"
	roleName := app

	// 1. Least-privilege policy: read only this app's secrets. Shared with
	// EnableDatabaseSecrets via AppReadPolicy. Detect an already-configured DB
	// role so re-running setup after `vault db setup` preserves the DB grant.
	if err := c.writeAppPolicy(ctx, container, root, app, c.hasDBRole(ctx, container, root, app)); err != nil {
		return nil, err
	}
	_ = policyName

	// 2. Enable approle auth (idempotent).
	if out, err := c.bao(ctx, container, root, "auth enable approle"); err != nil &&
		!strings.Contains(out, "already in use") && !strings.Contains(out, "already enabled") {
		return nil, fmt.Errorf("enabling approle: %s", truncate(out, 160))
	}

	// 3. Role bound to the policy.
	if out, err := c.bao(ctx, container, root,
		"write auth/approle/role/"+roleName+" token_policies="+policyName+" token_ttl=20m token_max_ttl=1h secret_id_ttl=0"); err != nil {
		return nil, fmt.Errorf("writing role: %s", truncate(out, 160))
	}

	// 4. Fetch role_id + a fresh secret_id.
	roleID, err := c.baoField(ctx, container, root, "read -format=json auth/approle/role/"+roleName+"/role-id", "role_id")
	if err != nil {
		return nil, fmt.Errorf("reading role_id: %w", err)
	}
	secretID, err := c.baoField(ctx, container, root, "write -f -format=json auth/approle/role/"+roleName+"/secret-id", "secret_id")
	if err != nil {
		return nil, fmt.Errorf("generating secret_id: %w", err)
	}

	// 5. Persist for the deploy/agent paths.
	_ = c.secrets.Set(ctx, app, secretRoleID, roleID)
	_ = c.secrets.Set(ctx, app, secretSecretID, secretID)
	return &AppRoleCreds{RoleID: roleID, SecretID: secretID}, nil
}

// baoField runs a bao command returning JSON and extracts data.<field>.
func (c *Client) baoField(ctx context.Context, container, token, args, field string) (string, error) {
	out, err := c.bao(ctx, container, token, args)
	if err != nil {
		return "", fmt.Errorf("%s", truncate(out, 160))
	}
	var res struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return "", fmt.Errorf("parsing %s: %w", field, err)
	}
	v, _ := res.Data[field].(string)
	if v == "" {
		return "", fmt.Errorf("%s not found in response", field)
	}
	return v, nil
}

// shellSingleQuote wraps a value in single quotes for the inner container shell,
// escaping embedded single quotes (mirrors the kv command's approach).
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
