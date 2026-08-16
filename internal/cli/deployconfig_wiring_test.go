package cli

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/useteploy/teploy/internal/config"
	"github.com/useteploy/teploy/internal/deploy"
)

// The gap that shipped `memory:` as a no-op for apps.
//
// deploy.Config is a separate struct from config.AppConfig, and the CLI copies
// the one into the other field by field. `memory:`/`cpu:` were added to
// AppConfig and were already consumed by deploy.go, so both ends were correct
// and every unit test on either side passed — but nothing copied them across,
// so a user setting `memory: 1g` got a container with no limit and no warning.
// Accessories were unaffected (they pass config.AccessoryConfig straight
// through), which is exactly why a live check of the accessory path looked
// like proof the feature worked.
//
// This guards the seam: any field present on BOTH structs under the same name
// must actually be assigned from appCfg in the deploy.Config literal. Add a
// deliberate exception below with a reason rather than deleting the check.
var commentPattern = regexp.MustCompile(`(?m)//.*$`)

func TestDeployConfigCopiesEveryMatchingAppConfigField(t *testing.T) {
	// Fields that exist on both but are deliberately NOT a straight copy.
	intentional := map[string]string{
		"Image":    "resolved from build/registry, not the raw config value",
		"Version":  "computed (git hash or --version), not from teploy.yml",
		"Volumes":  "normalized to host paths before being passed",
		"EnvFiles": "resolved + decrypted before being passed",
		"Domain":   "may be rewritten for preview environments",
		"Env":      "folded into EnvFiles by buildContainerEnvFiles, so secrets never reach the docker run argv",
	}

	source, err := os.ReadFile("deploy.go")
	if err != nil {
		t.Fatalf("reading deploy.go: %v", err)
	}
	// Just the deploy.Config literal, so an unrelated mention elsewhere in the
	// file cannot make a missing assignment look present.
	literal := regexp.MustCompile(`(?s)deployCfg := deploy\.Config\{(.*?)\n\t\}`).FindSubmatch(source)
	if literal == nil {
		t.Fatal("could not find the `deployCfg := deploy.Config{...}` literal in deploy.go")
	}
	// Strip line comments before matching. `strings.Contains(body, "Memory:")`
	// is otherwise satisfied by `// Memory: appCfg.Memory,` — verified: deleting
	// the assignment fails this test correctly, but commenting it out did not,
	// which is exactly how a field gets parked "temporarily" and never restored.
	body := commentPattern.ReplaceAllString(string(literal[1]), "")

	appFields := map[string]bool{}
	appType := reflect.TypeOf(config.AppConfig{})
	for i := range appType.NumField() {
		appFields[appType.Field(i).Name] = true
	}

	deployType := reflect.TypeOf(deploy.Config{})
	var missing []string
	for i := range deployType.NumField() {
		name := deployType.Field(i).Name
		if !appFields[name] {
			continue // no same-named counterpart; nothing to copy
		}
		if _, ok := intentional[name]; ok {
			continue
		}
		if !strings.Contains(body, name+":") {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Errorf("deploy.Config fields with an identically-named AppConfig field that the CLI never copies: %v\n"+
			"Each is a teploy.yml key that parses, validates, and is then silently ignored at deploy time.\n"+
			"Either assign it from appCfg, or add it to `intentional` with the reason.", missing)
	}
}
