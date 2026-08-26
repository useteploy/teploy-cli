package dns

import (
	"fmt"
	"strings"
	"testing"
)

func mockResolver(mapping map[string][]string) Resolver {
	return func(host string) ([]string, error) {
		if ips, ok := mapping[host]; ok {
			return ips, nil
		}
		return nil, fmt.Errorf("no such host: %s", host)
	}
}

func TestValidate_Match(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"1.2.3.4"},
		"1.2.3.4":   {"1.2.3.4"},
	})

	if err := Validate("myapp.com", "1.2.3.4", resolve); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidate_Mismatch(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"5.6.7.8"},
		"1.2.3.4":   {"1.2.3.4"},
	})

	err := Validate("myapp.com", "1.2.3.4", resolve)
	if err == nil {
		t.Fatal("expected DNS mismatch error")
	}
	if !strings.Contains(err.Error(), "DNS mismatch") {
		t.Errorf("expected DNS mismatch message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "point your DNS A record to 1.2.3.4") {
		t.Errorf("expected suggestion with correct IP, got: %v", err)
	}
}

func TestValidate_DomainUnresolvable(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"1.2.3.4": {"1.2.3.4"},
	})

	err := Validate("nonexistent.example.com", "1.2.3.4", resolve)
	if err == nil {
		t.Fatal("expected error for unresolvable domain")
	}
	if !strings.Contains(err.Error(), "could not resolve domain") {
		t.Errorf("expected resolve error, got: %v", err)
	}
}

func TestValidate_ServerUnresolvable(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"1.2.3.4"},
	})

	err := Validate("myapp.com", "bad-server.example.com", resolve)
	if err == nil {
		t.Fatal("expected error for unresolvable server")
	}
	if !strings.Contains(err.Error(), "could not resolve server") {
		t.Errorf("expected resolve error, got: %v", err)
	}
}

func TestValidate_MultipleIPs(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"10.0.0.1", "1.2.3.4"},
		"1.2.3.4":   {"1.2.3.4"},
	})

	if err := Validate("myapp.com", "1.2.3.4", resolve); err != nil {
		t.Errorf("expected match with multiple IPs, got: %v", err)
	}
}

func TestValidate_ServerHostname(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com":          {"1.2.3.4"},
		"server.example.com": {"1.2.3.4"},
	})

	if err := Validate("myapp.com", "server.example.com", resolve); err != nil {
		t.Errorf("expected match via hostname resolution, got: %v", err)
	}
}

func TestValidate_IPv6Match(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"::1"},
		"::1":       {"::1"},
	})

	if err := Validate("myapp.com", "::1", resolve); err != nil {
		t.Errorf("expected IPv6 match, got: %v", err)
	}
}

func TestValidate_DomainResolvesToNoAddresses(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"1.2.3.4": {"1.2.3.4"},
	})

	err := Validate("myapp.com", "1.2.3.4", func(host string) ([]string, error) {
		if host == "1.2.3.4" {
			return resolve(host)
		}
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected error for domain resolving to no addresses")
	}
	if !strings.Contains(err.Error(), "could not resolve domain") {
		t.Errorf("expected resolve error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no addresses returned") {
		t.Errorf("expected no addresses returned message, got: %v", err)
	}
}

func TestValidate_ServerResolvesToNoAddresses(t *testing.T) {
	resolve := mockResolver(map[string][]string{
		"myapp.com": {"1.2.3.4"},
	})

	err := Validate("myapp.com", "bad-server.example.com", func(host string) ([]string, error) {
		if host == "myapp.com" {
			return resolve(host)
		}
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected error for server resolving to no addresses")
	}
	if !strings.Contains(err.Error(), "could not resolve server") {
		t.Errorf("expected resolve error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "no addresses returned") {
		t.Errorf("expected no addresses returned message, got: %v", err)
	}
}
