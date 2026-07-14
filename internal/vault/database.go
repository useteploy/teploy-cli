package vault

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/accessories"
)

// DBSetupOptions configures the dynamic database secrets engine.
type DBSetupOptions struct {
	App         string
	Accessory   string // OpenBao accessory (default "openbao")
	DBAccessory string // the database accessory container to issue creds for (e.g. "postgres")
	DBName      string // logical DB name (default the DBAccessory)
	AdminUser   string // DB superuser OpenBao uses to create roles (default "postgres")
	AdminPass   string // DB superuser password (from the accessory's stored credentials)
	TTL         string // default lease TTL (default "1h")
	MaxTTL      string // max lease TTL (default "24h")
}

// dbRoleName / dbConnName are the OpenBao paths for this app's DB engine.
func dbRoleName(app string) string { return app + "-role" }
func dbConnName(app string) string { return app + "-db" }

// EnableDatabaseSecrets configures OpenBao's database secrets engine to issue
// short-lived, auto-revoked PostgreSQL credentials for the app's database
// accessory, and extends the app's AppRole policy to read them. Idempotent.
func (c *Client) EnableDatabaseSecrets(ctx context.Context, opts DBSetupOptions) error {
	if opts.Accessory == "" {
		opts.Accessory = defaultAccessory
	}
	if opts.DBName == "" {
		opts.DBName = opts.DBAccessory
	}
	if opts.AdminUser == "" {
		opts.AdminUser = "postgres"
	}
	if opts.TTL == "" {
		opts.TTL = "1h"
	}
	if opts.MaxTTL == "" {
		opts.MaxTTL = "24h"
	}
	root, err := c.rootToken(ctx, opts.App)
	if err != nil {
		return err
	}
	container := accessories.ContainerName(opts.App, opts.Accessory)
	dbHost := accessories.ContainerName(opts.App, opts.DBAccessory) // its network alias

	// 1. Enable the engine (idempotent).
	if out, err := c.bao(ctx, container, root, "secrets enable -path=database database"); err != nil &&
		!strings.Contains(out, "already in use") && !strings.Contains(out, "already enabled") {
		return fmt.Errorf("enabling database engine: %s", truncate(out, 160))
	}

	// 2. Configure the connection. The admin creds are used only to create/drop
	// the ephemeral roles; {{username}}/{{password}} are OpenBao's templating.
	connURL := fmt.Sprintf("postgresql://{{username}}:{{password}}@%s:5432/%s?sslmode=disable", dbHost, opts.DBName)
	cfg := fmt.Sprintf("write database/config/%s plugin_name=postgresql-database-plugin allowed_roles=%s connection_url=%s username=%s password=%s",
		dbConnName(opts.App), shellSingleQuote(dbRoleName(opts.App)),
		shellSingleQuote(connURL), shellSingleQuote(opts.AdminUser), shellSingleQuote(opts.AdminPass))
	if out, err := c.bao(ctx, container, root, cfg); err != nil {
		return fmt.Errorf("configuring db connection: %s", truncate(out, 200))
	}

	// 3. Role: each cred request mints a login role granted SELECT, expiring at
	// the lease end. Least privilege — read-only by default.
	creation := `CREATE ROLE "{{name}}" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}'; GRANT SELECT ON ALL TABLES IN SCHEMA public TO "{{name}}";`
	role := fmt.Sprintf("write database/roles/%s db_name=%s creation_statements=%s default_ttl=%s max_ttl=%s",
		dbRoleName(opts.App), dbConnName(opts.App), shellSingleQuote(creation), opts.TTL, opts.MaxTTL)
	if out, err := c.bao(ctx, container, root, role); err != nil {
		return fmt.Errorf("creating db role: %s", truncate(out, 200))
	}

	// 4. Extend the app policy to read the dynamic creds path (re-writes the
	// whole policy so it stays declarative).
	if err := c.writeAppPolicy(ctx, container, root, opts.App, true); err != nil {
		return err
	}
	return nil
}

// DBCreds reads a fresh set of dynamic database credentials.
func (c *Client) DBCreds(ctx context.Context, app, accessory string) (map[string]any, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	out, err := c.bao(ctx, container, root, "read -format=json database/creds/"+dbRoleName(app))
	if err != nil {
		return nil, fmt.Errorf("reading db creds: %s", truncate(out, 160))
	}
	var res struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return nil, fmt.Errorf("parsing db creds: %w", err)
	}
	return res.Data, nil
}

// writeAppPolicy (re)writes the app's read policy. When withDB is true it also
// grants read on the dynamic database creds path. Single source of truth for
// the policy so EnsureAppRole and EnableDatabaseSecrets stay consistent.
func (c *Client) writeAppPolicy(ctx context.Context, container, root, app string, withDB bool) error {
	policy := AppReadPolicy(app, withDB)
	// Pipe the policy via base64 rather than a heredoc: it avoids all quoting/
	// terminator interactions across the docker-exec + sh -c layers (a heredoc
	// terminator can't share its line with the 2>&1 the bao helper appends).
	b64 := base64.StdEncoding.EncodeToString([]byte(policy))
	cmd := fmt.Sprintf("echo %s | base64 -d | env BAO_ADDR=%s BAO_TOKEN=%s bao policy write %s-read -",
		b64, containerAPIAddr, root, app)
	if _, err := c.docker.Exec(ctx, container, cmd); err != nil {
		return fmt.Errorf("writing policy: %w", err)
	}
	return nil
}

// hasDBRole reports whether a dynamic DB role is already configured for the app
// (so a policy re-write can preserve the DB grant). Best-effort: any error =
// treat as absent.
func (c *Client) hasDBRole(ctx context.Context, container, root, app string) bool {
	out, err := c.bao(ctx, container, root, "read database/roles/"+dbRoleName(app))
	return err == nil && !strings.Contains(out, "No value found") && !strings.Contains(out, "Error")
}

// AppReadPolicy renders the app's least-privilege HCL policy (pure/testable):
// read-only on its own KV secrets, plus (optionally) read on its dynamic DB
// creds.
func AppReadPolicy(app string, withDB bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "path \"%s/data/%s/*\" { capabilities = [\"read\"] }\n", kvMount, app)
	fmt.Fprintf(&b, "path \"%s/metadata/%s/*\" { capabilities = [\"read\", \"list\"] }\n", kvMount, app)
	if withDB {
		fmt.Fprintf(&b, "path \"database/creds/%s\" { capabilities = [\"read\"] }\n", dbRoleName(app))
	}
	return b.String()
}
