package dns

import (
	"fmt"
	"net"
)

// Resolver resolves a hostname to IP addresses.
type Resolver func(host string) ([]string, error)

// Validate checks that the domain's DNS A record points at the server IP.
// Returns nil if any resolved IP matches, or an error with the expected IP.
// It also returns an error when either name resolves to no addresses.
// Passing nil for resolve uses the default net.LookupHost.
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

	for _, dip := range domainIPs {
		for _, sip := range serverIPs {
			if dip == sip {
				return nil
			}
		}
	}

	return fmt.Errorf(
		"DNS mismatch: %s resolves to %s, but server is %s — "+
			"point your DNS A record to %s, or use --skip-dns-check",
		domain, domainIPs[0], serverIPs[0], serverIPs[0],
	)
}
