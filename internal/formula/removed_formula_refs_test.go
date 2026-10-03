package formula

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formularefs"
)

// TestEmbeddedFormulasDoNotReferenceRemovedFormulas pins the survivors: a
// formula shipped in the binary must not name a formula a retirement deleted,
// or gt will hand an agent a step that cannot run.
func TestEmbeddedFormulasDoNotReferenceRemovedFormulas(t *testing.T) {
	t.Parallel()

	offenders, err := formularefs.ScanFS(formulasFS, "formulas")
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("embedded formulas reference removed formulas:\n%s", strings.Join(offenders, "\n"))
	}
}
