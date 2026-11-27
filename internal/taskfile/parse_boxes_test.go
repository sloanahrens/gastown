package taskfile

import (
	"reflect"
	"strings"
	"testing"
)

// The round trip: every box Assemble renders parses back in order, with its
// text (multi-line, with a blank line and indented lines inside it) and its
// ticked state. Notes after the section hold box-shaped text that must not be
// read as boxes.
func TestNoBeadAccessParseBoxesRoundTripsAssemble(t *testing.T) {
	t.Parallel()
	want := []Box{
		{Text: "the single-line box is ticked", Ticked: true},
		{Text: "a multi-line box\n  an indented line\n\nafter a blank line"},
		{Text: "\na box whose first line is empty"},
		{Text: "  leading spaces on the first line are kept", Ticked: true},
		{Text: "the last box has no trailing blank"},
	}
	text := Assemble(Input{
		BeadID:      "gt-abc1",
		Title:       "Round trip",
		Description: "A description, with no heading of its own.",
		Boxes:       want,
		Notes:       []Note{{Text: "- [x] a note that looks like a box"}},
		Steps:       []Step{{Text: "- [ ] a step that looks like a box"}},
	})
	got, err := ParseBoxes(text)
	if err != nil {
		t.Fatalf("ParseBoxes(Assemble(...)) error = %v, want none", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseBoxes(Assemble(...)) = %#v, want %#v", got, want)
	}
}

// Text with no acceptance section has no boxes, and that is not an error: a
// bead with no criteria renders no section at all.
func TestNoBeadAccessParseBoxesNoAcceptanceSection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
	}{
		{name: "an empty text", text: ""},
		{name: "a text Assemble renders with no boxes", text: Assemble(Input{BeadID: "gt-abc1", Title: "No criteria"})},
		{name: "a notes section whose items look like boxes", text: "## Notes\n\n- [ ] not a box here\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBoxes(tt.text)
			if err != nil {
				t.Errorf("ParseBoxes(%q) error = %v, want none", tt.text, err)
			}
			if len(got) != 0 {
				t.Errorf("ParseBoxes(%q) = %#v, want no boxes", tt.text, got)
			}
		})
	}
}

// A line in the acceptance section that is not a box, and a section that holds
// no box at all, are errors. Each body below is one refusal, placed under the
// acceptance heading.
func TestNoBeadAccessParseBoxesMalformedLineIsError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{name: "a bullet that is not a box", body: "- foo\n"},
		{name: "an uppercase tick", body: "- [X] the tick is lowercase\n"},
		{name: "a missing space after the bracket", body: "-[ ] no space before the box\n"},
		{name: "a box with no text", body: "- [ ] \n"},
		{name: "a box with no text before the next box", body: "- [ ] \n- [x] next box\n"},
		{name: "a continuation before any box", body: "  orphaned continuation\n- [ ] later box\n"},
		{name: "text at column zero after a box", body: "- [ ] a box\nnot indented\n"},
		{name: "a truncation marker inside the section", body: "- [ ] a box cut short\n[TASK.md truncated: the text exceeded 64 KiB and the rest is not shown]\n"},
		{name: "a heading with nothing under it", body: "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text := "# TASK gt-abc1\n\n## Acceptance\n\n" + tt.body
			if _, err := ParseBoxes(text); err == nil {
				t.Errorf("ParseBoxes(%q) error = nil, want an error", text)
			}
		})
	}
}

// The section ends at the next heading, so a malformed line after it is not part
// of the acceptance boxes and is not an error.
func TestNoBeadAccessParseBoxesStopsAtNextSection(t *testing.T) {
	t.Parallel()
	text := strings.Join([]string{
		"## Acceptance",
		"",
		"- [ ] the only box",
		"",
		"## Notes",
		"",
		"- foo, which is not a box but is under Notes",
		"",
	}, "\n")
	got, err := ParseBoxes(text)
	if err != nil {
		t.Fatalf("ParseBoxes(%q) error = %v, want none", text, err)
	}
	want := []Box{{Text: "the only box"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseBoxes(%q) = %#v, want %#v", text, got, want)
	}
}

// A description line "## Acceptance" comes before the real section, so the boxes
// come from the last such line; the CRLF form parses the same boxes.
func TestNoBeadAccessParseBoxesLastHeadingWins(t *testing.T) {
	t.Parallel()
	want := []Box{{Text: "the real box", Ticked: true}, {Text: "the second real box"}}
	lf := Assemble(Input{BeadID: "gt-abc1", Description: "Quoted:\n\n## Acceptance\n\n- [ ] a description box", Boxes: want})
	for _, text := range []string{lf, strings.ReplaceAll(lf, "\n", "\r\n")} {
		got, err := ParseBoxes(text)
		if err != nil {
			t.Fatalf("ParseBoxes error = %v, want none", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseBoxes = %#v, want %#v", got, want)
		}
	}
}

// A cut before the acceptance heading or inside the section is an error; a cut
// inside a later section keeps every box.
func TestNoBeadAccessParseBoxesCutByCap(t *testing.T) {
	t.Parallel()
	boxes := []Box{{Text: "the first box"}, {Text: "the second box", Ticked: true}}
	long := strings.Repeat("a description line that fills the cap\n", 2000)
	tests := []struct {
		name    string
		input   Input
		want    []Box
		wantErr bool
	}{
		{name: "cut before the acceptance heading", input: Input{BeadID: "gt-abc1", Description: long, Boxes: boxes}, wantErr: true},
		{name: "cut inside the acceptance section", input: Input{BeadID: "gt-abc1", Boxes: []Box{{Text: "a box " + strings.Repeat("x", 70<<10)}}}, wantErr: true},
		{name: "cut inside a later section", input: Input{BeadID: "gt-abc1", Boxes: boxes, Notes: []Note{{Text: strings.Repeat("n", 70<<10)}}}, want: boxes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text := Assemble(tt.input)
			if !strings.Contains(text, truncationMarker()) {
				t.Fatalf("Assemble(...) was not cut, so the case does not test a cut")
			}
			got, err := ParseBoxes(text)
			switch {
			case tt.wantErr && err == nil:
				t.Errorf("ParseBoxes(cut text) = %#v, want an error", got)
			case !tt.wantErr && err != nil:
				t.Fatalf("ParseBoxes(cut text) error = %v, want none", err)
			case !tt.wantErr && !reflect.DeepEqual(got, tt.want):
				t.Errorf("ParseBoxes(cut text) = %#v, want %#v", got, tt.want)
			}
		})
	}
}
