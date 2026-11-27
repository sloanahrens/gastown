package rehearsal

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// withinBars is a measured record inside every ruled bar.
func withinBars() Record {
	return Record{
		Slings:   slingsOf(rep(setSize, 100)...),
		Gates:    gatesOf(GatePass, 400),
		Landings: []Landing{{Bead: "l1", Landed: true}},
		Load:     Load{Samples: 10},
	}
}

// rep returns n copies of s.
func rep(n int, s float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// slingsOf returns one ok sling per duration.
func slingsOf(d ...float64) []Sling {
	var out []Sling
	for i, s := range d {
		out = append(out, Sling{Bead: fmt.Sprint("s", i), Outcome: SlingOK, Seconds: s})
	}
	return out
}

// gatesOf returns one gate per duration, all with verdict v.
func gatesOf(v string, d ...float64) []Gate {
	var out []Gate
	for i, s := range d {
		out = append(out, Gate{Bead: fmt.Sprint("g", i), Verdict: v, Seconds: s})
	}
	return out
}

// assertVerdict fails unless v passes with no reason when want is empty, and
// otherwise fails with exactly one reason containing want.
func assertVerdict(t *testing.T, v Verdict, want string) {
	t.Helper()
	if want == "" {
		if !v.Pass || len(v.Reasons) != 0 {
			t.Fatalf("Judge = %+v; want a pass with no reason", v)
		}
		return
	}
	if v.Pass || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], want) {
		t.Fatalf("Judge = %+v; want a fail with one reason containing %q", v, want)
	}
}

func TestRehearsalJudgeWithinBars(t *testing.T) {
	t.Parallel()
	assertVerdict(t, Judge(withinBars()), "")
}

func TestRehearsalJudgeZeroTolerance(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*Record)
		want string
	}{
		{name: "sling failed", edit: func(r *Record) { r.Slings[0].Outcome = SlingFailed }, want: `"failed" is not ok`},
		{name: "sling killed", edit: func(r *Record) { r.Slings[0].Outcome = SlingKilled }, want: `"killed" is not ok`},
		{name: "stranded seat", edit: func(r *Record) { r.Stranded = []string{"seat-3"} }, want: "stranded seat seat-3"},
		{name: "landing CI failed", edit: func(r *Record) { r.Landings[0].CIFailed = true }, want: "landing l1 failed CI"},
		{name: "failed call", edit: func(r *Record) { r.FailedCalls = []string{"sling: boom"} }, want: "failed call: sling: boom"},
		{name: "intervention", edit: func(r *Record) { r.Interventions = []string{"killed a seat"} }, want: "intervention: killed a seat"},
		{name: "stopped run", edit: func(r *Record) { r.Stopped, r.StopReason = true, "window" }, want: "run stopped: window"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := withinBars()
			tt.edit(&r)
			assertVerdict(t, Judge(r), tt.want)
		})
	}
}

// TestRehearsalJudgeNotLanded checks that every landing the run never landed
// fails with its own reason, and that a bead which also failed CI keeps both.
func TestRehearsalJudgeNotLanded(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		landings []Landing
		want     []string
	}{
		{name: "one bead not landed", landings: []Landing{{Bead: "l1", Landed: true}, {Bead: "l2"}},
			want: []string{"bead l2 did not land within the run"}},
		{name: "two beads not landed each add a reason", landings: []Landing{{Bead: "l1", Landed: true}, {Bead: "l2"}, {Bead: "l3"}},
			want: []string{"bead l2 did not land within the run", "bead l3 did not land within the run"}},
		{name: "a bead not landed that also failed CI adds both reasons", landings: []Landing{{Bead: "l1", Landed: true}, {Bead: "l2", CIFailed: true}},
			want: []string{"landing l2 failed CI", "bead l2 did not land within the run"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := withinBars()
			r.Landings = tt.landings
			v := Judge(r)
			if v.Pass {
				t.Fatalf("Judge = %+v; want a fail", v)
			}
			if !slices.Equal(v.Reasons, tt.want) {
				t.Errorf("reasons = %q; want %q", v.Reasons, tt.want)
			}
		})
	}
}

func TestRehearsalJudgeP90(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*Record)
		want string // reason substring; empty means the run passes
	}{
		{name: "sling p90 at the limit passes", edit: func(r *Record) { r.Slings = slingsOf(rep(setSize, 120)...) }},
		{name: "sling p90 one second over fails", edit: func(r *Record) { r.Slings = slingsOf(rep(setSize, 121)...) }, want: "sling p90 121 s is over 120 s"},
		{name: "one slow sling in sixteen is outside nearest rank", edit: func(r *Record) { r.Slings = slingsOf(append(rep(setSize-1, 100), 500)...) }},
		{name: "two slow slings in sixteen fail the sling p90", edit: func(r *Record) { r.Slings = slingsOf(append(rep(setSize-2, 100), 500, 500)...) }, want: "sling p90 500 s is over 120 s"},
		{name: "two slow slings in seventeen fail the sling p90", edit: func(r *Record) { r.Slings = slingsOf(append(rep(setSize-1, 100), 500, 500)...) }, want: "sling p90 500 s is over 120 s"},
		{name: "slow slings listed first still count", edit: func(r *Record) { r.Slings = slingsOf(append([]float64{500, 500}, rep(setSize-2, 100)...)...) }, want: "sling p90 500 s is over 120 s"},
		{name: "presubmit p90 at the limit passes", edit: func(r *Record) { r.Gates = gatesOf(GatePass, rep(setSize, 472)...) }},
		{name: "presubmit p90 one second over fails", edit: func(r *Record) { r.Gates = gatesOf(GatePass, rep(setSize, 473)...) }, want: "presubmit p90 473 s is over 472 s"},
		{name: "slow passing gates count", edit: func(r *Record) { r.Gates = gatesOf(GatePass, append(rep(setSize-2, 100), 500, 500)...) }, want: "presubmit p90 500 s is over 472 s"},
		{name: "slow failing gates count", edit: func(r *Record) { r.Gates = gatesOf(GateFail, append(rep(setSize-2, 100), 500, 500)...) }, want: "presubmit p90 500 s is over 472 s"},
		{name: "zero-second gates beside one slow gate still fail", edit: func(r *Record) { r.Gates = gatesOf(GatePass, append(rep(setSize-1, 0), 500)...) }, want: "presubmit p90 500 s is over 472 s"},
		{name: "zero-second gates beside one measured gate within bars pass", edit: func(r *Record) { r.Gates = gatesOf(GatePass, append(rep(setSize-1, 0), 400)...) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := withinBars()
			tt.edit(&r)
			assertVerdict(t, Judge(r), tt.want)
		})
	}
}

func TestRehearsalJudgeUnmeasured(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*Record)
		want string
	}{
		{name: "fifteen slings", edit: func(r *Record) { r.Slings = slingsOf(rep(setSize-1, 100)...) }, want: "only 15 slings, want 16: the run is unmeasured"},
		{name: "no slings", edit: func(r *Record) { r.Slings = nil }, want: "only 0 slings, want 16: the run is unmeasured"},
		{name: "no gate figure", edit: func(r *Record) { r.Gates = nil }, want: "no gate figure: the run is unmeasured"},
		{name: "every gate zero seconds", edit: func(r *Record) { r.Gates = gatesOf(GatePass, rep(setSize, 0)...) }, want: "no gate figure: the run is unmeasured"},
		{name: "no load samples", edit: func(r *Record) { r.Load = Load{} }, want: "no host load samples: the run is unmeasured"},
		{name: "negative load samples", edit: func(r *Record) { r.Load = Load{Samples: -1} }, want: "no host load samples: the run is unmeasured"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := withinBars()
			tt.edit(&r)
			assertVerdict(t, Judge(r), tt.want)
		})
	}
}

// TestRehearsalJudgeEveryBrokenRuleAddsAReason checks that three unmet measures
// give three reasons, not one: an unmeasured run keeps every refusal it earned.
func TestRehearsalJudgeEveryBrokenRuleAddsAReason(t *testing.T) {
	t.Parallel()
	r := withinBars()
	r.Slings, r.Gates, r.Load = slingsOf(rep(setSize-1, 100)...), nil, Load{}
	v := Judge(r)
	if v.Pass {
		t.Fatalf("Judge = %+v; want a fail", v)
	}
	for _, want := range []string{"only 15 slings", "no gate figure", "no host load samples"} {
		found := false
		for _, reason := range v.Reasons {
			found = found || strings.Contains(reason, want)
		}
		if !found {
			t.Errorf("reasons %q; want one containing %q", v.Reasons, want)
		}
	}
	if len(v.Reasons) != 3 {
		t.Errorf("reasons = %q; want exactly the 3 unmeasured reasons", v.Reasons)
	}
}
