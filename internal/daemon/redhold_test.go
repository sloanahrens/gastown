package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
)

func TestRedHoldEvalSweepTriggers(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	held := now.Add(-time.Hour)
	tests := []struct {
		name   string
		sweeps []redHoldSweep
		prev   []dispatch.RedHoldReason
		want   []dispatch.RedHoldReason
	}{
		{"no sweeps hold nothing", nil, nil, nil},
		{"one red cycle is below the threshold", []redHoldSweep{{Rig: "gastown", RedCycles: 1}}, nil, nil},
		{"two red cycles hold as sweep-red", []redHoldSweep{{Rig: "gastown", RedCycles: 2}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: now, Detail: "gastown: 2 red cycles"}}},
		{"no result holds as sweep-no-result", []redHoldSweep{{Rig: "gastown", RedCycles: 3, NoResult: true}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepNoResult, Since: now, Detail: "gastown: 3 red cycles"}}},
		{"rigs of one kind share one reason in input order", []redHoldSweep{{Rig: "gastown", RedCycles: 3}, {Rig: "other", RedCycles: 2}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: now, Detail: "gastown: 3 red cycles, other: 2 red cycles"}}},
		{"kinds come out sorted", []redHoldSweep{{Rig: "gastown", RedCycles: 2}, {Rig: "other", RedCycles: 2, NoResult: true}}, nil,
			[]dispatch.RedHoldReason{
				{Kind: dispatch.RedHoldSweepNoResult, Since: now, Detail: "other: 2 red cycles"},
				{Kind: dispatch.RedHoldSweepRed, Since: now, Detail: "gastown: 2 red cycles"},
			}},
		{"a held kind keeps its Since", []redHoldSweep{{Rig: "gastown", RedCycles: 2}},
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: held}},
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: held, Detail: "gastown: 2 red cycles"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalRedHold(redHoldInputs{Sweeps: tt.sweeps}, now, dispatch.RedHoldRecord{Reasons: tt.prev})
			if !reflect.DeepEqual(got.Reasons, tt.want) {
				t.Errorf("reasons = %+v, want %+v", got.Reasons, tt.want)
			}
		})
	}
}

func TestRedHoldChangeLine(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	rec := func(reasons ...dispatch.RedHoldReason) dispatch.RedHoldRecord {
		return dispatch.RedHoldRecord{Reasons: reasons}
	}
	red := func(since time.Time, detail string) dispatch.RedHoldReason {
		return dispatch.RedHoldReason{Kind: dispatch.RedHoldSweepRed, Since: since, Detail: detail}
	}
	tests := []struct {
		name       string
		prev, next dispatch.RedHoldRecord
		want       string
	}{
		{"both empty log nothing", rec(), rec(), ""},
		{"equal kinds log nothing", rec(red(at, "")), rec(red(at, "")), ""},
		{"no hold to one kind sets it", rec(), rec(red(at, "")), "red hold: set: sweep-red"},
		{"a new kind is listed sorted", rec(red(at, "")), rec(red(at, ""), dispatch.RedHoldReason{Kind: dispatch.RedHoldBehindMain, Since: at}), "red hold: set: behind-main, sweep-red"},
		{"one kind to none clears it", rec(red(at, "")), rec(), "red hold: cleared"},
		{"a changed Since is silent", rec(red(at, "")), rec(red(at.Add(time.Hour), "")), ""},
		{"a changed Detail is silent", rec(red(at, "old")), rec(red(at, "new")), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redHoldChangeLine(tt.prev, tt.next); got != tt.want {
				t.Errorf("redHoldChangeLine(%+v, %+v) = %q, want %q", tt.prev, tt.next, got, tt.want)
			}
		})
	}
}
