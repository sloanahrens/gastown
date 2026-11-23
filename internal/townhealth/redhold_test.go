package townhealth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The dispatcher field names the red hold the moment one stands, before the
// tick window can call a held dispatcher "silent" and before a young daemon
// reads green; an empty RedHold leaves the operator's hold green as before
// (gt-xq3s9u).
func TestHealthRedHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		rec                     DispatchRecord
		started                 time.Time
		verdict                 Verdict
		value, detail, linePart string
	}{
		{"fresh ticks", DispatchRecord{Active: true, RedHold: "sweep-red, main-red", Ticks: []DispatchTick{seeded(ago(2*time.Minute), 1)}}, ago(time.Hour), Red,
			"held by red hold (sweep-red, main-red)", "the red hold stands: sweep-red, main-red", "dispatch=held by red hold (sweep-red, main-red)"},
		{"young daemon", DispatchRecord{Active: true, RedHold: "sweep-red"}, ago(2 * time.Minute), Red,
			"held by red hold (sweep-red)", "the red hold stands: sweep-red", "dispatch=held by red hold (sweep-red)"},
		{"stall and operator hold", DispatchRecord{Hold: "town ESTOP active", RedHold: "main-red", Ticks: []DispatchTick{seeded(ago(2*time.Minute), 0)}}, ago(time.Hour), Red,
			"held by red hold (main-red)", "the red hold stands: main-red", "dispatch=held by red hold (main-red)"},
		{"empty RedHold keeps the operator hold green", DispatchRecord{Hold: "operator dispatch hold present (/town/seat-refill.hold); remove it to resume"}, ago(time.Hour), Green,
			"held", "operator dispatch hold", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := healthy()
			f.dispatch, f.started = tc.rec, tc.started
			r := Compute(context.Background(), inputs(f))
			got := field(t, r, FieldDispatch)
			if got.Verdict != tc.verdict || got.Value != tc.value || got.Tag != Recorded {
				t.Errorf("dispatch = %+v, want %s %q RECORDED", got, tc.verdict, tc.value)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to carry %q", got.Detail, tc.detail)
			}
			line := Line(r, now, DefaultStaleAfter)
			if tc.linePart == "" {
				if strings.Contains(line, "dispatch=") {
					t.Errorf("line reports an unremarkable dispatcher: %q", line)
				}
			} else if !strings.Contains(line, tc.linePart) {
				t.Errorf("line = %q, want it to carry %q", line, tc.linePart)
			}
		})
	}
}
