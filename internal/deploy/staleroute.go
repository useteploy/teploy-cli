package deploy

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/useteploy/teploy/internal/caddy"
	"github.com/useteploy/teploy/internal/state"
)

// staleRouteBlock is the managed Caddy block a non-caddy release found on the
// server: left by an earlier caddy-ingress release, it keeps Caddy obtaining
// certificates and serving a dead upstream for a domain teploy no longer
// routes (one such block burned Let's Encrypt rate limits ~56 times a week).
type staleRouteBlock struct {
	present bool
	hash    string
	webhook bool
}

// resolveStaleRoute reads the app's managed block under the app lock. A read
// failure only warns: a non-caddy deploy never depended on the Caddyfile, and
// a server running no Caddy at all reads as absent.
func resolveStaleRoute(ctx context.Context, cd *caddy.Client, out io.Writer, app string) staleRouteBlock {
	region, present, err := cd.ReadManagedBlock(ctx, app)
	if err != nil {
		fmt.Fprintf(out, "Warning: could not check for a stale Caddy route for %s: %v\n", app, err)
		return staleRouteBlock{}
	}
	if !present {
		return staleRouteBlock{}
	}
	return staleRouteFromRegion(region)
}

func staleRouteFromRegion(region string) staleRouteBlock {
	return staleRouteBlock{
		present: true,
		hash:    caddy.ManagedRegionHash(region),
		webhook: strings.Contains(region, "# TEPLOY WEBHOOK BEGIN"),
	}
}

// retireStaleRoute removes the block once the non-caddy release is committed:
// under the operation's fence guard and an exact-block compare-and-swap, so a
// successor's block is never removed and a failed operation (which never
// reaches this point) leaves the route intact. Failure leaves the block in
// place for the next deploy and is reported; traffic is already committed.
func retireStaleRoute(ctx context.Context, cd *caddy.Client, out io.Writer, app, ingress string, block staleRouteBlock, generation uint64, guardPrefix string) error {
	if !block.present {
		return nil
	}
	fmt.Fprintf(out, "Removing the stale Caddy route for %s (ingress: %s)...\n", app, ingress)
	if block.webhook {
		fmt.Fprintf(out, "  Note: the block carried the autodeploy webhook route; it stops working until a caddy-ingress release or another front serves it\n")
	}
	err := cd.WithCommitGuard(guardPrefix).WithRouteCAS(app, []string{block.hash}, generation).RemoveRoute(ctx, app)
	if err == nil {
		return nil
	}
	if state.FenceLost(err) {
		err = fmt.Errorf("%w: refusing to remove %s's route", state.ErrFenceLost, app)
	}
	fmt.Fprintf(out, "Warning: the stale Caddy route for %s was not removed: %v\n", app, err)
	return err
}
