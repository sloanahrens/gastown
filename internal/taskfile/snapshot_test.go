package taskfile

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeLookup answers from a fixed set of briefs and records every id it was
// asked for, so a test can pin the order and that an id was asked once.
func fakeLookup(known map[string]Brief, asked *[]string) func(string) (Brief, bool) {
	return func(id string) (Brief, bool) {
		if asked != nil {
			*asked = append(*asked, id)
		}
		brief, ok := known[id]
		return brief, ok
	}
}

// anyBrief answers for every id with one fixed brief, for the tests that read
// which ids the text yielded rather than what a block says.
func anyBrief(brief Brief) func(string) (Brief, bool) {
	return func(string) (Brief, bool) { return brief, true }
}

// blockHeadings is each block heading of a snapshot, in file order.
func blockHeadings(out string) []string {
	var heads []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "### "); ok {
			heads = append(heads, rest)
		}
	}
	return heads
}

// TestSnapshotFindsIDsOnceInOrder covers the acceptance box for repeats: an id
// the text names twice is looked up once and rendered in one block, and blocks
// follow the order of first appearance rather than the lookup's order.
func TestSnapshotFindsIDsOnceInOrder(t *testing.T) {
	t.Parallel()
	var asked []string
	lookup := fakeLookup(map[string]Brief{
		"gt-abc1": {ID: "gt-abc1", Title: "First"},
		"gt-def2": {ID: "gt-def2", Title: "Second"},
	}, &asked)
	out := Snapshot("Start from gt-def2, then read gt-abc1, and gt-def2 again.", lookup)
	if want := []string{"gt-def2", "gt-abc1"}; !slices.Equal(asked, want) {
		t.Errorf("lookup asked for %v, want %v", asked, want)
	}
	if want := []string{"gt-def2: Second", "gt-abc1: First"}; !slices.Equal(blockHeadings(out), want) {
		t.Errorf("headings = %v, want %v\n%s", blockHeadings(out), want, out)
	}
}

// TestSnapshotSkipsUnknownIDs covers the acceptance box for a name the store
// does not hold: the block is left out whole and the bead is not named at all.
func TestSnapshotSkipsUnknownIDs(t *testing.T) {
	t.Parallel()
	lookup := fakeLookup(map[string]Brief{
		"gt-abc1": {ID: "gt-abc1", Title: "Known"},
	}, nil)
	out := Snapshot("The known bead gt-abc1 and the unknown gt-zzz9.", lookup)
	if want := []string{"gt-abc1: Known"}; !slices.Equal(blockHeadings(out), want) {
		t.Errorf("headings = %v, want %v\n%s", blockHeadings(out), want, out)
	}
	if strings.Contains(out, "gt-zzz9") {
		t.Errorf("snapshot names an id the lookup does not know:\n%s", out)
	}
}

// TestSnapshotBlockCarriesFields covers the acceptance box for a block's
// content: the id with its title on the heading line, then the status and the
// description.
func TestSnapshotBlockCarriesFields(t *testing.T) {
	t.Parallel()
	lookup := anyBrief(Brief{
		ID:          "gt-abc1",
		Title:       "Assemble the snapshot",
		Status:      "in_progress",
		Description: "The seat reads this instead of a bead.",
	})
	out := Snapshot("see gt-abc1", lookup)
	want := "## Beads named in this task\n\n" +
		"### gt-abc1: Assemble the snapshot\n\n" +
		"Status: in_progress\n\n" +
		"The seat reads this instead of a bead.\n"
	if out != want {
		t.Errorf("Snapshot() = %q, want %q", out, want)
	}
}

// TestSnapshotOmitsEmptyFields is one row per field a caller can leave out:
// the line for that field is left out rather than standing empty, and a title
// or id holding a newline stays on the heading line.
func TestSnapshotOmitsEmptyFields(t *testing.T) {
	t.Parallel()
	head := "## Beads named in this task\n\n### "
	tests := []struct {
		name  string
		brief Brief
		want  string
	}{
		{"heading only", Brief{ID: "gt-abc1"}, head + "gt-abc1\n"},
		{"no status", Brief{ID: "gt-abc1", Description: "Body."}, head + "gt-abc1\n\nBody.\n"},
		{"no description", Brief{ID: "gt-abc1", Title: "T", Status: "open"}, head + "gt-abc1: T\n\nStatus: open\n"},
		{"newline in title", Brief{ID: "gt-abc1", Title: "two\nlines"}, head + "gt-abc1: two lines\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Snapshot("gt-abc1", anyBrief(tt.brief)); got != tt.want {
				t.Errorf("Snapshot() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSnapshotNamesTheMatchedID covers a brief that carries no id: the heading
// falls back to the id the task text named, so a block never stands bare.
func TestSnapshotNamesTheMatchedID(t *testing.T) {
	t.Parallel()
	out := Snapshot("gt-abc1", anyBrief(Brief{Title: "No id of its own"}))
	if want := "## Beads named in this task\n\n### gt-abc1: No id of its own\n"; out != want {
		t.Errorf("Snapshot() = %q, want %q", out, want)
	}
}

// TestSnapshotIDBoundaries is one row per shape a bead id takes in a task text,
// or a token that only looks like one: a fragment of a longer token is never
// offered to the lookup as a bead of its own.
func TestSnapshotIDBoundaries(t *testing.T) {
	t.Parallel()
	any := Brief{Title: "t"}
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"named", "Read gt-abc1 before you start.", []string{"gt-abc1: t"}},
		{"ends a sentence", "Read gt-abc1.", []string{"gt-abc1: t"}},
		{"in a path", "polecat/slit/gt-abc1", []string{"gt-abc1: t"}},
		{"branch with a hash", "branch polecat/slit/gt-kuncsk+mwh4rxdz", []string{"gt-kuncsk: t"}},
		{"dotted child", "the child gt-abc1.12 stays", []string{"gt-abc1.12: t"}},
		{"deep child", "the child gt-abc1.12.3 stays", []string{"gt-abc1.12.3: t"}},
		{"child and parent", "gt-abc1.12 and gt-abc1", []string{"gt-abc1.12: t", "gt-abc1: t"}},
		{"digits in the core", "gt-123 is a bead", []string{"gt-123: t"}},
		{"fragment of a longer id", "gt-wisp-yzuwzb", nil},
		{"glued after a digit", "1gt-abc1", nil},
		{"glued after a dot", "x.gt-abc1", nil},
		{"over-long core", "gt-abc1xyz", nil},
		{"prefix over four letters", "abcde-xyz", nil},
		{"prefix under two letters", "a-bcd", nil},
		{"core under three", "gt-ab", nil},
		{"uppercase", "GT-ABC1", nil},
		{"no id at all", "no bead here", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := blockHeadings(Snapshot(tt.text, anyBrief(any))); !slices.Equal(got, tt.want) {
				t.Errorf("headings for %q = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// TestSnapshotNoBeads covers the acceptance box for a text that names no bead:
// the snapshot is empty, and a nil lookup is safe because it is never called.
func TestSnapshotNoBeads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{"empty text", ""},
		{"prose with no id", "Read the task and return an outbox."},
		{"fragment only", "gt-wisp-yzuwzb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if out := Snapshot(tt.text, nil); out != "" {
				t.Errorf("Snapshot(%q) = %q, want the empty snapshot", tt.text, out)
			}
		})
	}
}

// TestSnapshotCapsDescription covers the acceptance box for the per-bead cap: a
// long description is cut to maxBriefBytes, marker included, and the marker is
// the last line of the block.
func TestSnapshotCapsDescription(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 4*maxBriefBytes)
	out := Snapshot("gt-abc1", anyBrief(Brief{ID: "gt-abc1", Title: "Long", Description: long}))
	if !strings.HasSuffix(out, briefTruncationMarker()+"\n") {
		t.Errorf("block does not end with the description marker:\n%s", tail(out))
	}
	// The heading and status lines ride along with the capped description; a
	// description cut at 4 KiB instead of 2 KiB would overrun this by a mile.
	if limit := maxBriefBytes + 200; len(out) > limit {
		t.Errorf("snapshot of one description is %d bytes, want at most %d", len(out), limit)
	}
}

// TestSnapshotDescriptionCutKeepsUTF8 covers a description cut mid-rune: the
// block still ends inside a rune, so the file a seat reads is valid UTF-8.
func TestSnapshotDescriptionCutKeepsUTF8(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("→", 4*maxBriefBytes)
	out := Snapshot("gt-abc1", anyBrief(Brief{ID: "gt-abc1", Description: long}))
	if !utf8.ValidString(out) {
		t.Fatalf("capped description is not valid UTF-8:\n%q", tail(out))
	}
	if !strings.HasSuffix(out, briefTruncationMarker()+"\n") {
		t.Errorf("block does not end with the description marker:\n%s", tail(out))
	}
}

// TestSnapshotCapsWhole covers the acceptance box for the snapshot cap: twenty
// beads of capped descriptions exceed maxSnapshotBytes, so the snapshot is cut
// and the marker stands on its own last line.
func TestSnapshotCapsWhole(t *testing.T) {
	t.Parallel()
	known := make(map[string]Brief)
	ids := make([]string, 0, 20)
	for i := range 20 {
		id := fmt.Sprintf("gt-b%04d", i)
		known[id] = Brief{ID: id, Title: "Bead", Status: "open", Description: strings.Repeat("d", 3*maxBriefBytes)}
		ids = append(ids, id)
	}
	out := Snapshot(strings.Join(ids, " "), fakeLookup(known, nil))
	if len(out) > maxSnapshotBytes {
		t.Errorf("snapshot is %d bytes, want at most %d", len(out), maxSnapshotBytes)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if last := lines[len(lines)-1]; last != snapshotTruncationMarker() {
		t.Errorf("last line = %q, want %q", last, snapshotTruncationMarker())
	}
	if !utf8.ValidString(out) {
		t.Error("capped snapshot is not valid UTF-8")
	}
}

// tail is the last few hundred bytes of a long output, so a failure prints the
// cut instead of a wall of filler.
func tail(s string) string {
	const keep = 300
	if len(s) <= keep {
		return s
	}
	return "..." + s[len(s)-keep:]
}
