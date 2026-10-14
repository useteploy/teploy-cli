package docker

import (
	"strings"
	"testing"
)

func TestContainersByAppExactLabel(t *testing.T) {
	containers := []Container{
		{Name: "speedballs-web-1", Labels: map[string]string{"teploy.app": "speedballs"}},
		{Name: "speedballs-mp-web-1", Labels: map[string]string{"teploy.app": "speedballs-mp"}},
		{Name: "unlabeled"},
		{Name: "foreign", Labels: map[string]string{"other": "speedballs"}},
	}
	got := ContainersByApp(containers, "speedballs")
	if len(got) != 1 || got[0].Name != "speedballs-web-1" {
		t.Fatalf("exact-label grouping wrong: %#v", got)
	}
	if got := ContainersByApp(containers, "nope"); len(got) != 0 {
		t.Fatalf("unknown app matched containers: %#v", got)
	}
}

// The unfiltered fleet query and the per-app filtered query must share one
// format string — both must request Labels as a structured JSON object
// (audit T15: `{{json .}}` renders Labels as docker's comma-joined display
// string, which can forge reserved labels) — or the two views of the same
// host can disagree.
func TestFleetAndPerAppQueriesShareFormat(t *testing.T) {
	if all := listAllContainersCommand(); all != "docker ps --all "+containersFormat {
		t.Fatalf("fleet command drifted: %s", all)
	}
	if filtered := listContainersCommand("demo"); filtered != "docker ps --all --filter label=teploy.app='demo' "+containersFormat {
		t.Fatalf("per-app command drifted: %s", filtered)
	}
	if !strings.Contains(containersFormat, `"Labels":{{json .Labels}}`) {
		t.Fatalf("format must render Labels as structured JSON (audit T15): %q", containersFormat)
	}
}
