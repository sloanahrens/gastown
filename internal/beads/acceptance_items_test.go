package beads

import (
	"reflect"
	"testing"
)

// TestAcceptanceItemsDescribeEachItem: each item reports whether it is nested,
// whether it carries a checkbox, and the trimmed text after that checkbox, and
// a checkbox is checked only for "x" or "X".
func TestAcceptanceItemsDescribeEachItem(t *testing.T) {
	t.Parallel()
	got := AcceptanceItems("- [x] a\n  - [ ] b\n1. c\n* [X] d\nprose line")
	want := []AcceptanceItem{
		{Line: "- [x] a", Boxed: true, Checked: true, Box: "a"},
		{Line: "- [ ] b", Nested: true, Boxed: true, Box: "b"},
		{Line: "1. c"},
		{Line: "* [X] d", Boxed: true, Checked: true, Box: "d"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AcceptanceItems = %+v, want %+v", got, want)
	}
}

// TestAcceptanceBoxesGiveEachBoxItsLineAndOffset: each box reports the line it
// sits on, the byte offset of its "[" in that line, and whether it is ticked,
// in the order AcceptanceItems reads the boxes.
func TestAcceptanceBoxesGiveEachBoxItsLineAndOffset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []AcceptanceBox
	}{
		{
			name: "boxes on consecutive lines",
			text: "- [ ] a\n- [x] b",
			want: []AcceptanceBox{{Line: 0, At: 2}, {Line: 1, At: 2, Checked: true}},
		},
		{
			name: "an indented box offsets into the raw line",
			text: "  - [ ] a",
			want: []AcceptanceBox{{Line: 0, At: 4}},
		},
		{
			name: "a closed fence's copy is skipped and the real box keeps its line",
			text: "- [ ] a\n```\n- [ ] b\n```\n- [ ] b",
			want: []AcceptanceBox{{Line: 0, At: 2}, {Line: 4, At: 2}},
		},
		{
			name: "an unfinished fence reads as text, so its next line's box counts",
			text: "```\n- [ ] a",
			want: []AcceptanceBox{{Line: 1, At: 2}},
		},
		{
			name: "an unboxed item between boxes is not a box",
			text: "- [ ] a\n- b\n- [ ] c",
			want: []AcceptanceBox{{Line: 0, At: 2}, {Line: 2, At: 2}},
		},
		{
			name: "no box",
			text: "- a\n1. b",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AcceptanceBoxes(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AcceptanceBoxes(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}

// TestFenceMarkerReadsOnlyAFenceLine: a fence is three or more backticks or
// tildes; a backtick fence's info string may not hold a backtick.
func TestFenceMarkerReadsOnlyAFenceLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want string
	}{
		{"```", "```"},
		{"```go", "```"},
		{"~~~~ rust", "~~~~"},
		{"``", ""},
		{"```x`y", ""},
		{"- [ ] a", ""},
	} {
		if got := FenceMarker(tc.line); got != tc.want {
			t.Errorf("FenceMarker(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
