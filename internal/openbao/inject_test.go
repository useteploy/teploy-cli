package openbao

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in        string
		name, key string
		ok        bool
	}{
		{"secret:db#password", "db", "password", true},
		{"secret:api/keys#stripe", "api/keys", "stripe", true},
		{"secret:DB_PASS", "DB_PASS", "value", true}, // no #field -> defaults to "value" (flat secret)
		{"secret:db#", "db", "value", true},          // empty field -> "value"
		{"plain-value", "", "", false},
		{"secret:", "", "", false},         // no name
		{"secret:#password", "", "", false}, // no name
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
		"DB_PASS": "secret:db#password",
		"API_KEY": "secret:api#stripe",
		"FLAT":    "secret:MY_KEY", // flat ref -> field "value"
		"PLAIN":   "just-a-value",
	}
	refs := CollectRefs(env)
	if len(refs) != 3 {
		t.Fatalf("expected 3 refs, got %d: %v", len(refs), refs)
	}
	if refs["DB_PASS"] != [2]string{"db", "password"} {
		t.Errorf("DB_PASS ref wrong: %v", refs["DB_PASS"])
	}
	if refs["FLAT"] != [2]string{"MY_KEY", "value"} {
		t.Errorf("FLAT ref should default field to value: %v", refs["FLAT"])
	}
	if _, ok := refs["PLAIN"]; ok {
		t.Error("PLAIN should not be a ref")
	}
}
