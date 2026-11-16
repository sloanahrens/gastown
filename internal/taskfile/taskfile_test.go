package taskfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// numberedNotes is n notes whose texts read "note 00" through "note n-1", so a
// test names each one and checks the order they render in.
func numberedNotes(n int) []Note {
	notes := make([]Note, n)
	for i := range notes {
		notes[i] = Note{Text: fmt.Sprintf("note %02d", i)}
	}
	return notes
}

// notesText is the exact text Assemble owes a bead with only these notes.
func notesText(notes []Note) string {
	var sb strings.Builder
	sb.WriteString("# TASK gt-1\n\n## Notes\n\n")
	for _, note := range notes {
		sb.WriteString("- " + note.Text + "\n")
	}
	return sb.String()
}

// TestAssembleNotesCap keeps only the newest maxNotes notes, in caller order,
// and an empty entry renders nothing wherever it lands.
func TestAssembleNotesCap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		notes []Note
		want  []Note
	}{
		{"at the cap", numberedNotes(20), numberedNotes(20)},
		{"one over the cap", numberedNotes(21), numberedNotes(21)[1:]},
		{"five over the cap", numberedNotes(25), numberedNotes(25)[5:]},
		{
			"empty entry dropped with the oldest",
			append([]Note{{}}, numberedNotes(20)...),
			numberedNotes(20),
		},
		{
			"empty entry inside the cap window",
			append(numberedNotes(20), Note{}),
			numberedNotes(20)[1:],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Assemble(Input{BeadID: "gt-1", Notes: tt.notes})
			if want := notesText(tt.want); got != want {
				t.Errorf("Assemble() = %q, want %q", got, want)
			}
		})
	}
}

// TestAssembleKeepsTextAtCap leaves text of exactly maxTextBytes untouched, so
// the cut starts at one byte over rather than at the cap.
func TestAssembleKeepsTextAtCap(t *testing.T) {
	t.Parallel()
	const head = "# TASK gt-1\n\n## Description\n\n"
	body := strings.Repeat("a", maxTextBytes-len(head)-1)
	out := Assemble(Input{BeadID: "gt-1", Description: body})
	if want := maxTextBytes; len(out) != want {
		t.Fatalf("fixture assembled to %d bytes, want %d", len(out), want)
	}
	if out != head+body+"\n" {
		t.Error("text at the cap was changed")
	}
}

// TestAssembleTruncatesOverCap cuts over-long text to maxTextBytes and ends it
// with the marker line; the kept head is a prefix of the untruncated text.
func TestAssembleTruncatesOverCap(t *testing.T) {
	t.Parallel()
	const head = "# TASK gt-1\n\n## Description\n\n"
	tests := []struct {
		name string
		body string
	}{
		{"one byte over the cap", strings.Repeat("a", maxTextBytes-len(head))},
		{"ascii", strings.Repeat("a", maxTextBytes+1024)},
		{"multibyte", strings.Repeat("é", (maxTextBytes+1024)/2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := Assemble(Input{BeadID: "gt-1", Description: tt.body})
			if len(out) > maxTextBytes {
				t.Errorf("assembled %d bytes, cap is %d", len(out), maxTextBytes)
			}
			marker := truncationMarker()
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if last := lines[len(lines)-1]; last != marker {
				t.Errorf("last line = %q, want the marker %q", last, marker)
			}
			if !utf8.ValidString(out) {
				t.Error("truncated text is not valid UTF-8")
			}
			kept := strings.TrimSuffix(out, "\n"+marker+"\n")
			if full := head + tt.body + "\n"; !strings.HasPrefix(full, kept) {
				t.Errorf("kept text is not a prefix of the untruncated text:\ngot:\n%q\nfull:\n%q", kept, full)
			}
		})
	}
}

// TestAssembleTruncatesInsideRune drives the cut into the middle of a
// multi-byte rune, where backing off to the rune start is what keeps the text
// valid UTF-8.
func TestAssembleTruncatesInsideRune(t *testing.T) {
	t.Parallel()
	const head = "# TASK gt-1\n\n## Description\n\n"
	budget := maxTextBytes - len(truncationMarker()) - 2
	// Pad so the cut offset lands on a continuation byte of a three-byte rune.
	pad := ((budget-len(head))%3 + 1) % 3
	body := strings.Repeat("a", pad) + strings.Repeat("€", maxTextBytes/3+1)
	if utf8.RuneStart((head + body + "\n")[budget]) {
		t.Fatalf("fixture offsets do not cut inside a rune (pad %d)", pad)
	}
	out := Assemble(Input{BeadID: "gt-1", Description: body})
	if !utf8.ValidString(out) {
		t.Error("truncated text is not valid UTF-8")
	}
	if len(out) > maxTextBytes {
		t.Errorf("assembled %d bytes, cap is %d", len(out), maxTextBytes)
	}
}

// TestTruncationMarkerNamesTheCap pins the size the marker states to the cap the
// assembler applies, so a cap change cannot leave the marker naming the old one.
func TestTruncationMarkerNamesTheCap(t *testing.T) {
	t.Parallel()
	marker := truncationMarker()
	var stated int
	if _, err := fmt.Sscanf(marker, "[TASK.md truncated: the text exceeded %d KiB and the rest is not shown]", &stated); err != nil {
		t.Fatalf("marker does not state its size in KiB: %q", marker)
	}
	if want := maxTextBytes / 1024; stated != want {
		t.Errorf("marker states a %d KiB cap, the assembler applies %d KiB", stated, want)
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

// The rework text carries every earlier attempt and, for each, the findings its
// note recorded: id, severity, path, line and title, in the shape the bead
// holds them.
func TestNoBeadAccessAssembleRendersEveryAttemptsFindings(t *testing.T) {
	t.Parallel()
	out := Assemble(Input{
		BeadID: "gt-abc1",
		Title:  "A re-slung bead",
		Rework: &Rework{Attempts: []Rejection{
			{
				Attempt: 1,
				Kind:    "review",
				Reason:  "om requested changes (score 0.70)",
				Findings: []Finding{
					{ID: "gt-abc.1", Severity: "major", Path: "internal/foo.go", Line: 42, Title: "the retry never resets its counter"},
					{ID: "gt-abc.2", Severity: "minor", Path: "README.md", Line: 7, Title: "a claim the code does not make"},
				},
			},
			{
				Attempt:  2,
				Kind:     "gate",
				Reason:   "gate failed",
				Findings: []Finding{{ID: "gt-abc.3", Severity: "blocker", Path: "internal/baz.go", Line: 1, Title: "the build fails"}},
			},
		}},
	})
	for _, want := range []string{
		"## Earlier rejections",
		"### Attempt 1: review — om requested changes (score 0.70)",
		"- id:gt-abc.1 sev:major internal/foo.go:42 — the retry never resets its counter",
		"- id:gt-abc.2 sev:minor README.md:7 — a claim the code does not make",
		"### Attempt 2: gate — gate failed",
		"- id:gt-abc.3 sev:blocker internal/baz.go:1 — the build fails",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Assemble(...) = %q, want it to carry %q", out, want)
		}
	}
}

// The rejection-unchanged fact appears only when the host established it, and a
// first-time seat's text carries no rework section at all.
func TestNoBeadAccessAssembleRejectionUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rework  *Rework
		want    []string
		notWant []string
	}{
		{
			name: "a branch on the rejected commit carries the fact",
			rework: &Rework{
				Attempts:  []Rejection{{Attempt: 2, Kind: "review"}},
				Unchanged: &UnchangedRejection{Attempt: 2, Kind: "review", Head: "3f2a1b9cdef0123456789abcdef0123456789abcd"},
			},
			want: []string{"## Rejection unchanged", "on 3f2a1b9, the commit that attempt 2 (review) refused"},
		},
		{
			name:    "a branch that moved past the refusal carries no fact",
			rework:  &Rework{Attempts: []Rejection{{Attempt: 2, Kind: "review"}}},
			want:    []string{"## Earlier rejections"},
			notWant: []string{"## Rejection unchanged"},
		},
		{
			name:    "a first-time seat carries no rework section",
			rework:  nil,
			notWant: []string{"## Earlier rejections", "## Rejection unchanged"},
		},
		{
			name:   "an unclassified refusal is named without a class",
			rework: &Rework{Attempts: []Rejection{{Attempt: 1}}, Unchanged: &UnchangedRejection{Attempt: 1, Head: "abc1234"}},
			want:   []string{"on abc1234, the commit that attempt 1 refused"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := Assemble(Input{BeadID: "gt-abc1", Title: "t", Rework: tt.rework})
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("Assemble(...) = %q, want it to carry %q", out, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("Assemble(...) = %q, want no %q", out, notWant)
				}
			}
		})
	}
}
