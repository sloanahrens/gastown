package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestSlingTimerStepLines pins the line format the operator greps for when
// attributing a slow sling: one line per step, in call order, with the step's
// own duration and the running total, both measured from the timer's clock.
func TestSlingTimerStepLines(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 10, 20, 19, 0, time.UTC)
	ticks := []time.Time{t0, t0.Add(1 * time.Second), t0.Add(3 * time.Second)}
	i := 0
	clock := func() time.Time { v := ticks[i]; i++; return v }

	var out bytes.Buffer
	tm := newSlingTimerWithClock(&out, clock)
	tm.step("admission")
	tm.step("allocate")

	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := []string{
		"[sling] step admission took 1s (total 1s)",
		"[sling] step allocate took 2s (total 3s)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(got), got, len(want))
	}
	for n := range want {
		if got[n] != want[n] {
			t.Errorf("line %d: got %q, want %q", n, got[n], want[n])
		}
	}
}

// A nil timer must be safe to call: every caller on the spawn path guards
// nothing, so the zero value has to be a no-op rather than a nil dereference.
func TestSlingTimerNilIsNoop(t *testing.T) {
	var tm *slingTimer
	tm.step("anything") // must not panic
}
