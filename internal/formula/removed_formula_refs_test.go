package formula

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formularefs"
)

// TestEmbeddedFormulasDoNotInvokeRemovedConvoyFormulas pins the survivors: a
// formula shipped in the binary must not invoke or name a formula gt-gzhin.5
// deleted, or gt will hand an agent a step that cannot run.
func TestEmbeddedFormulasDoNotInvokeRemovedConvoyFormulas(t *testing.T) {
	t.Parallel()

	offenders, err := formularefs.ScanFS(formulasFS, "formulas")
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("embedded formulas reference removed convoy formulas (gt-gzhin.5):\n%s", strings.Join(offenders, "\n"))
	}
}
