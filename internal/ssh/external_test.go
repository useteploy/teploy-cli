package ssh

import "testing"

// TCL-52: endpoint handling must survive an explicit :22 (previously left
// in place and then bracketed as if it were an IPv6 literal) and an
// already-bracketed IPv6 address with a port (previously double-bracketed).
func TestRsyncTarget_EndpointForms(t *testing.T) {
	for _, tc := range []struct{ endpoint, want string }{
		{"example.com", "root@example.com:/dst"},
		{"example.com:22", "root@example.com:/dst"},
		{"example.com:2222", "root@example.com:/dst"},
		{"2001:db8::1", "root@[2001:db8::1]:/dst"},
		{"[2001:db8::1]:22", "root@[2001:db8::1]:/dst"},
		{"[2001:db8::1]:2222", "root@[2001:db8::1]:/dst"},
	} {
		if got := RsyncTarget("root", tc.endpoint, "/dst"); got != tc.want {
			t.Errorf("RsyncTarget(%q) = %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}
