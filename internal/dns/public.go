package dns

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// Check outcomes for `teploy domains check`.
const (
	StatusOK          = "ok"
	StatusNXDomain    = "nxdomain"
	StatusWrongTarget = "wrong_target"
	StatusProxied     = "proxied"
	StatusUnknown     = "unknown"
	StatusSkipped     = "skipped"
)

// PublicResolvers are the fixed resolvers a domain check asks. The machine's
// own resolver may be a tailnet or split-horizon one, which answers differently
// from what the rest of the internet sees.
var PublicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// PublicAnswer is what the public resolvers said about one name.
type PublicAnswer struct {
	// Answers is the union across resolvers, sorted.
	Answers []string
	// NotFound is true when a resolver said the name has no addresses and none
	// returned any: "no DNS record", as opposed to a resolver failure.
	NotFound bool
	// Err is set when no resolver gave an answer at all.
	Err error
}

// lookupFunc is one resolver's lookup, replaceable in tests.
type lookupFunc func(ctx context.Context, resolver, host string) ([]string, error)

func publicLookup(ctx context.Context, resolver, host string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, resolver)
		},
	}
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP.String())
	}
	return out, nil
}

// LookupPublic asks every public resolver concurrently, each bounded by
// perResolver, so one blackholed resolver delays only itself. A trailing dot
// keeps the machine's search domains out of the name.
func LookupPublic(ctx context.Context, host string, perResolver time.Duration) PublicAnswer {
	return lookupVia(ctx, publicLookup, PublicResolvers, host, perResolver)
}

func lookupVia(ctx context.Context, lookup lookupFunc, resolvers []string, host string, perResolver time.Duration) PublicAnswer {
	name := strings.TrimSuffix(host, ".") + "."
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	notFound := false
	var errs []error
	for _, resolver := range resolvers {
		wg.Add(1)
		go func(resolver string) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, perResolver)
			defer cancel()
			ips, err := lookup(qctx, resolver, name)
			mu.Lock()
			defer mu.Unlock()
			var dnsErr *net.DNSError
			switch {
			case err == nil:
				for _, ip := range ips {
					seen[ip] = true
				}
				if len(ips) == 0 {
					notFound = true
				}
			case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
				notFound = true
			default:
				errs = append(errs, err)
			}
		}(resolver)
	}
	wg.Wait()
	res := PublicAnswer{}
	for ip := range seen {
		res.Answers = append(res.Answers, ip)
	}
	sort.Strings(res.Answers)
	switch {
	case len(res.Answers) > 0:
	case notFound:
		res.NotFound = true
	default:
		res.Err = errors.Join(errs...)
		if res.Err == nil {
			res.Err = errors.New("no resolver answered")
		}
	}
	return res
}

// Classify turns an answer into a status. serverIPs are the addresses that
// count as this server; targetKnown is false when no public address could be
// determined for it, in which case a non-matching answer proves nothing.
func Classify(ans PublicAnswer, serverIPs []string, targetKnown bool) string {
	switch {
	case ans.Err != nil:
		return StatusUnknown
	case ans.NotFound || len(ans.Answers) == 0:
		return StatusNXDomain
	case anyMatch(ans.Answers, serverIPs):
		return StatusOK
	case AllCloudflare(ans.Answers):
		return StatusProxied
	case !targetKnown:
		return StatusUnknown
	default:
		return StatusWrongTarget
	}
}

// cloudflareRangesAsOf dates the list below (https://www.cloudflare.com/ips/).
// A stale list can only miss a new range, which reads wrong_target for that
// answer until updated; ranges are additions in practice.
const cloudflareRangesAsOf = "2026-11-04"

var cloudflareRanges = mustCIDRs(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

// CloudflareRangesAsOf reports the date of the embedded list.
func CloudflareRangesAsOf() string { return cloudflareRangesAsOf }

// IsCloudflare reports whether one address is in Cloudflare's published ranges.
func IsCloudflare(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range cloudflareRanges {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// AllCloudflare reports whether every address is Cloudflare's (and there is at
// least one).
func AllCloudflare(ips []string) bool {
	if len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !IsCloudflare(ip) {
			return false
		}
	}
	return true
}
