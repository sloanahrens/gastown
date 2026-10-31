package templates

import (
	"strings"
	"testing"
)

// gt-0chpv: the Dolt Connectivity section must make bd the probe and say that a
// shell connection to port 3307 is denied by sandbox policy, not an outage.
func TestPolecatDoltConnectivityNamesTheBdProbeAndTheBashDenial(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	role, err := tmpl.RenderRole("polecat", RoleData{
		Role: "polecat", RigName: "myrig", TownRoot: "/test/town", TownName: "town",
		WorkDir: "/test/town/myrig/polecats/TestCat", DefaultBranch: "main", Polecat: "TestCat",
	})
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}
	for _, want := range []string{
		"bd list --limit 1",
		"3 times, about 10 seconds apart",
		"Port 3307 from a shell is not evidence",
		"denied by\npolicy while Dolt is healthy",
	} {
		if !strings.Contains(role, want) {
			t.Errorf("polecat role lacks %q", want)
		}
	}
	if strings.Contains(role, "Check server health + latency") {
		t.Error("polecat role still offers gt dolt status as the first Dolt check")
	}
}
