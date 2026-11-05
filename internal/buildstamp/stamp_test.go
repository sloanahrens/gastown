package buildstamp

import (
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// TestShortCutsToNine: the stamp is the first Len characters of a full sha, and
// Len is the nine characters make install records and make polecat-image tags.
func TestShortCutsToNine(t *testing.T) {
	t.Parallel()
	const full = "0123456789abcdef0123456789abcdef01234567"
	if Len != 9 {
		t.Fatalf("Len = %d, want 9", Len)
	}
	got, err := Short(full)
	if err != nil {
		t.Fatalf("Short(%q): %v", full, err)
	}
	if want := full[:9]; got != want {
		t.Errorf("Short = %q, want %q", got, want)
	}
}

// TestShortRefusesBadInput: only a full lowercase hex sha is stamped; every
// other input is a refusal, never a padded or guessed stamp.
func TestShortRefusesBadInput(t *testing.T) {
	t.Parallel()
	const full = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct{ name, in string }{
		{name: "empty", in: ""},
		{name: "eight characters", in: full[:8]},
		{name: "thirty-nine characters", in: full[:39]},
		{name: "forty-one characters", in: full + "0"},
		{name: "uppercase", in: strings.ToUpper(full)},
		{name: "non-hex", in: "g123456789abcdef0123456789abcdef01234567"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Short(tc.in)
			if err == nil {
				t.Fatalf("Short(%q) = %q, want an error", tc.in, got)
			}
			if got != "" {
				t.Errorf("Short(%q) = %q alongside an error, want no value", tc.in, got)
			}
		})
	}
}
