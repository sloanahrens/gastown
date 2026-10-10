package beads

import (
	"reflect"
	"strings"
	"testing"
)

// TestAddCommentAsPassesLeadingDashTextAsArgument pins the argv that makes an
// authored comment body starting with a dash reach bd: bd reads
// `comments add <id> "- [ ] x" --author me` as flags and fails, and
// `comments add <id> --author me -- "- [ ] x"` parses (gt-1q9nl).
func TestAddCommentAsPassesLeadingDashTextAsArgument(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)

	if err := b.AddCommentAs("gt-1", "operator", "- [ ] ship it"); err != nil {
		t.Fatalf("AddCommentAs(dash text): %v", err)
	}
	if err := b.AddCommentAs("gt-1", "operator", "plain text"); err != nil {
		t.Fatalf("AddCommentAs(plain text): %v", err)
	}

	want := []string{
		"comments add gt-1 --author operator -- - [ ] ship it",
		"comments add gt-1 --author operator plain text",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("bd argvs = %q, want %q", got, want)
	}
}

// TestAddCommentAsRoutedCommentKeepsLeadingDashText: the routed path hands the
// text to the other database's wrapper, so the separator survives an issue
// that lives in another rig.
func TestAddCommentAsRoutedCommentKeepsLeadingDashText(t *testing.T) {
	t.Parallel()
	b, _, _ := routedTown(t)
	r := newRecorder(nil)
	b.exec = r.exec

	if err := b.AddCommentAs("pt-x", "operator", "- [ ] ship it"); err != nil {
		t.Fatal(err)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("%d bd calls, want 1", len(calls))
	}
	if got := strings.Join(calls[0].args, " "); got != "comments add pt-x --author operator -- - [ ] ship it" {
		t.Errorf("argv = %q", got)
	}
}

// TestAppendNotesPassesLeadingDashTextAsArgument pins the argv for a
// dash-leading note: bd takes the next argument as --append-notes' value
// whatever it looks like, so no separator is needed and the argv every other
// caller sees is unchanged (gt-1q9nl).
func TestAppendNotesPassesLeadingDashTextAsArgument(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	b := newRecordedBeads(t.TempDir(), r)

	if err := b.AppendNotes("gt-1", "- [ ] ship it"); err != nil {
		t.Fatalf("AppendNotes(dash note): %v", err)
	}
	if err := b.AppendNotes("gt-1", "plain note"); err != nil {
		t.Fatalf("AppendNotes(plain note): %v", err)
	}

	want := []string{
		"update gt-1 --append-notes - [ ] ship it",
		"update gt-1 --append-notes plain note",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("bd argvs = %q, want %q", got, want)
	}
}
