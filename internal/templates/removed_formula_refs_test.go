package templates

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formularefs"
)

// TestRoleTemplatesDoNotInvokeRemovedConvoyFormulas pins the role templates
// against instructions that name a deleted formula (gt-gzhin.5): telling a
// crew worker to run one sends it down a path that now fails.
func TestRoleTemplatesDoNotInvokeRemovedConvoyFormulas(t *testing.T) {
	t.Parallel()

	offenders, err := formularefs.ScanFS(templateFS, "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("role templates reference removed convoy formulas (gt-gzhin.5):\n%s", strings.Join(offenders, "\n"))
	}
}
