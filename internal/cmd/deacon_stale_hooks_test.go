package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deacon"
)

// TestPrintHookStoresSearched pins the output that keeps "No hooked beads
// found" from reading as "nothing is wedged anywhere" (gt-hdph): the scan must
// name the stores it queried.
func TestPrintHookStoresSearched(t *testing.T) {
	result := &deacon.StaleHookScanResult{
		StoresSearched: []string{"town (/tmp/town/.beads)", "gastown (/tmp/town/gastown/mayor/rig/.beads)"},
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

// TestPrintStoreErrors pins the warning that keeps a partial sweep from
// reading as a clean one (gt-8gjk): a store the scan could not query must be
// named, regardless of whether stale beads were found elsewhere. The warning
// goes to stderr (style.PrintWarning), so this reads stderr, not stdout.
func TestPrintStoreErrors(t *testing.T) {
	result := &deacon.StaleHookScanResult{
		StoreErrors: map[string]string{"beads": "dolt unreachable"},
	}

	errOut := captureStderr(t, func() { printStoreErrors(result) })

	for _, want := range []string{"beads", "dolt unreachable"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q missing %q", errOut, want)
		}
	}
}

func TestPrintStoreErrorsNone(t *testing.T) {
	errOut := captureStderr(t, func() { printStoreErrors(&deacon.StaleHookScanResult{}) })
	if strings.TrimSpace(errOut) != "" {
		t.Errorf("stderr %q, want nothing when no store errors were recorded", errOut)
	}
}
