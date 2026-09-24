package openbao

import (
	"context"
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
	// The whole config — including the admin password — rides stdin as JSON
	// (`write <path> -`), never the docker exec argv (C08).
	connURL := fmt.Sprintf("postgresql://{{username}}:{{password}}@%s:5432/%s?sslmode=disable", dbHost, opts.DBName)
	cfg, err := json.Marshal(map[string]string{
		"plugin_name":     "postgresql-database-plugin",
		"allowed_roles":   dbRoleName(opts.App),
		"connection_url":  connURL,
		"username":        opts.AdminUser,
		"password":        opts.AdminPass,
	})
	if err != nil {
		return fmt.Errorf("encoding db connection config: %w", err)
	}
	if out, err := c.baoInput(ctx, container, root, "write database/config/"+dbConnName(opts.App)+" -", string(cfg)); err != nil {
		return fmt.Errorf("configuring db connection: %s", truncate(out, 200))
	}

	// 3. Role: each cred request mints a login role granted SELECT, expiring at
	// the lease end. Least privilege — read-only by default. Same stdin JSON
	// transport (creation_statements is arbitrary SQL — no quoting surface).
	creation := `CREATE ROLE "{{name}}" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}'; GRANT SELECT ON ALL TABLES IN SCHEMA public TO "{{name}}";`
	role, err := json.Marshal(map[string]string{
		"db_name":             dbConnName(opts.App),
		"creation_statements": creation,
		"default_ttl":         opts.TTL,
		"max_ttl":             opts.MaxTTL,
	})
	if err != nil {
		return fmt.Errorf("encoding db role config: %w", err)
	}
	if out, err := c.baoInput(ctx, container, root, "write database/roles/"+dbRoleName(opts.App)+" -", string(role)); err != nil {
		return fmt.Errorf("creating db role: %s", truncate(out, 200))
	}

	// 4. Extend the app policy to read the dynamic creds path (re-writes the
	// whole policy so it stays declarative).
	if err := c.writeAppPolicy(ctx, container, root, opts.App, true); err != nil {
		return err
	}
	return nil
}

// StaticRoleOptions configures a static database role: OpenBao takes over an
// EXISTING database user's password and rotates it on a schedule (vs. dynamic
// roles, which mint a new short-lived user per request).
type StaticRoleOptions struct {
	App            string
	Accessory      string
	DBAccessory    string
	DBName         string
	AdminUser      string
	AdminPass      string
	Username       string // the existing DB user OpenBao will manage
	RotationPeriod string // e.g. "24h"
}

// staticRoleName namespaces the static role by app to avoid cross-app collision.
func staticRoleName(app, username string) string { return app + "-" + username }

// EnableStaticRole registers a static role that rotates an existing DB user's
// password every RotationPeriod. Ensures the connection exists (allowed_roles
// "*", so both dynamic and static roles work), then writes the static role and
// extends the app policy to read its rotating credentials. Idempotent.
func (c *Client) EnableStaticRole(ctx context.Context, opts StaticRoleOptions) error {
	if opts.Accessory == "" {
		opts.Accessory = defaultAccessory
	}
	if opts.DBName == "" {
		opts.DBName = opts.DBAccessory
	}
	if opts.AdminUser == "" {
		opts.AdminUser = "postgres"
	}
	if opts.RotationPeriod == "" {
		opts.RotationPeriod = "24h"
	}
	if opts.Username == "" {
		return fmt.Errorf("static role requires --username (an existing DB user)")
	}
	root, err := c.rootToken(ctx, opts.App)
	if err != nil {
		return err
	}
	container := accessories.ContainerName(opts.App, opts.Accessory)
	dbHost := accessories.ContainerName(opts.App, opts.DBAccessory)

	if out, err := c.bao(ctx, container, root, "secrets enable -path=database database"); err != nil &&
		!strings.Contains(out, "already in use") && !strings.Contains(out, "already enabled") {
		return fmt.Errorf("enabling database engine: %s", truncate(out, 160))
	}
	// Connection with allowed_roles "*" so dynamic + static roles both work.
	// Same stdin JSON transport as the dynamic path — the admin password
	// never enters an argv (C08).
	connURL := fmt.Sprintf("postgresql://{{username}}:{{password}}@%s:5432/%s?sslmode=disable", dbHost, opts.DBName)
	cfg, err := json.Marshal(map[string]string{
		"plugin_name":    "postgresql-database-plugin",
		"allowed_roles":  "*",
		"connection_url": connURL,
		"username":       opts.AdminUser,
		"password":       opts.AdminPass,
	})
	if err != nil {
		return fmt.Errorf("encoding db connection config: %w", err)
	}
	if out, err := c.baoInput(ctx, container, root, "write database/config/"+dbConnName(opts.App)+" -", string(cfg)); err != nil {
		return fmt.Errorf("configuring db connection: %s", truncate(out, 200))
	}
	// Static role: OpenBao rotates opts.Username's password every RotationPeriod.
	role, err := json.Marshal(map[string]string{
		"db_name":         dbConnName(opts.App),
		"username":        opts.Username,
		"rotation_period": opts.RotationPeriod,
	})
	if err != nil {
		return fmt.Errorf("encoding static role config: %w", err)
	}
	if out, err := c.baoInput(ctx, container, root, "write database/static-roles/"+staticRoleName(opts.App, opts.Username)+" -", string(role)); err != nil {
		return fmt.Errorf("creating static role: %s", truncate(out, 200))
	}
	// Grant the app read on its rotating static creds.
	return c.writeAppPolicy(ctx, container, root, opts.App, true)
}

// StaticCreds reads the current (OpenBao-managed) credentials for a static role.
func (c *Client) StaticCreds(ctx context.Context, app, accessory, username string) (map[string]any, error) {
	if accessory == "" {
		accessory = defaultAccessory
	}
	root, err := c.rootToken(ctx, app)
	if err != nil {
		return nil, err
	}
	container := accessories.ContainerName(app, accessory)
	out, err := c.bao(ctx, container, root, "read -format=json database/static-creds/"+staticRoleName(app, username))
	if err != nil {
		return nil, fmt.Errorf("reading static creds: %s", truncate(out, 160))
	}
	var res struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(extractJSON(out)), &res); err != nil {
		return nil, fmt.Errorf("parsing static creds: %w", err)
	}
	return res.Data, nil
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
// the policy so EnsureAppRole and EnableDatabaseSecrets stay consistent. The
// token (line 1) and the raw HCL policy ride stdin — the old form put the
// root token directly in the docker exec argv (C08), and the base64 dance
// existed only to survive argv quoting, which the pipe no longer needs.
func (c *Client) writeAppPolicy(ctx context.Context, container, root, app string, withDB bool) error {
	inner := `IFS= read -r teploy_tok; export BAO_ADDR=` + containerAPIAddr + ` BAO_TOKEN="$teploy_tok"; exec bao policy write ` + app + `-read -`
	if _, err := c.docker.ExecInput(ctx, container, inner, strings.NewReader(root+"\n"+AppReadPolicy(app, withDB))); err != nil {
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
		// Static roles are namespaced by app prefix; scope the grant to them.
		fmt.Fprintf(&b, "path \"database/static-creds/%s-*\" { capabilities = [\"read\"] }\n", app)
	}
	return b.String()
}
