package rehearsal

import (
	"fmt"
	"strings"
	"testing"
)

// withinBars is a record inside every ruled bar.
func withinBars() Record {
	return Record{
		Slings:   []Sling{{Bead: "s1", Outcome: SlingOK, Seconds: 100}},
		Gates:    []Gate{{Bead: "g1", Verdict: GatePass, Seconds: 400}},
		Landings: []Landing{{Bead: "l1", Landed: true}},
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

func TestRehearsalJudgeP90(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		rec  Record
		want string // reason substring; empty means the run passes
	}{
		{name: "sling p90 at the limit passes", rec: Record{Slings: slingsOf(rep(10, 120)...)}},
		{name: "sling p90 over the limit fails", rec: Record{Slings: slingsOf(rep(10, 121)...)}, want: "sling p90 121 s is over 120 s"},
		{name: "one slow sling in ten is outside nearest rank", rec: Record{Slings: slingsOf(append(rep(9, 100), 500)...)}},
		{name: "two slow slings in ten fail the sling p90", rec: Record{Slings: slingsOf(append(rep(8, 100), 500, 500)...)}, want: "sling p90 500 s is over 120 s"},
		{name: "two slow slings in eleven fail the sling p90", rec: Record{Slings: slingsOf(append(rep(9, 100), 500, 500)...)}, want: "sling p90 500 s is over 120 s"},
		{name: "slow slings listed first still count", rec: Record{Slings: slingsOf(append([]float64{500, 500}, rep(8, 100)...)...)}, want: "sling p90 500 s is over 120 s"},
		{name: "presubmit p90 at the limit passes", rec: Record{Gates: gatesOf(GatePass, rep(10, 472)...)}},
		{name: "presubmit p90 over the limit fails", rec: Record{Gates: gatesOf(GatePass, rep(10, 473)...)}, want: "presubmit p90 473 s is over 472 s"},
		{name: "slow passing gates count", rec: Record{Gates: gatesOf(GatePass, append(rep(8, 100), 500, 500)...)}, want: "presubmit p90 500 s is over 472 s"},
		{name: "slow failing gates count", rec: Record{Gates: gatesOf(GateFail, append(rep(8, 100), 500, 500)...)}, want: "presubmit p90 500 s is over 472 s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertVerdict(t, Judge(tt.rec), tt.want)
		})
	}
}
