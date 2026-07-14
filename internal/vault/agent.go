package vault

import (
	"context"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/accessories"
	"github.com/useteploy/teploy/internal/ssh"
)

// SecretsVolume is the named docker volume shared between the OpenBao Agent
// sidecar and the app container. The agent renders secrets into it; the app
// reads them at AgentMountPath.
func SecretsVolume(app string) string { return app + "-vault-secrets" }

// AgentMountPath is where the shared secrets volume mounts inside the app.
const AgentMountPath = "/vault/secrets"

// AgentTemplate is one rendered secret file the agent maintains.
type AgentTemplate struct {
	Contents    string // consul-template body
	Destination string // path inside the shared volume (e.g. /vault/secrets/db.env)
}

// AgentConfig is the input to the agent HCL.
type AgentConfig struct {
	VaultAddr   string
	RoleIDPath  string
	SecretIDPath string
	TokenSink   string
	Templates   []AgentTemplate
}

// RenderAgentConfig produces the OpenBao Agent HCL (pure/testable): auto-auth
// via AppRole (renews the token automatically) + one template per secret. The
// agent re-renders and the leases auto-renew, so dynamic creds rotate without
// the app doing anything but re-reading the file.
func RenderAgentConfig(c AgentConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "vault {\n  address = %q\n}\n\n", c.VaultAddr)
	b.WriteString("auto_auth {\n")
	b.WriteString("  method \"approle\" {\n")
	b.WriteString("    config = {\n")
	fmt.Fprintf(&b, "      role_id_file_path                   = %q\n", c.RoleIDPath)
	fmt.Fprintf(&b, "      secret_id_file_path                 = %q\n", c.SecretIDPath)
	b.WriteString("      remove_secret_id_file_after_reading = false\n")
	b.WriteString("    }\n  }\n")
	fmt.Fprintf(&b, "  sink \"file\" {\n    config = {\n      path = %q\n    }\n  }\n", c.TokenSink)
	b.WriteString("}\n")
	for _, t := range c.Templates {
		fmt.Fprintf(&b, "\ntemplate {\n  contents    = %q\n  destination = %q\n}\n", t.Contents, t.Destination)
	}
	return b.String()
}

// DBEnvTemplate renders a consul-template that writes dynamic DB creds as an
// env file the app can source (DB_USER / DB_PASS).
func DBEnvTemplate(app string) AgentTemplate {
	return AgentTemplate{
		Contents:    fmt.Sprintf(`{{ with secret "database/creds/%s" }}DB_USER={{ .Data.username }}%sDB_PASS={{ .Data.password }}%s{{ end }}`, dbRoleName(app), "\n", "\n"),
		Destination: AgentMountPath + "/db.env",
	}
}

// DeployAgent (re)deploys the OpenBao Agent sidecar for the app: writes the
// AppRole credential files + agent config, prepares the shared secrets volume,
// and runs the agent container on the private network. Idempotent — a redeploy
// recreates the agent with fresh config. Requires EnsureAppRole to have stored
// the role_id/secret_id.
func (c *Client) DeployAgent(ctx context.Context, app, accessory string, templates []AgentTemplate) error {
	if accessory == "" {
		accessory = defaultAccessory
	}
	roleID, err := c.secrets.Get(ctx, app, secretRoleID)
	if err != nil || roleID == "" {
		return fmt.Errorf("no AppRole for %q — run 'teploy vault setup' first", app)
	}
	secretID, err := c.secrets.Get(ctx, app, secretSecretID)
	if err != nil || secretID == "" {
		return fmt.Errorf("no AppRole secret_id for %q — run 'teploy vault setup' first", app)
	}

	baoContainer := accessories.ContainerName(app, accessory)
	agentContainer := app + "-vault-agent"
	dir := fmt.Sprintf("/deployments/%s/accessories/vault-agent", app)

	// Stage role_id / secret_id / agent.hcl on the host (0600), then copy them
	// into a config VOLUME owned by uid 100 (the agent's user). A bind mount of
	// these 0600 SSH-user-owned files would be unreadable by uid 100 inside the
	// container; a chowned volume is the correct way to hand secret files to a
	// non-root container without making them world-readable on the host.
	if err := c.exec.Upload(ctx, strings.NewReader(roleID), dir+"/role_id", "0600"); err != nil {
		return fmt.Errorf("writing role_id: %w", err)
	}
	if err := c.exec.Upload(ctx, strings.NewReader(secretID), dir+"/secret_id", "0600"); err != nil {
		return fmt.Errorf("writing secret_id: %w", err)
	}
	agentCfg := RenderAgentConfig(AgentConfig{
		VaultAddr:    "http://" + baoContainer + ":8200",
		RoleIDPath:   "/agent/role_id",
		SecretIDPath: "/agent/secret_id",
		TokenSink:    AgentMountPath + "/.vault-token",
		Templates:    templates,
	})
	if err := c.exec.Upload(ctx, strings.NewReader(agentCfg), dir+"/agent.hcl", "0644"); err != nil {
		return fmt.Errorf("writing agent config: %w", err)
	}

	image := defaultImage
	// Config volume: copy the staged files in as root, chown to uid 100, lock
	// the credential files to 0400 (owner-read only, inside the volume).
	cfgVol := agentContainer + "-config"
	if _, err := c.exec.Run(ctx, "docker volume create "+ssh.ShellQuote(cfgVol)+" >/dev/null"); err != nil {
		return fmt.Errorf("creating agent config volume: %w", err)
	}
	stage := "docker run --rm --user 0:0 -v " + ssh.ShellQuote(dir) + ":/src:ro -v " + ssh.ShellQuote(cfgVol) +
		":/agent --entrypoint sh " + ssh.ShellQuote(image) +
		" -c 'cp /src/role_id /src/secret_id /src/agent.hcl /agent/ && chown -R 100:1000 /agent && chmod 400 /agent/role_id /agent/secret_id'"
	if _, err := c.exec.Run(ctx, stage); err != nil {
		return fmt.Errorf("staging agent credentials: %w", err)
	}

	// Prepare the shared secrets volume (chowned for uid 100, like the data vol).
	vol := SecretsVolume(app)
	if _, err := c.exec.Run(ctx, "docker volume create "+ssh.ShellQuote(vol)+" >/dev/null"); err != nil {
		return fmt.Errorf("creating secrets volume: %w", err)
	}
	chown := "docker run --rm --user 0:0 --entrypoint chown -v " + ssh.ShellQuote(vol) + ":" + AgentMountPath + " " + ssh.ShellQuote(image) + " -R 100:1000 " + AgentMountPath
	if _, err := c.exec.Run(ctx, chown); err != nil {
		return fmt.Errorf("preparing secrets volume: %w", err)
	}

	// Recreate the agent container with the current config.
	c.exec.Run(ctx, "docker rm -f "+ssh.ShellQuote(agentContainer)+" >/dev/null 2>&1 || true")
	run := strings.Join([]string{
		"docker run --detach",
		"--restart always",
		"--name " + ssh.ShellQuote(agentContainer),
		"--network teploy",
		"--label teploy.app=" + ssh.ShellQuote(app),
		"--label teploy.role=accessory",
		"--label teploy.accessory=vault-agent",
		"-v " + ssh.ShellQuote(cfgVol) + ":/agent",
		"-v " + ssh.ShellQuote(vol) + ":" + AgentMountPath,
		"--log-opt max-size=10m",
		ssh.ShellQuote(image),
		"agent -config=/agent/agent.hcl",
	}, " ")
	if _, err := c.exec.Run(ctx, run); err != nil {
		return fmt.Errorf("starting vault agent: %w", err)
	}
	return nil
}
