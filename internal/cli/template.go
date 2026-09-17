package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"github.com/useteploy/teploy/internal/config"
	tmpl "github.com/useteploy/teploy/internal/template"
)

func newTemplateCmd(flags *Flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Deploy from community templates",
	}

	cmd.AddCommand(newTemplateListCmd(flags))
	cmd.AddCommand(newTemplateInfoCmd(flags))
	cmd.AddCommand(newTemplateDeployCmd(flags))
	cmd.AddCommand(newTemplateInstallCmd(flags))

	return cmd
}

func newTemplateListCmd(flags *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available templates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()

			reg := tmpl.NewRegistry()
			templates, err := reg.List(ctx)
			if err != nil {
				return err
			}

			// --json is a documented, working global flag on every other
			// list-style command (server list, registry list, status) —
			// this one silently discarded its *Flags argument and never
			// checked it, always printing the human-readable format
			// regardless. Found while checking whether teploy-dash (which
			// calls exactly `teploy template list --json` and expects
			// real JSON to unmarshal) would work now that the template
			// registry itself is fixed — it wouldn't have, on this bug
			// alone.
			if flags.JSON {
				return json.NewEncoder(os.Stdout).Encode(templates)
			}

			if len(templates) == 0 {
				fmt.Println("No templates available")
				return nil
			}

			for _, t := range templates {
				accs := ""
				if len(t.Accessories) > 0 {
					accs = fmt.Sprintf(" [%s]", joinStrings(t.Accessories))
				}
				fmt.Printf("  %-20s %s%s\n", t.Name, t.Description, accs)
			}
			return nil
		},
	}
}

func newTemplateInfoCmd(_ *Flags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show template details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()

			reg := tmpl.NewRegistry()
			content, _, err := reg.Fetch(ctx, args[0], nil)
			if err != nil {
				return err
			}

			fmt.Println(content)
			return nil
		},
	}
}

func newTemplateDeployCmd(flags *Flags) *cobra.Command {
	var domain, server string
	var port int
	var vars []string
	var varStdin bool

	cmd := &cobra.Command{
		Use:   "deploy <name>",
		Short: "Deploy from a template",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stdinVars, err := stdinVarMap(cmd, varStdin)
			if err != nil {
				return err
			}
			return runTemplateDeploy(flags, args[0], domain, server, port, vars, stdinVars)
		},
	}

	// --domain is optional for host-ingress templates (they publish on
	// bind:port, not a domain); the pairing is validated after render in
	// applyTemplateOverrides, so the error names the template's actual ingress
	// mode rather than demanding a flag the template may not use.
	cmd.Flags().StringVar(&domain, "domain", "", "domain for the app (required unless the template uses ingress: host)")
	cmd.Flags().StringVar(&server, "server", "", "server to deploy to")
	cmd.Flags().IntVar(&port, "port", 0, "host port override for ingress: host templates")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "template variables as key=value (required for every variable the template declares)")
	cmd.Flags().BoolVar(&varStdin, "var-stdin", false, "read variable values as a JSON object from stdin (e.g. '{\"API_KEY\":\"...\"}'); overrides --var entries with the same name — use for secret values")

	return cmd
}

func runTemplateDeploy(flags *Flags, name, domain, server string, port int, extraVars []string, stdinVars map[string]string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	vars := templateVars(domain, extraVars, stdinVars)

	reg := tmpl.NewRegistry()
	content, generated, err := reg.Fetch(ctx, name, vars)
	if err != nil {
		return err
	}

	// Parse as AppConfig.
	appCfg, err := config.ParseAppBytes([]byte(content))
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}

	if err := applyTemplateOverrides(appCfg, domain, server, port); err != nil {
		return err
	}

	// The written teploy.yml is what `teploy deploy` re-reads, so a --port
	// override must land in the FILE, not just the in-memory config (there is
	// no AppConfig serializer; a targeted top-level line patch is the same
	// shape GenerateSecrets uses for its substitutions).
	if port != 0 {
		content = replaceTopLevelPort(content, port)
	}

	// Write to teploy.yml in current directory.
	if err := os.WriteFile("teploy.yml", []byte(content), 0644); err != nil {
		return fmt.Errorf("writing teploy.yml: %w", err)
	}

	fmt.Printf("Template %q written to teploy.yml\n", name)
	fmt.Printf("  App: %s\n", appCfg.App)
	if appCfg.Domain != "" {
		fmt.Printf("  Domain: %s\n", appCfg.Domain)
	} else {
		fmt.Printf("  Ingress: host (port %d)\n", appCfg.Port)
	}
	if len(appCfg.Accessories) > 0 {
		fmt.Println("  Accessories:")
		for accName, acc := range appCfg.Accessories {
			fmt.Printf("    %s (%s)\n", accName, acc.Image)
		}
	}
	printGeneratedSecrets(generated)
	fmt.Println("\nRun 'teploy deploy' to deploy this template.")
	return nil
}

// stdinVarMap reads the --var-stdin JSON object when the flag is set, else
// returns nil. Shared by template deploy and install (audit UPSTREAM-1:
// secret variable values must be able to avoid argv).
func stdinVarMap(cmd *cobra.Command, varStdin bool) (map[string]string, error) {
	if !varStdin {
		return nil, nil
	}
	return readVarMapStdin(cmd.InOrStdin())
}

// templateVars builds the variable map handed to the registry: the built-in
// domain entry, then --var flag pairs, then the --var-stdin object on top
// (stdin entries override --var pairs with the same name). Malformed --var
// pairs (no "=") are skipped, matching the historical behavior.
func templateVars(domain string, extraVars []string, stdinVars map[string]string) map[string]string {
	vars := map[string]string{"domain": domain}
	for _, v := range extraVars {
		parts := strings.SplitN(v, "=", 2)
		if len(parts) == 2 {
			vars[parts[0]] = parts[1]
		}
	}
	for k, v := range stdinVars {
		vars[k] = v
	}
	return vars
}

// applyTemplateOverrides sets the operator's flags on the rendered config and
// validates the ingress/domain pairing: an `ingress: host` template publishes
// on bind:port and needs no domain (the port comes from the template or
// --port); every other ingress mode routes by domain and requires one.
func applyTemplateOverrides(appCfg *config.AppConfig, domain, server string, port int) error {
	if appCfg.Ingress == config.IngressHost {
		if domain == "" && port == 0 && appCfg.Port == 0 {
			return fmt.Errorf("template uses ingress: host but declares no port; pass --port")
		}
	} else if domain == "" {
		return fmt.Errorf("--domain is required (only ingress: host templates may omit it)")
	}
	if domain != "" {
		appCfg.Domain = domain
	}
	if server != "" {
		appCfg.Server = server
	}
	if port != 0 {
		appCfg.Port = port
	}
	return nil
}

func newTemplateInstallCmd(flags *Flags) *cobra.Command {
	var domain, server string
	var port int
	var vars []string
	var varStdin bool

	cmd := &cobra.Command{
		Use:   "install <name>",
		Short: "Fetch a template and deploy it in one step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("--server is required")
			}
			stdinVars, err := stdinVarMap(cmd, varStdin)
			if err != nil {
				return err
			}
			return runTemplateInstall(flags, args[0], domain, server, port, vars, stdinVars)
		},
	}

	cmd.Flags().StringVar(&domain, "domain", "", "domain for the app (required unless the template uses ingress: host)")
	cmd.Flags().StringVar(&server, "server", "", "server to deploy to (required)")
	cmd.Flags().IntVar(&port, "port", 0, "host port override for ingress: host templates")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "extra template variables as key=value")
	cmd.Flags().BoolVar(&varStdin, "var-stdin", false, "read variable values as a JSON object from stdin (e.g. '{\"API_KEY\":\"...\"}'); overrides --var entries with the same name — use for secret values")

	return cmd
}

func runTemplateInstall(flags *Flags, name, domain, server string, port int, extraVars []string, stdinVars map[string]string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	vars := templateVars(domain, extraVars, stdinVars)

	reg := tmpl.NewRegistry()
	content, generated, err := reg.Fetch(ctx, name, vars)
	if err != nil {
		return err
	}

	appCfg, err := config.ParseAppBytes([]byte(content))
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}

	if err := applyTemplateOverrides(appCfg, domain, server, port); err != nil {
		return err
	}

	fmt.Printf("Installing template %q\n", name)
	fmt.Printf("  App:    %s\n", appCfg.App)
	if appCfg.Domain != "" {
		fmt.Printf("  Domain: %s\n", appCfg.Domain)
	} else {
		fmt.Printf("  Ingress: host (port %d)\n", appCfg.Port)
	}
	fmt.Printf("  Server: %s\n", server)

	// Templates are first-deploys by definition, so volume mismatch can't apply yet.
	if err := deployAppConfig(flags, appCfg, server, appCfg.Image, "", false, false); err != nil {
		return err
	}
	// Unlike `template deploy` (which writes the rendered content, secrets
	// included, to a local teploy.yml the operator can reopen), install
	// deploys directly — the rendered content with real generated secret
	// values is never written anywhere else. Without printing them here,
	// a "generate"d database password is used once to deploy and then
	// permanently lost, locking the operator out of their own database.
	printGeneratedSecrets(generated)
	return nil
}

// replaceTopLevelPort rewrites (or appends) the top-level `port:` line in a
// rendered teploy.yml. Indented `port:` lines belong to accessories and are
// left alone.
func replaceTopLevelPort(content string, port int) string {
	lines := strings.Split(content, "\n")
	replaced := false
	for i, line := range lines {
		if strings.HasPrefix(line, "port:") {
			lines[i] = fmt.Sprintf("port: %d", port)
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, fmt.Sprintf("port: %d", port))
	}
	return strings.Join(lines, "\n")
}

// printGeneratedSecrets shows the operator any "generate" sentinel values
// a template resolved to a real random value, since nothing else captures
// them — see Fetch's doc comment in internal/template/template.go.
func printGeneratedSecrets(generated map[string]string) {
	if len(generated) == 0 {
		return
	}
	fmt.Println("\nGenerated credentials — save these now, they will not be shown again:")
	for key, value := range generated {
		fmt.Printf("  %s: %s\n", key, value)
	}
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
