package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

// TestPrintHookStoresSearched pins the output that keeps "No hooked beads
// found" from reading as "nothing is wedged anywhere" (gt-hdph): the scan must
// name the stores it queried, and name any store it could not query.
func TestPrintHookStoresSearched(t *testing.T) {
	result := &deacon.StaleHookScanResult{
		StoresSearched: []string{"town (/tmp/town/.beads)", "gastown (/tmp/town/gastown/mayor/rig/.beads)"},
		StoreErrors:    map[string]string{"beads": "dolt unreachable"},
	}

	out := captureStdout(t, func() { printHookStoresSearched(result) })

	for _, want := range []string{
		"Searched 2 store(s)",
		"town (/tmp/town/.beads)",
		"gastown (/tmp/town/gastown/mayor/rig/.beads)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestPrintHookStoresSearchedNoStores(t *testing.T) {
	out := captureStdout(t, func() { printHookStoresSearched(&deacon.StaleHookScanResult{}) })
	if strings.TrimSpace(out) != "" {
		t.Errorf("output %q, want nothing when no stores were recorded", out)
	}
}
