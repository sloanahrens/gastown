package reaper

import (
	"strings"
	"testing"
)

func TestNextOpenWispAlertState(t *testing.T) {
	t.Parallel()

	previous := &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 138, Databases: 2}}

	tests := []struct {
		name        string
		previous    *OpenWispAlertState
		current     OpenWispSample
		want        bool
		wantBase    int
		wantHeld    int
		wantCompare bool // the recorded baseline is not this cycle's sample
	}{
		{
			name:     "no baseline yet",
			current:  OpenWispSample{OpenWisps: 5000, Databases: 2},
			wantBase: 5000,
		},
		{
			name:     "steady state, the count the old threshold false-alarmed on",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 138, Databases: 2},
			wantBase: 138,
		},
		{
			name:     "growth under the factor is held, not absorbed",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 200, Databases: 2},
			wantBase: 138,
			wantHeld: 1,
		},
		{
			name:     "exactly the factor, under the floor",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 276, Databases: 2},
			wantBase: 138,
			wantHeld: 1,
		},
		{
			name:     "at the factor and past the floor",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 400, Databases: 2},
			want:     true,
			wantBase: 400,
		},
		{
			name:     "a small count doubling is noise, not accumulation",
			previous: &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 3, Databases: 1}},
			current:  OpenWispSample{OpenWisps: 6, Databases: 1},
			wantBase: 3,
			wantHeld: 1,
		},
		{
			name:     "a big absolute jump from a small base still clears the floor",
			previous: &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 400, Databases: 1}},
			current:  OpenWispSample{OpenWisps: 810, Databases: 1},
			want:     true,
			wantBase: 810,
		},
		{
			name:     "shrinking re-anchors on the floor the town settled at",
			previous: &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 276, Databases: 2}, Held: 4},
			current:  OpenWispSample{OpenWisps: 111, Databases: 2},
			wantBase: 111,
		},
		{
			name:     "a dry run does not compare against a live cycle",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 900, Databases: 2, DryRun: true},
			wantBase: 900,
		},
		{
			name:     "a wider database set is not growth",
			previous: previous,
			current:  OpenWispSample{OpenWisps: 900, Databases: 5},
			wantBase: 900,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			next, alert, detail := NextOpenWispAlertState(tt.previous, tt.current)
			if alert != tt.want {
				t.Fatalf("NextOpenWispAlertState(%+v, %+v) alerted = %v, want %v", tt.previous, tt.current, alert, tt.want)
			}
			if next.Baseline.OpenWisps != tt.wantBase {
				t.Errorf("recorded baseline = %+v, want a count of %d", next.Baseline, tt.wantBase)
			}
			if next.Baseline.Databases != tt.current.Databases {
				t.Errorf("recorded baseline = %+v, want this cycle's database count", next.Baseline)
			}
			if next.Held != tt.wantHeld {
				t.Errorf("recorded Held = %d, want %d", next.Held, tt.wantHeld)
			}
			if !alert && detail != "" {
				t.Errorf("a non-alert must carry no description, got %q", detail)
			}
			if alert && detail == "" {
				t.Error("an alert must describe the growth it reports")
			}
		})
	}
}

// TestNextOpenWispAlertStateCatchesSlowAccumulation is the reason the baseline
// is held rather than replaced every cycle (gt-11kyy): a count that climbs in
// steps smaller than either threshold never doubles between two cycles, so a
// baseline that followed it could not report the climb at all.
func TestNextOpenWispAlertStateCatchesSlowAccumulation(t *testing.T) {
	t.Parallel()

	const (
		start  = 138
		step   = 80
		cycles = 40
	)

	if step >= AlertGrowthFloor {
		t.Fatalf("the test's step of %d is large enough to alert on its own", step)
	}

	var state *OpenWispAlertState
	alerted := -1
	for cycle := 0; cycle < cycles; cycle++ {
		count := start + cycle*step
		next, alert, _ := NextOpenWispAlertState(state, OpenWispSample{OpenWisps: count, Databases: 2})
		state = &next
		if alert {
			alerted = cycle
			break
		}
	}

	if alerted < 0 {
		t.Fatalf("%d cycles of +%d open wisps never alerted", cycles, step)
	}
	if alerted < 2 {
		t.Fatalf("alerted after %d cycles, want a climb spread over several", alerted)
	}
}

// A rise that stops short of the thresholds is not accumulation, and the
// baseline it re-anchors on is the count the town actually settled at.
func TestNextOpenWispAlertStateReAnchorsWhenTheRiseStopsShort(t *testing.T) {
	t.Parallel()

	state := OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 138, Databases: 2}}
	for i := 0; i < 20; i++ {
		next, alert, detail := NextOpenWispAlertState(&state, OpenWispSample{OpenWisps: 300, Databases: 2})
		if alert {
			t.Fatalf("a steady 300 against a 138 baseline alerted: %s", detail)
		}
		state = next
	}
	if state.Baseline.OpenWisps != 138 {
		t.Fatalf("baseline = %+v, want the 138 the rise is measured from", state.Baseline)
	}

	// The town reaps below the baseline, and the floor it settled on becomes
	// what the next rise is measured from.
	next, _, _ := NextOpenWispAlertState(&state, OpenWispSample{OpenWisps: 120, Databases: 2})
	if next.Baseline.OpenWisps != 120 || next.Held != 0 {
		t.Fatalf("after a fall the state is %+v, want a fresh 120 baseline", next)
	}

	next, alert, _ := NextOpenWispAlertState(&next, OpenWispSample{OpenWisps: 500, Databases: 2})
	if !alert {
		t.Fatal("120 to 500 is growth past the factor and the floor")
	}
	if next.Baseline.OpenWisps != 500 {
		t.Fatalf("after an alert the baseline is %+v, want this cycle's 500: a town that stays high reports the rise once", next.Baseline)
	}
}

// The threshold the escalation reported sat below the town's steady state, so
// the alert fired on a healthy town and said nothing (gt-11kyy). A steady state
// large enough to have tripped any absolute threshold must not alert on its
// own: only the change from the baseline can.
func TestNextOpenWispAlertStateIsSilentAtSteadyStateAboveAnyAbsoluteThreshold(t *testing.T) {
	t.Parallel()

	for _, count := range []int{138, 1966, 3000, 12000} {
		state := &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: count, Databases: 6}}
		for i := 0; i < 5; i++ {
			next, alert, detail := NextOpenWispAlertState(state, OpenWispSample{OpenWisps: count, Databases: 6})
			if alert {
				t.Errorf("steady state of %d alerted: %s", count, detail)
			}
			state = &next
		}
	}
}

func TestNextOpenWispAlertStateDescribesTheGrowth(t *testing.T) {
	t.Parallel()

	_, alert, detail := NextOpenWispAlertState(
		&OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 138, Databases: 2}},
		OpenWispSample{OpenWisps: 900, Databases: 2},
	)
	if !alert {
		t.Fatal("138 to 900 is growth past the factor and the floor")
	}
	for _, want := range []string{"138", "900", "+762", "1 cycle", "2 database"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not name %q", detail, want)
		}
	}
}

// A held baseline reports how long the climb took, so a reader can tell a
// one-cycle jump from a month of drift.
func TestNextOpenWispAlertStateReportsHowManyCyclesTheClimbTook(t *testing.T) {
	t.Parallel()

	state := &OpenWispAlertState{Baseline: OpenWispSample{OpenWisps: 138, Databases: 2}, Held: 6}
	_, alert, detail := NextOpenWispAlertState(state, OpenWispSample{OpenWisps: 900, Databases: 2})
	if !alert {
		t.Fatal("138 to 900 is growth past the factor and the floor")
	}
	if !strings.Contains(detail, "7 cycle") {
		t.Errorf("detail %q does not report the 7 cycles the baseline stood", detail)
	}
}
