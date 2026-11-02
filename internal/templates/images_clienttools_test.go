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

// Distinctness cannot see a swap: the table before gt-qmd68 was distinct too,
// with docker and kubectl exchanged. Each named pin is therefore checked
// against the digest its tool was verified with. A tool missing from the table
// reads as "" and fails here.
func TestDaemonClientToolPinsMatchTheirTool(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"docker":  "76a624e4a8e5da654d1150e808175125efb5a6f1b6aa1cbd9caee18f51047a50",
		"kubectl": "922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3",
	}
	got := map[string]string{}
	for _, p := range templates.DaemonClientTools {
		got[p.Name] = p.SHA256
	}
	for name, sha := range want {
		if got[name] != sha {
			t.Errorf("%s pin = %q, want %q", name, got[name], sha)
		}
	}
}
