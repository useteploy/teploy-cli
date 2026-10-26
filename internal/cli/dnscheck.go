package cli

import (
	"context"
	"strings"

	"github.com/useteploy/teploy/internal/dns"
	"github.com/useteploy/teploy/internal/ssh"
)

// validateDomainDNS checks the domain points at the server. When the SSH
// host does not match (a server reached over its mesh IP), the server's
// own interface addresses are accepted too: a box registered by its
// Tailscale IP otherwise refused every first deploy of a public domain
// (2026-10-25 live finding, fresh OVH box set up with --network
// tailscale). A NAT'd server whose public IP is not on an interface
// still needs --skip-dns-check.
func validateDomainDNS(ctx context.Context, executor ssh.Executor, domain, host string) error {
	err := dns.Validate(domain, host, nil)
	if err == nil || executor == nil {
		return err
	}
	out, runErr := executor.Run(ctx, "hostname -I")
	if runErr == nil && dns.Resolves(domain, strings.Fields(out), nil) {
		return nil
	}
	return err
}
