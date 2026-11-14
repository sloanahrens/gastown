package polecat

import (
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// TestParseAcceptanceBoxes covers the acceptance box for the parser: checked
// and unchecked boxes in order, with every other line skipped.
func TestParseAcceptanceBoxes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []taskfile.Box
	}{
		{
			name: "checked and unchecked boxes keep their order and state",
			text: "- [x] first\n- [ ] second\n- [X] third\n- [ ] fourth",
			want: []taskfile.Box{
				{Text: "first", Ticked: true},
				{Text: "second"},
				{Text: "third", Ticked: true},
				{Text: "fourth"},
			},
		},
		{
			name: "every other line is skipped",
			text: "## Acceptance\n" +
				"\n" +
				"prose about the boxes\n" +
				"- a plain bullet with no box\n" +
				"1. a numbered item with no box\n" +
				"| - [x] a table cell is not a box |\n" +
				"- [y] an unknown box marks nothing\n" +
				"[x] a bare box with no list marker\n" +
				"- [ ] the only box",
			want: []taskfile.Box{{Text: "the only box"}},
		},
		{
			name: "nested and numbered items are boxes too",
			text: "- [x] outer\n  - [ ] nested\n1. [ ] numbered\n2) [x] numbered ticked",
			want: []taskfile.Box{
				{Text: "outer", Ticked: true},
				{Text: "nested"},
				{Text: "numbered"},
				{Text: "numbered ticked", Ticked: true},
			},
		},
		{
			name: "the box text is trimmed",
			text: "- [ ]   spaced text  \r\n- [x]\t tabbed",
			want: []taskfile.Box{
				{Text: "spaced text"},
				{Text: "tabbed", Ticked: true},
			},
		},
		{
			name: "a box with no text is kept as an empty one",
			text: "- [ ]\n- [x]",
			want: []taskfile.Box{
				{},
				{Ticked: true},
			},
		},
		{
			name: "boxes inside a fenced block are example text",
			text: "```\n- [ ] not a box\n```\n- [ ] a box",
			want: []taskfile.Box{{Text: "a box"}},
		},
		{
			name: "no checkbox lines",
			text: "prose only\n- a plain bullet\n1. a numbered item",
		},
		{
			name: "empty field",
			text: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseAcceptanceBoxes(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseAcceptanceBoxes(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}

// TestTaskInputFromBeadBoxesNameTheHostsBox covers the parity the box text
// promises: a box TASK.md shows is a box the host's TickAcceptanceBox finds, so
// a seat's tick lands on the box it read. An open box changes the text; a
// ticked one is a no-op there.
func TestTaskInputFromBeadBoxesNameTheHostsBox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{name: "a plain box", text: "- [ ] first\n- [ ] second"},
		{name: "padding around the text", text: "1. [ ]   spaced text  \n- [ ] second"},
		{name: "a tab after the box", text: "- [ ]\ta tabbed box\n- [ ] other"},
		{name: "a nested box under a ticked one", text: "- [x] outer\n  - [ ] nested"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			boxes := taskInputFromBead(beadTaskFields{Acceptance: tt.text}).Boxes
			if len(boxes) == 0 {
				t.Fatalf("taskInputFromBead(%q).Boxes is empty", tt.text)
			}
			for i, box := range boxes {
				got, err := beads.TickAcceptanceBox(tt.text, box.Text)
				if err != nil {
					t.Fatalf("TickAcceptanceBox(%q) with box %d %q = error %v, want the box found", tt.text, i, box.Text, err)
				}
				if box.Ticked && got != tt.text {
					t.Errorf("TickAcceptanceBox(%q) with ticked box %q = %q, want it unchanged", tt.text, box.Text, got)
				}
				if !box.Ticked && got == tt.text {
					t.Errorf("TickAcceptanceBox(%q) with open box %q changed nothing", tt.text, box.Text)
				}
			}
		})
	}
}

// TestTaskInputFromBead covers the acceptance box for the mapping: a full bead
// and a bead with no notes and no comments each build the taskfile Input.
func TestTaskInputFromBead(t *testing.T) {
	t.Parallel()
	commentTime := time.Date(2026, 11, 14, 22, 40, 0, 0, time.UTC)
	tests := []struct {
		name   string
		fields beadTaskFields
		want   taskfile.Input
	}{
		{
			name: "a full bead",
			fields: beadTaskFields{
				ID:          "gt-abc1",
				Title:       "Build the task input from a bead",
				Description: "The seat reads its task from TASK.md instead of a bead.",
				Acceptance:  "- [x] the boxes carry their ticked state\n- [ ] the comments carry their author",
				Notes:       []string{"first note", "second note"},
				Comments: []beadTaskComment{
					{Author: "daemon", Time: commentTime, Text: "SP ESTIMATE v1 sp:8"},
					{Author: "overseer", Text: "a comment with no time"},
				},
			},
			want: taskfile.Input{
				BeadID:      "gt-abc1",
				Title:       "Build the task input from a bead",
				Description: "The seat reads its task from TASK.md instead of a bead.",
				Boxes: []taskfile.Box{
					{Text: "the boxes carry their ticked state", Ticked: true},
					{Text: "the comments carry their author"},
				},
				Notes: []taskfile.Note{
					{Text: "first note"},
					{Text: "second note"},
				},
				Comments: []taskfile.Comment{
					{Author: "daemon", Time: commentTime, Text: "SP ESTIMATE v1 sp:8"},
					{Author: "overseer", Text: "a comment with no time"},
				},
			},
		},
		{
			name: "a bead with no notes and no comments",
			fields: beadTaskFields{
				ID:          "gt-min1",
				Title:       "A bead with no lists",
				Description: "Only a description.",
				Acceptance:  "- [ ] one box",
			},
			want: taskfile.Input{
				BeadID:      "gt-min1",
				Title:       "A bead with no lists",
				Description: "Only a description.",
				Boxes:       []taskfile.Box{{Text: "one box"}},
			},
		},
		{
			name: "an acceptance field with no boxes",
			fields: beadTaskFields{
				Title:      "Prose acceptance",
				Acceptance: "the criterion is written as a sentence",
			},
			want: taskfile.Input{Title: "Prose acceptance"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := taskInputFromBead(tt.fields); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("taskInputFromBead(%+v) = %+v, want %+v", tt.fields, got, tt.want)
			}
		})
	}
}
