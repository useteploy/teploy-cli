package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/ssh"
)

// dnsCheckApplies is the one rule deploy and validate share: only a
// Caddy-routed domain must point at this server. Host ingress publishes a raw
// port and external ingress is served by the user's own front, so neither
// domain resolves here by design.
func dnsCheckApplies(appCfg *config.AppConfig) bool {
	return appCfg.Domain != "" && appCfg.UsesCaddy()
}

// validateDomainDNS checks every host of a (possibly comma-separated) domain
// field points at the server. When the SSH host does not match (a server
// reached over its mesh IP), the server's own interface addresses are accepted
// too: a box registered by its Tailscale IP otherwise refused every first
// deploy of a public domain (2026-10-25 live finding, fresh OVH box set up with
// --network tailscale). A NAT'd server whose public IP is not on an interface
// still needs --skip-dns-check. A host answering from Cloudflare is proxied:
// its origin cannot be read from DNS, so it passes with a returned note instead
// of reading as a mismatch.
func validateDomainDNS(ctx context.Context, executor ssh.Executor, domain, host string) (notes []string, err error) {
	var failures []error
	var interfaceIPs []string
	interfacesRead := false
	for _, d := range strings.Split(domain, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		verr := dns.Validate(d, host, nil)
		if verr == nil {
			continue
		}
		if errors.Is(verr, dns.ErrProxied) {
			notes = append(notes, fmt.Sprintf("%s answers from Cloudflare (proxied); its origin cannot be verified from DNS", d))
			continue
		}
		if executor != nil {
			if !interfacesRead {
				interfacesRead = true
				if out, runErr := executor.Run(ctx, "hostname -I"); runErr == nil {
					interfaceIPs = strings.Fields(out)
				}
			}
			if dns.Resolves(d, interfaceIPs, nil) {
				continue
			}
		}
		failures = append(failures, verr)
	}
	return notes, errors.Join(failures...)
}
