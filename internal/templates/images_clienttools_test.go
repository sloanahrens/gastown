package templates_test

import (
	"regexp"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// The pins are distinct 64-hex sha256 values: two tools sharing a value is the
// swap that the first real image build found (gt-qmd68). The values themselves
// are checked against the publishers' files by hand, because no unit test
// downloads.
func TestDaemonClientToolPinsAreDistinctAndWellFormed(t *testing.T) {
	t.Parallel()
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]string{}
	for _, p := range templates.DaemonClientTools {
		if !hex64.MatchString(p.SHA256) {
			t.Errorf("%s pin %q is not 64 lower-case hex digits", p.Name, p.SHA256)
		}
		if other, dup := seen[p.SHA256]; dup {
			t.Errorf("%s and %s share the pin %s", p.Name, other, p.SHA256)
		}
		seen[p.SHA256] = p.Name
	}
	if len(seen) != 3 {
		t.Errorf("%d distinct pins, want 3 (openshell, docker, kubectl)", len(seen))
	}
}
