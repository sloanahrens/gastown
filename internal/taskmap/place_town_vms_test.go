package taskmap

import (
	"strings"
	"testing"
)

// TestTownVMsSitsWithHostTools pins the grouping judgement place_town_vms.go
// records (gt-czoguq): the command starts the Mac's Colima VMs, so both entries
// belong in host, not sandboxes, which covers running work in an isolated
// environment rather than the VMs the town runs on.
func TestTownVMsSitsWithHostTools(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"town vms", "town vms start"} {
		e, ok := Lookup(path)
		if !ok {
			t.Fatalf("Lookup(%q) found no entry", path)
		}
		if e.Group != "host" {
			t.Errorf("%s Group = %q, want \"host\"", path, e.Group)
		}
		if !strings.HasPrefix(e.Use, "Use this when") {
			t.Errorf("%s Use = %q; want a line starting \"Use this when\" that names the situation", path, e.Use)
		}
	}
}
