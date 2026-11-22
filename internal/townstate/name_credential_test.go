package townstate

import (
	"strings"
	"testing"
)

// TestNameHoldsCredential: a file name that carries a credential-shaped value is
// reported by the detector the export runs over file content, and a plain name is
// not (gt-ea3yjo). The credential is built from parts so no literal sits here.
func TestNameHoldsCredential(t *testing.T) {
	t.Parallel()
	token := "sk-" + "abcdefghijklmnop1234"
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"settings/config.json", false},
		{"settings/notes-2026-11-21.json", false},
		{"settings/" + token + ".json", true},
		{token, true},
		{"settings/ghp_" + strings.Repeat("a", 36) + ".json", true},
		{"settings/x\x1b[31m\n.json", false},
	} {
		if got := NameHoldsCredential(tc.name); got != tc.want {
			t.Errorf("NameHoldsCredential(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
