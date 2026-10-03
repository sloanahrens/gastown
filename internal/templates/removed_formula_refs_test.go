package templates

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formularefs"
)

// TestRoleTemplatesDoNotReferenceRemovedFormulas pins the role templates
// against instructions that name a deleted formula: telling a crew worker to
// run one sends it down a path that now fails.
func TestRoleTemplatesDoNotReferenceRemovedFormulas(t *testing.T) {
	t.Parallel()

	offenders, err := formularefs.ScanFS(templateFS, "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("role templates reference removed formulas:\n%s", strings.Join(offenders, "\n"))
	}
}
