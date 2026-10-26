package dns

import "testing"

func TestResolvesMatchesAnyServerAddress(t *testing.T) {
	resolve := func(host string) ([]string, error) { return []string{"158.69.206.240"}, nil }
	if !Resolves("example.com", []string{"100.67.126.38", "158.69.206.240", "172.17.0.1"}, resolve) {
		t.Fatal("domain on the public interface must match")
	}
	if Resolves("example.com", []string{"100.67.126.38"}, resolve) {
		t.Fatal("mesh-only addresses must not match a public A record")
	}
}
