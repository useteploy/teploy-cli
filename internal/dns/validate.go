package dns

import (
	"errors"
	"fmt"
	"net"
)

// Resolver resolves a hostname to IP addresses.
type Resolver func(host string) ([]string, error)

// ErrProxied marks a domain whose answers all belong to Cloudflare: it may well
// reach this server through the proxy, which DNS cannot show. Callers treat it
// as a hint, not a mismatch.
var ErrProxied = errors.New("domain is proxied by Cloudflare")

// Validate checks that the domain's DNS A record points at the server IP.
// Returns nil if any resolved IP matches, an error wrapping ErrProxied when
// every answer is a Cloudflare address, or an error with the expected IP.
// It also returns an error when either name resolves to no addresses.
// Pass nil for resolve to use the default net.LookupHost.
func Validate(domain, serverHost string, resolve Resolver) error {
	if resolve == nil {
		resolve = net.LookupHost
	}

	domainIPs, err := resolve(domain)
	if err != nil {
		return fmt.Errorf("could not resolve domain %s: %w", domain, err)
	}
	if len(domainIPs) == 0 {
		return fmt.Errorf("could not resolve domain %s: no addresses returned", domain)
	}

	serverIPs, err := resolve(serverHost)
	if err != nil {
		return fmt.Errorf("could not resolve server %s: %w", serverHost, err)
	}
	if len(serverIPs) == 0 {
		return fmt.Errorf("could not resolve server %s: no addresses returned", serverHost)
	}

	if anyMatch(domainIPs, serverIPs) {
		return nil
	}
	if AllCloudflare(domainIPs) {
		return fmt.Errorf("%w: %s resolves to %s", ErrProxied, domain, domainIPs[0])
	}

	return fmt.Errorf(
		"DNS mismatch: %s resolves to %s, but server is %s — "+
			"point your DNS A record to %s, or use --skip-dns-check",
		domain, domainIPs[0], serverIPs[0], serverIPs[0],
	)
}

// Resolves reports whether the domain resolves to any of ips. It is the
// fallback when the server is addressed by a private or mesh IP (a
// servers.yml host set to its Tailscale address by `teploy setup
// --network`): public DNS points at the server's public interface, which
// Validate's comparison against the SSH host can never match.
func Resolves(domain string, ips []string, resolve Resolver) bool {
	if resolve == nil {
		resolve = net.LookupHost
	}
	domainIPs, err := resolve(domain)
	if err != nil {
		return false
	}
	return anyMatch(domainIPs, ips)
}

// anyMatch compares addresses by value, so two spellings of one IPv6 address
// match.
func anyMatch(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if sameIP(x, y) {
				return true
			}
		}
	}
	return false
}

func sameIP(a, b string) bool {
	if a == b {
		return true
	}
	x, y := net.ParseIP(a), net.ParseIP(b)
	return x != nil && y != nil && x.Equal(y)
}
