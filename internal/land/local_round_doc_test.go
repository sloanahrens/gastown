package land

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalRoundDocNamesTheBound holds the local round design page to the rules
// the code enforces: the bound, the switch and the failure rule. Each phrase is
// its own subtest, so a reworded rule fails one row and names what went missing.
func TestLocalRoundDocNamesTheBound(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "local-round.md"))
	if err != nil {
		t.Fatalf("read the local round design page: %v", err)
	}
	doc := string(data)
	for _, want := range []struct {
		name   string
		phrase string
	}{
		{"the bound", "two per bead per day"},
		{"the switch", "daemon.local_round.enabled"},
		{"the unavailable rule", "An unavailable answer never blocks a submit"},
	} {
		t.Run(want.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(doc, want.phrase) {
				t.Errorf("docs/design/local-round.md does not state %q (%q)", want.name, want.phrase)
			}
		})
	}
}
