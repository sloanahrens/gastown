package taskfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fullInput is the fixture the acceptance box names: a title, a description,
// three boxes with one ticked, five notes, two comments and four steps.
func fullInput() Input {
	return Input{
		BeadID:      "gt-abc1",
		Title:       "Assemble the task text from bead fields",
		Description: "The seat reads its task from TASK.md instead of a bead.\n\nNothing here calls a service.",
		Boxes: []Box{
			{Text: "the full fixture assembles to this text", Ticked: true},
			{Text: "empty sections leave no heading behind"},
			{Text: "vet and lint exit 0"},
		},
		Notes: []Note{
			{Text: "read the test standard first"},
			{Text: "picked golden files"},
			{Text: "three boxes, one ticked"},
			{Text: "two comments with UTC times"},
			{Text: "vet clean"},
		},
		Comments: []Comment{
			{Author: "daemon", Time: time.Date(2026, 11, 14, 22, 40, 0, 0, time.UTC), Text: "SP ESTIMATE v1 sp:8 rung:all"},
			{Author: "overseer", Time: time.Date(2026, 11, 14, 22, 38, 0, 0, time.UTC), Text: "plan landed as bce08826"},
		},
		Steps: []Step{
			{Text: "Load context and verify assignment", Done: true},
			{Text: "Implement the solution"},
			{Text: "Build and sanity check"},
			{Text: "Submit work and self-clean"},
		},
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(data)
}

// TestAssembleFullGolden pins the whole text for the acceptance box's fixture
// and shows that a second run assembles the same bytes.
func TestAssembleFullGolden(t *testing.T) {
	t.Parallel()
	golden := readGolden(t, "full.md")
	first := Assemble(fullInput())
	if first != golden {
		t.Errorf("first run differs from testdata/full.md\ngot:\n%s\nwant:\n%s", first, golden)
	}
	if second := Assemble(fullInput()); second != first {
		t.Errorf("second run differs from the first\ngot:\n%s\nfirst:\n%s", second, first)
	}
}

// TestAssembleOmitsEmptySections covers the acceptance box for a bead with no
// notes, no comments and no boxes: no heading stands over nothing.
func TestAssembleOmitsEmptySections(t *testing.T) {
	t.Parallel()
	out := Assemble(Input{
		BeadID:      "gt-min1",
		Title:       "A bead with no lists",
		Description: "Only a description, and no boxes, notes or comments.",
	})
	if want := readGolden(t, "minimal.md"); out != want {
		t.Errorf("assembled text differs from testdata/minimal.md\ngot:\n%s\nwant:\n%s", out, want)
	}
	for _, heading := range []string{"## Acceptance", "## Notes", "## Comments", "## Steps"} {
		if strings.Contains(out, heading) {
			t.Errorf("output carries %q with nothing under it:\n%s", heading, out)
		}
	}
}

func TestAssembleHeading(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"id and title", Input{BeadID: "gt-1", Title: "Fix it"}, "# TASK gt-1: Fix it\n"},
		{"id only", Input{BeadID: "gt-1"}, "# TASK gt-1\n"},
		{"title only", Input{Title: "Fix it"}, "# TASK: Fix it\n"},
		{"neither", Input{}, ""},
		{"surrounding space", Input{BeadID: " gt-1 ", Title: " Fix it "}, "# TASK gt-1: Fix it\n"},
		{"newline in title", Input{BeadID: "gt-1", Title: "Fix\nit"}, "# TASK gt-1: Fix it\n"},
		{"newline in id", Input{BeadID: "gt\n1", Title: "Fix it"}, "# TASK gt 1: Fix it\n"},
		{"newline in both", Input{BeadID: " gt-1\n", Title: " Fix\nit "}, "# TASK gt-1: Fix it\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Assemble(tt.in); got != tt.want {
				t.Errorf("Assemble() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAssembleOmitsFieldsNotPassed is one row per field a caller can leave out:
// each one leaves no heading for that field behind.
func TestAssembleOmitsFieldsNotPassed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     Input
		absent string
	}{
		{"description", Input{BeadID: "gt-1", Notes: []Note{{Text: "n"}}}, "## Description"},
		{"boxes", Input{BeadID: "gt-1", Notes: []Note{{Text: "n"}}}, "## Acceptance"},
		{"notes", Input{BeadID: "gt-1", Boxes: []Box{{Text: "b"}}}, "## Notes"},
		{"comments", Input{BeadID: "gt-1", Notes: []Note{{Text: "n"}}}, "## Comments"},
		{"steps", Input{BeadID: "gt-1", Notes: []Note{{Text: "n"}}}, "## Steps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if out := Assemble(tt.in); strings.Contains(out, tt.absent) {
				t.Errorf("output carries %q for a field not passed:\n%s", tt.absent, out)
			}
		})
	}
}

// TestAssembleEmptyEntries covers the no-empty-heading rule for list entries
// that carry no text: a zero-value entry leaves no bullet and no heading
// behind, and an empty entry between real ones drops out without disturbing
// them.
func TestAssembleEmptyEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"box", Input{BeadID: "gt-1", Boxes: []Box{{}}}, "# TASK gt-1\n"},
		{"ticked box", Input{BeadID: "gt-1", Boxes: []Box{{Ticked: true}}}, "# TASK gt-1\n"},
		{"note", Input{BeadID: "gt-1", Notes: []Note{{}}}, "# TASK gt-1\n"},
		{"comment", Input{BeadID: "gt-1", Comments: []Comment{{}}}, "# TASK gt-1\n"},
		{"step", Input{BeadID: "gt-1", Steps: []Step{{}}}, "# TASK gt-1\n"},
		{"whitespace note", Input{BeadID: "gt-1", Notes: []Note{{Text: " \n\t"}}}, "# TASK gt-1\n"},
		{
			"empty between real notes",
			Input{BeadID: "gt-1", Notes: []Note{{Text: "a"}, {}, {Text: "b"}}},
			"# TASK gt-1\n\n## Notes\n\n- a\n- b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Assemble(tt.in); got != tt.want {
				t.Errorf("Assemble() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAssembleCommentLabel(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 22, 40, 0, 0, time.UTC)
	tests := []struct {
		name    string
		comment Comment
		want    string
	}{
		{"time and author", Comment{Author: "daemon", Time: at, Text: "hello"}, "## Comments\n\n- 2026-11-14 22:40 daemon: hello\n"},
		{"author only", Comment{Author: "daemon", Text: "hello"}, "## Comments\n\n- daemon: hello\n"},
		{"time only", Comment{Time: at, Text: "hello"}, "## Comments\n\n- 2026-11-14 22:40: hello\n"},
		{"neither", Comment{Text: "hello"}, "## Comments\n\n- hello\n"},
		{"empty text", Comment{Author: "daemon", Time: at}, "## Comments\n\n- 2026-11-14 22:40 daemon\n"},
		{
			"multi-line text stays one item",
			Comment{Author: "daemon", Text: "first\nsecond"},
			"## Comments\n\n- daemon: first\n  second\n",
		},
		{
			"blank line inside the text is not indented",
			Comment{Author: "daemon", Text: "first\n\nsecond"},
			"## Comments\n\n- daemon: first\n\n  second\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Assemble(Input{Comments: []Comment{tt.comment}}); got != tt.want {
				t.Errorf("Assemble() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAssembleCommentTimeIsUTC keeps the same instant assembling to the same
// bytes whatever zone the caller's time carries.
func TestAssembleCommentTimeIsUTC(t *testing.T) {
	t.Parallel()
	utc := time.Date(2026, 11, 14, 22, 40, 0, 0, time.UTC)
	zone := time.FixedZone("X", 2*60*60)
	shifted := utc.In(zone)
	if shifted.Format(commentTimeLayout) == utc.Format(commentTimeLayout) {
		t.Fatalf("fixture does not shift the wall clock: %s and %s", shifted, utc)
	}
	want := Assemble(Input{Comments: []Comment{{Time: utc, Text: "hello"}}})
	got := Assemble(Input{Comments: []Comment{{Time: shifted, Text: "hello"}}})
	if got != want {
		t.Errorf("same instant in another zone assembled differently\ngot:\n%s\nwant:\n%s", got, want)
	}
}
