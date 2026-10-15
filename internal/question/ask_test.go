package question

import (
	"strings"
	"testing"
)

// The priority a question is pushed at is the one the human must answer before
// the work it holds can move: P0, P1 and P2 are pushed, P3 and P4 wait on the
// dashboard (gt-71714).
func TestPushPriority(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		priority int
		want     bool
	}{
		{0, true}, {1, true}, {2, true}, {3, false}, {4, false},
		// Outside bd's range is not a question anyone filed.
		{-1, false}, {5, false},
	} {
		if got := PushPriority(tc.priority); got != tc.want {
			t.Errorf("PushPriority(%d) = %v, want %v", tc.priority, got, tc.want)
		}
	}
}

// A pushed title names the dashboard row it is about, and carries no more than
// the dashboard would draw: the body never reaches it, a newline cannot forge a
// second line of the message, and the control and invisible format characters
// that reorder what a reader sees are gone (gt-71714).
func TestPushTitleIsTheDashboardRowOnOneLine(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		title string
		want  string
	}{
		"a plain title":     {"Retry policy", "Question hq-t1 P1: Retry policy"},
		"a wrapped title":   {"Retry\na rejected landing\nonce?", "Question hq-t1 P1: Retry a rejected landing once?"},
		"an escape in it":   {"a\x1b[31mred\x1b[0m title", "Question hq-t1 P1: a [31mred [0m title"},
		"a bidi override":   {"retry‮gnihsihs", "Question hq-t1 P1: retry gnihsihs"},
		"an over-long one":  {strings.Repeat("é", FieldMax+200), "Question hq-t1 P1: " + strings.Repeat("é", FieldMax)},
		"a title of spaces": {"   ", "Question hq-t1 P1: "},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := PushTitle("hq-t1", 1, tc.title)
			if got != tc.want {
				t.Errorf("PushTitle = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "\n") || strings.ContainsFunc(got, func(r rune) bool { return r < ' ' }) {
				t.Errorf("PushTitle = %q, want one line with no control characters", got)
			}
		})
	}
}
