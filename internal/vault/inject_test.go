package vault

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in         string
		name, key  string
		ok         bool
	}{
		{"vault:db#password", "db", "password", true},
		{"vault:api/keys#stripe", "api/keys", "stripe", true},
		{"plain-value", "", "", false},
		{"vault:db", "", "", false},       // no #key
		{"vault:#password", "", "", false}, // no name
		{"vault:db#", "", "", false},       // no key
	}
	for _, c := range cases {
		name, key, ok := ParseRef(c.in)
		if ok != c.ok || name != c.name || key != c.key {
			t.Errorf("ParseRef(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, name, key, ok, c.name, c.key, c.ok)
		}
	}
}

func TestCollectRefs(t *testing.T) {
	env := map[string]string{
		"DB_PASS":  "vault:db#password",
		"API_KEY":  "vault:api#stripe",
		"PLAIN":    "just-a-value",
		"BAD_REF":  "vault:oops", // malformed, ignored
	}
	refs := CollectRefs(env)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %v", len(refs), refs)
	}
	if refs["DB_PASS"] != [2]string{"db", "password"} {
		t.Errorf("DB_PASS ref wrong: %v", refs["DB_PASS"])
	}
	if _, ok := refs["PLAIN"]; ok {
		t.Error("PLAIN should not be a ref")
	}
	if _, ok := refs["BAD_REF"]; ok {
		t.Error("malformed ref should be ignored")
	}
}
