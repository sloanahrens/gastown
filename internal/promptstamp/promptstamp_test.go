package promptstamp

import "testing"

// The stamp's literal text is the contract between the spawn that writes it
// and the landing that reads it (gt-2lu00, gt-b9j3o.16). This test pins the
// text itself; every other test builds its input with Format.
func TestFormat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hash string
		size int
		want string
	}{
		{"a full sha256 is truncated to 12 hex digits", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 4096, "prompt 0123456789ab (4096 bytes)"},
		{"a hash already short enough is kept", "0123456789ab", 7, "prompt 0123456789ab (7 bytes)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Format(tc.hash, tc.size); got != tc.want {
				t.Errorf("Format(%q, %d) = %q, want %q", tc.hash, tc.size, got, tc.want)
			}
		})
	}
}

// Parse reads back exactly what Format wrote, and nothing else: a note quoting
// a stamp, a longer hash no writer produces and a near miss all name no prompt.
func TestParse(t *testing.T) {
	t.Parallel()
	stamp := Format("0123456789abcdef", 4096)
	for _, tc := range []struct {
		name, text, want string
		ok               bool
	}{
		{"the text Format wrote", stamp, "0123456789ab", true},
		{"a stamp with a trailing newline from the store", stamp + "\n", "0123456789ab", true},
		{"a stamp quoted inside a note", "the prompt " + stamp + " scored low", "", false},
		{"a whole hash, which no writer produces", "prompt " + "0123456789abcdef" + " (4096 bytes)", "", false},
		{"11 digits", "prompt 0123456789a (4096 bytes)", "", false},
		{"a non-hex digit", "prompt 0123456789ag (4096 bytes)", "", false},
		{"no size", "prompt 0123456789ab", "", false},
		{"a comment that is not a stamp", "Submitted for landing: polecat/opal/gt-1+x1", "", false},
		{"the empty string", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Parse(tc.text)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Parse(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// What the writer stamps is what the reader finds: the round trip holds for a
// full sha256, which is what the spawn holds when it stores the prompt.
func TestParseReadsBackFormat(t *testing.T) {
	t.Parallel()
	const sum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got, ok := Parse(Format(sum, 1234)); !ok || got != sum[:HashLen] {
		t.Errorf("Parse(Format(%q, 1234)) = %q, %v; want %q, true", sum, got, ok, sum[:HashLen])
	}
}

// The author a spawn stamps under is the actor of the seat it spawns, which is
// the seat the work's branch names (gt-b9j3o.16).
func TestAuthor(t *testing.T) {
	t.Parallel()
	if got, want := Author("gastown", "opal"), "gastown/polecats/opal"; got != want {
		t.Errorf("Author = %q, want %q", got, want)
	}
}
