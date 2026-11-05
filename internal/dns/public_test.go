package dns

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	server := []string{"192.0.2.10"}
	cases := []struct {
		name        string
		ans         PublicAnswer
		targetKnown bool
		want        string
	}{
		// A public IPv6 on an interface says nothing about the box's IPv4.
		{"v4 answer, only v6 known", PublicAnswer{Answers: []string{"198.51.100.1"}}, false, StatusUnknown},
		{"answer is the server", PublicAnswer{Answers: []string{"192.0.2.10"}}, true, StatusOK},
		{"one of several is the server", PublicAnswer{Answers: []string{"192.0.2.10", "198.51.100.1"}}, true, StatusOK},
		{"no record", PublicAnswer{NotFound: true}, true, StatusNXDomain},
		{"elsewhere", PublicAnswer{Answers: []string{"198.51.100.1"}}, true, StatusWrongTarget},
		{"cloudflare", PublicAnswer{Answers: []string{"104.16.0.1", "172.67.1.1"}}, true, StatusProxied},
		{"partly cloudflare", PublicAnswer{Answers: []string{"104.16.0.1", "198.51.100.1"}}, true, StatusWrongTarget},
		{"resolvers failed", PublicAnswer{Err: errors.New("timeout")}, true, StatusUnknown},
		{"server address unknown", PublicAnswer{Answers: []string{"198.51.100.1"}}, false, StatusUnknown},
		{"no record even with unknown server", PublicAnswer{NotFound: true}, false, StatusNXDomain},
		{"cloudflare even with unknown server", PublicAnswer{Answers: []string{"104.16.0.1"}}, false, StatusProxied},
	}
	for _, c := range cases {
		if got := Classify(c.ans, server, c.targetKnown, c.targetKnown); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestClassify_PerFamily(t *testing.T) {
	server := []string{"192.0.2.10", "2001:db8::1"}
	v4 := PublicAnswer{Answers: []string{"198.51.100.1"}}
	v6 := PublicAnswer{Answers: []string{"2001:db8::99"}}
	both := PublicAnswer{Answers: []string{"198.51.100.1", "2001:db8::99"}}
	for _, c := range []struct {
		name         string
		ans          PublicAnswer
		v4Known, v6K bool
		want         string
	}{
		{"A answer, public IPv6 only", v4, false, true, StatusUnknown},
		{"A answer, public IPv4 known", v4, true, false, StatusWrongTarget},
		{"AAAA answer, public IPv4 only", v6, true, false, StatusUnknown},
		{"AAAA answer, public IPv6 known", v6, false, true, StatusWrongTarget},
		{"both families, one unknown", both, true, false, StatusUnknown},
		{"both families, both known", both, true, true, StatusWrongTarget},
	} {
		if got := Classify(c.ans, server, c.v4Known, c.v6K); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLookupVia(t *testing.T) {
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	t.Run("a blackholed resolver delays only itself", func(t *testing.T) {
		start := time.Now()
		got := lookupVia(context.Background(), func(ctx context.Context, resolver, host string) ([]string, error) {
			if resolver == "slow" {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []string{"192.0.2.1"}, nil
		}, []string{"slow", "fast"}, "site.dev", 50*time.Millisecond)
		if len(got.Answers) != 1 || got.Err != nil || got.NotFound {
			t.Fatalf("got %+v", got)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("the lookup outlived its per-resolver bound")
		}
	})
	t.Run("a timeout is not a missing record", func(t *testing.T) {
		got := lookupVia(context.Background(), func(ctx context.Context, resolver, host string) ([]string, error) {
			return nil, context.DeadlineExceeded
		}, []string{"a", "b"}, "site.dev", time.Second)
		if got.NotFound || got.Err == nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("not found from one resolver", func(t *testing.T) {
		got := lookupVia(context.Background(), func(ctx context.Context, resolver, host string) ([]string, error) {
			if resolver == "a" {
				return nil, notFound
			}
			return nil, context.DeadlineExceeded
		}, []string{"a", "b"}, "site.dev", time.Second)
		if !got.NotFound || got.Err != nil {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("an answer wins over a not found", func(t *testing.T) {
		got := lookupVia(context.Background(), func(ctx context.Context, resolver, host string) ([]string, error) {
			if resolver == "a" {
				return nil, notFound
			}
			return []string{"192.0.2.1"}, nil
		}, []string{"a", "b"}, "site.dev", time.Second)
		if got.NotFound || len(got.Answers) != 1 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("the name is queried fully qualified", func(t *testing.T) {
		var seen string
		lookupVia(context.Background(), func(ctx context.Context, resolver, host string) ([]string, error) {
			seen = host
			return nil, notFound
		}, []string{"a"}, "site.dev", time.Second)
		if seen != "site.dev." {
			t.Fatalf("queried %q", seen)
		}
	})
}

func TestIsCloudflare(t *testing.T) {
	for ip, want := range map[string]bool{
		"104.21.3.4":     true,
		"172.67.1.1":     true,
		"2606:4700::1":   true,
		"192.0.2.1":      false,
		"100.83.22.26":   false,
		"not an address": false,
	} {
		if got := IsCloudflare(ip); got != want {
			t.Errorf("IsCloudflare(%q) = %v", ip, got)
		}
	}
	if CloudflareRangesAsOf() == "" {
		t.Error("the range list must be dated")
	}
}
