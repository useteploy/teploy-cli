package config

import "testing"

func TestPublishDefaultTCPConflict(t *testing.T) {
	if err := ValidatePublishEntries([]string{"127.0.0.1:8080:80", "127.0.0.1:8080:81/tcp"}); err == nil {
		t.Fatal("equivalent TCP bindings accepted")
	}
	if err := ValidatePublishEntries([]string{"127.0.0.1:8080:80", "127.0.0.1:8080:81/udp"}); err != nil {
		t.Fatal(err)
	}
}
func TestAccessoryOverlayCommandTransitions(t *testing.T) {
	a := mergeAccessory(AccessoryConfig{Command: "old"}, AccessoryConfig{CommandArgs: []string{"new", "arg"}})
	if a.Command != "" || len(a.CommandArgs) != 2 {
		t.Fatalf("argv overlay: %+v", a)
	}
	a = mergeAccessory(a, AccessoryConfig{Command: "replacement"})
	if a.Command != "replacement" || len(a.CommandArgs) != 0 {
		t.Fatalf("string overlay: %+v", a)
	}
	a = mergeAccessory(AccessoryConfig{}, AccessoryConfig{CommandArgs: []string{"new"}})
	if len(a.CommandArgs) != 1 {
		t.Fatal("new accessory lost argv")
	}
}
func TestAppliedManifestRejectsInvalidBaseline(t *testing.T) {
	for _, input := range []string{`null`, `{"app":42}`, `{"app":null}`, `{"domain":[]}`, `{"ingress_mode":{}}`} {
		if _, err := ParseAppliedManifest([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}
