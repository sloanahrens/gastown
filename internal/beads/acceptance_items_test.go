package beads

import (
	"reflect"
	"testing"
)

// TestAcceptanceItemsDescribeEachItem: each item reports whether it is nested
// and whether it carries a checkbox, and a checkbox is checked only for "x" or
// "X".
func TestAcceptanceItemsDescribeEachItem(t *testing.T) {
	t.Parallel()
	got := AcceptanceItems("- [x] a\n  - [ ] b\n1. c\n* [X] d\nprose line")
	want := []AcceptanceItem{
		{Line: "- [x] a", Boxed: true, Checked: true},
		{Line: "- [ ] b", Nested: true, Boxed: true},
		{Line: "1. c"},
		{Line: "* [X] d", Boxed: true, Checked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AcceptanceItems = %+v, want %+v", got, want)
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
