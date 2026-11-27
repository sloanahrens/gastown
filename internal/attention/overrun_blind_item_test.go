package attention

import "testing"

// TestOverrunBlindItem pins the blind item's threshold and its text: no item
// below three blind ticks in a row, and one info item at three or more naming
// the seat count and the first read error (gt-13d389).
func TestOverrunBlindItem(t *testing.T) {
	t.Parallel()

	const refused = "bead read: connection refused"
	for _, tc := range []struct {
		name     string
		streak   int
		seats    int
		firstErr string
		want     bool
		summary  string
	}{
		{"no blind tick", 0, 0, "", false, ""},
		{"two blind ticks", 2, 2, refused, false, ""},
		{"three blind ticks", 3, 2, refused, true,
			"overrun guard blind for 3 ticks in a row: 2 seats could not be judged; first read error: " + refused},
		{"a longer streak", 5, 3, "bead read: deadline exceeded", true,
			"overrun guard blind for 5 ticks in a row: 3 seats could not be judged; first read error: bead read: deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			item, ok := OverrunBlindItem(tc.streak, tc.seats, tc.firstErr)
			if ok != tc.want {
				t.Fatalf("OverrunBlindItem(%d, %d, %q) reports %v, want %v", tc.streak, tc.seats, tc.firstErr, ok, tc.want)
			}
			if !tc.want {
				return
			}
			if item.Key != "overrun-guard:blind" || item.Kind != KindOverrunBlind {
				t.Errorf("item = %q/%q, want key overrun-guard:blind and kind %q", item.Key, item.Kind, KindOverrunBlind)
			}
			if f, found := Flags(RuleFor(item)); !found || f.Handled != HandledInfo || f.Wake {
				t.Errorf("Flags(RuleFor(item)) = %+v (found %v), want an info record that wakes no one", f, found)
			}
			if item.Severity != SeverityLow {
				t.Errorf("Severity = %q, want %q for an info record", item.Severity, SeverityLow)
			}
			if item.Summary != tc.summary {
				t.Errorf("Summary = %q, want %q", item.Summary, tc.summary)
			}
		})
	}
}
