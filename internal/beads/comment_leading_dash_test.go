package beads

import (
	"reflect"
	"testing"
)

// TestAddCommentPassesLeadingDashTextAsArgument: bd parses a bare argument
// that starts with a dash as flags, so a comment body like "- [ ] ship it"
// failed the whole call with "unknown shorthand flag: ' '" and the comment
// was never written; only `gt bead comment <id> -- "- [ ] x"` reached bd.
// AddComment now puts such text after "--", which ends flag parsing and
// leaves everything after it positional, and a comment that needs no
// separator keeps the argv every other caller sees (gt-m0fvn).
func TestAddCommentPassesLeadingDashTextAsArgument(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)

	if err := b.AddComment("gt-1", "- [ ] ship it"); err != nil {
		t.Fatalf("AddComment(dash text): %v", err)
	}
	if err := b.AddComment("gt-1", "plain text"); err != nil {
		t.Fatalf("AddComment(plain text): %v", err)
	}

	want := []string{
		"comments add gt-1 -- - [ ] ship it",
		"comments add gt-1 plain text",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("bd argvs = %q, want %q", got, want)
	}
}
