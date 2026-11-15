package taskmap

import (
	"strings"
	"testing"
)

// TestHooksReconcileCheckQuotesDryRunOutput pins the reconcile Check to the
// text `gt hooks reconcile --dry-run` prints: the per-file marker and the
// summary line in internal/cmd/hooks_reconcile.go. A Check that names words
// the output never prints sends the reader looking for a line that is not there.
func TestHooksReconcileCheckQuotesDryRunOutput(t *testing.T) {
	t.Parallel()

	entry, ok := Lookup("hooks reconcile")
	if !ok {
		t.Fatal("Lookup(hooks reconcile) found no entry")
	}
	for _, printed := range []string{
		"(would remove interim hook)",
		"Would remove the interim hook from 0 of",
	} {
		if !strings.Contains(entry.Check, printed) {
			t.Errorf("hooks reconcile Check = %q, want it to quote the printed %q", entry.Check, printed)
		}
	}
}
