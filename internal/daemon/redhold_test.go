package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/stretchr/testify/require"
)

// TestHeartbeatRedHoldRunsTheStep pins the step's registration and the hold it
// publishes from the sweep record.
func TestHeartbeatRedHoldRunsTheStep(t *testing.T) {
	t.Parallel()
	var found []heartbeatStep
	for _, s := range heartbeatSteps {
		if s.name == "red-hold" {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "the heartbeat lists red-hold once")
	require.Equal(t, reflect.ValueOf((*Daemon).checkRedHold).Pointer(), reflect.ValueOf(found[0].run).Pointer(),
		"the red-hold step runs checkRedHold")

	townRoot := t.TempDir()
	writeKnownRigs(t, townRoot, "gastown")
	require.NoError(t, writeTierSweepState(townRoot, "gastown", tierSweepState{RedCycles: redHoldSweepCycles}))
	logs := &lockedBuffer{}
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(logs, "", 0)}
	found[0].run(d)
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Len(t, rec.Reasons, 1)
	require.Equal(t, dispatch.RedHoldSweepRed, rec.Reasons[0].Kind)

	// An unreadable record logs once across two runs: the unread log outlives
	// one heartbeat.
	require.NoError(t, os.WriteFile(tierSweepStatePath(townRoot, "gastown"), []byte("{not json"), 0o600))
	found[0].run(d)
	found[0].run(d)
	require.Equal(t, 1, strings.Count(logs.String(), "sweep record unreadable for gastown"), logs.String())
}

func TestRedHoldSweepOf(t *testing.T) {
	t.Parallel()
	one := func(ts tierSweepTierResult) tierSweepState {
		return tierSweepState{RedCycles: 3, Tiers: map[string]tierSweepTierResult{"race": ts}}
	}
	tests := []struct {
		name   string
		st     tierSweepState
		err    error
		want   redHoldSweep
		wantOK bool
	}{
		{"a no-result cause reports no result", one(tierSweepTierResult{Verdict: tierSweepRed, Cause: tierSweepRunnerNoResult}), nil, redHoldSweep{Rig: "gastown", RedCycles: 3, NoResult: true}, true},
		{"the sweep's no-result failing name reports no result", one(tierSweepTierResult{Verdict: tierSweepRed, Failed: 1, FailedNames: []string{tierSweepRunnerNoResult}}), nil, redHoldSweep{Rig: "gastown", RedCycles: 3, NoResult: true}, true},
		{"a red tier of another cause is sweep-red", one(tierSweepTierResult{Verdict: tierSweepRed, Cause: "step failed"}), nil, redHoldSweep{Rig: "gastown", RedCycles: 3}, true},
		{"a green tier's no-result cause reports no result as false", one(tierSweepTierResult{Verdict: tierSweepGreen, Cause: tierSweepRunnerNoResult}), nil, redHoldSweep{Rig: "gastown", RedCycles: 3}, true},
		{"an unreadable record is unknown whatever it holds", one(tierSweepTierResult{Verdict: tierSweepRed, Cause: tierSweepRunnerNoResult}), errors.New("reading the record"), redHoldSweep{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := redHoldSweepOf("gastown", tt.st, tt.err)
			if ok != tt.wantOK || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("redHoldSweepOf = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRedHoldInputsOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rigs        []string
		states      map[string]tierSweepState
		unread      string
		wantSweeps  []redHoldSweep
		wantUnknown []string
	}{
		{"rigs convert in the order they are read", []string{"alpha", "beta"},
			map[string]tierSweepState{"alpha": {RedCycles: 1}, "beta": {RedCycles: 3}}, "",
			[]redHoldSweep{{Rig: "alpha", RedCycles: 1}, {Rig: "beta", RedCycles: 3}}, nil},
		{"an unreadable rig is left out and named, the others convert", []string{"alpha", "beta", "gamma"},
			map[string]tierSweepState{"alpha": {RedCycles: 3}, "gamma": {RedCycles: 1}}, "beta",
			[]redHoldSweep{{Rig: "alpha", RedCycles: 3}, {Rig: "gamma", RedCycles: 1}}, []string{"beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := redHoldSources{Rigs: tt.rigs, Read: func(rig string) (tierSweepState, error) {
				if rig == tt.unread {
					return tierSweepState{}, errors.New("reading the record")
				}
				return tt.states[rig], nil
			}}
			in, unknown := redHoldInputsOf(src)
			if !reflect.DeepEqual(in.Sweeps, tt.wantSweeps) {
				t.Errorf("sweeps = %+v, want %+v", in.Sweeps, tt.wantSweeps)
			}
			if !reflect.DeepEqual(unknown, tt.wantUnknown) {
				t.Errorf("unknown = %v, want %v", unknown, tt.wantUnknown)
			}
		})
	}
}

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

func TestRedHoldEvalMainRed(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	held := now.Add(-time.Hour)
	tests := []struct {
		name string
		in   redHoldInputs
		prev []dispatch.RedHoldReason
		want []dispatch.RedHoldReason
	}{
		{"a red main holds as main-red", redHoldInputs{MainRed: []string{"gastown"}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: now, Detail: "main red: gastown"}}},
		{"an empty list holds nothing", redHoldInputs{MainRed: []string{}}, nil, nil},
		{"several red rigs name every rig in input order", redHoldInputs{MainRed: []string{"gastown", "other"}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: now, Detail: "main red: gastown, other"}}},
		{"a held main-red keeps its Since and sorts with the other kinds",
			redHoldInputs{MainRed: []string{"gastown"}, Sweeps: []redHoldSweep{{Rig: "other", RedCycles: 2}}},
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: held}},
			[]dispatch.RedHoldReason{
				{Kind: dispatch.RedHoldMainRed, Since: held, Detail: "main red: gastown"},
				{Kind: dispatch.RedHoldSweepRed, Since: now, Detail: "other: 2 red cycles"},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalRedHold(tt.in, now, dispatch.RedHoldRecord{Reasons: tt.prev})
			if !reflect.DeepEqual(got.Reasons, tt.want) {
				t.Errorf("reasons = %+v, want %+v", got.Reasons, tt.want)
			}
		})
	}
}

func TestRedHoldEvalDriftTriggers(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	behind := dispatch.RedHoldReason{Kind: dispatch.RedHoldBehindMain, Since: now, Detail: "100 landings behind main"}
	sweepSince, greenSince := now.Add(-time.Hour), now.Add(-2*time.Hour)
	tests := []struct {
		name string
		in   redHoldInputs
		prev []dispatch.RedHoldReason
		want []dispatch.RedHoldReason
	}{
		{"the threshold number behind main holds", redHoldInputs{Waiting: 100}, nil, []dispatch.RedHoldReason{behind}},
		{"one landing under the threshold holds nothing", redHoldInputs{Waiting: 99}, nil, nil},
		{"a green sweep older than the window holds", redHoldInputs{OldestGreenAt: now.Add(-3*time.Hour - time.Minute)}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldGreenStale, Since: now, Detail: "no green sweep for 3h"}}},
		{"a green sweep inside the window holds nothing", redHoldInputs{OldestGreenAt: now.Add(-2*time.Hour - 59*time.Minute)}, nil, nil},
		{"an unknown green time holds nothing", redHoldInputs{}, nil, nil},
		{"both drift reasons sort with the sweep reasons and keep their Since",
			redHoldInputs{Sweeps: []redHoldSweep{{Rig: "gastown", RedCycles: 2}}, Waiting: 100, OldestGreenAt: now.Add(-4 * time.Hour)},
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: sweepSince}, {Kind: dispatch.RedHoldGreenStale, Since: greenSince}},
			[]dispatch.RedHoldReason{
				behind,
				{Kind: dispatch.RedHoldGreenStale, Since: greenSince, Detail: "no green sweep for 4h"},
				{Kind: dispatch.RedHoldSweepRed, Since: sweepSince, Detail: "gastown: 2 red cycles"},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalRedHold(tt.in, now, dispatch.RedHoldRecord{Reasons: tt.prev})
			if !reflect.DeepEqual(got.Reasons, tt.want) {
				t.Errorf("reasons = %+v, want %+v", got.Reasons, tt.want)
			}
		})
	}
}

func TestRedHoldEvalRollTriggers(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	held := now.Add(-time.Hour)
	target := "0123456789abcdef0123456789abcdef"
	roll := func(outcome string) []dispatch.RedHoldReason {
		return []dispatch.RedHoldReason{{Kind: dispatch.RedHoldRollFailed, Since: now, Detail: "update " + outcome + " at 0123456789ab"}}
	}
	tests := []struct {
		name string
		in   redHoldInputs
		prev []dispatch.RedHoldReason
		want []dispatch.RedHoldReason
	}{
		{"an update that failed holds", redHoldInputs{UpdateOutcome: "failed", UpdateTarget: target}, nil, roll("failed")},
		{"an update that came back partial holds", redHoldInputs{UpdateOutcome: "partial", UpdateTarget: target}, nil, roll("partial")},
		{"an aborted update holds", redHoldInputs{UpdateOutcome: "aborted", UpdateTarget: target}, nil, roll("aborted")},
		{"an interrupted update holds", redHoldInputs{UpdateOutcome: "interrupted", UpdateTarget: target}, nil, roll("interrupted")},
		{"an update still running holds nothing", redHoldInputs{UpdateOutcome: "running", UpdateTarget: target}, nil, nil},
		{"an update that finished ok holds nothing", redHoldInputs{UpdateOutcome: "ok", UpdateTarget: target}, nil, nil},
		{"no update outcome holds nothing", redHoldInputs{}, nil, nil},
		{"images at the retry cap hold", redHoldInputs{ImagesRetries: 3}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldImagesFailed, Since: now, Detail: "images build failed 3 times"}}},
		{"images one retry under the cap hold nothing", redHoldInputs{ImagesRetries: 2}, nil, nil},
		{"a stuck pair names each sha cut to 12 characters", redHoldInputs{PairStuck: []string{"abcdef1234567890deadbeef", "abc"}}, nil,
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldPairStuck, Since: now, Detail: "stuck pair: abcdef123456, abc"}}},
		{"the roll reasons sort together and a held one keeps its Since",
			redHoldInputs{UpdateOutcome: "failed", UpdateTarget: target, ImagesRetries: 4, PairStuck: []string{target}},
			[]dispatch.RedHoldReason{{Kind: dispatch.RedHoldRollFailed, Since: held}},
			[]dispatch.RedHoldReason{
				{Kind: dispatch.RedHoldImagesFailed, Since: now, Detail: "images build failed 4 times"},
				{Kind: dispatch.RedHoldPairStuck, Since: now, Detail: "stuck pair: 0123456789ab"},
				{Kind: dispatch.RedHoldRollFailed, Since: held, Detail: "update failed at 0123456789ab"},
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalRedHold(tt.in, now, dispatch.RedHoldRecord{Reasons: tt.prev})
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

// redHoldSentinel is a record already on disk: a run that must not write keeps it.
var redHoldSentinel = dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Detail: "sentinel"}}}

func TestRedHoldStepSetsThenClears(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	now := time.Unix(1_700_000_000, 0).UTC()
	state := tierSweepState{RedCycles: 2}
	src := redHoldSources{Rigs: []string{"gastown"}, Read: func(string) (tierSweepState, error) { return state, nil }, Now: func() time.Time { return now }}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, logf))
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, []dispatch.RedHoldReason{{Kind: dispatch.RedHoldSweepRed, Since: now, Detail: "gastown: 2 red cycles"}}, rec.Reasons)

	state = tierSweepState{}
	require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, logf))
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "a green record removes the file")
	require.Equal(t, []string{"red hold: set: sweep-red", "red hold: cleared"}, logs, "each change logs once")
}

// TestRedHoldStepDriftFeedsInputs pins that the step hands the evaluator the
// waiting count and the oldest green sweep across the known rigs.
func TestRedHoldStepDriftFeedsInputs(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	behind := []dispatch.RedHoldReason{{Kind: dispatch.RedHoldBehindMain, Since: now, Detail: "100 landings behind main"}}
	stale := []dispatch.RedHoldReason{{Kind: dispatch.RedHoldGreenStale, Since: now, Detail: "no green sweep for 4h"}}
	old := tierSweepState{LastGreenAt: now.Add(-4 * time.Hour)}
	count := func(n int) func() (int, error) { return func() (int, error) { return n, nil } }
	tests := []struct {
		name   string
		rigs   []string
		states map[string]tierSweepState
		wait   func() (int, error)
		want   []dispatch.RedHoldReason
	}{
		{"waiting at the threshold holds behind-main", []string{"gastown"}, map[string]tierSweepState{"gastown": {}}, count(100), behind},
		{"the oldest green sweep 4 hours old holds green-stale", []string{"alpha", "beta"}, map[string]tierSweepState{"alpha": {LastGreenAt: now.Add(-time.Hour)}, "beta": old}, nil, stale},
		{"a known rig with no green sweep leaves the time unknown", []string{"alpha", "beta"}, map[string]tierSweepState{"alpha": old, "beta": {}}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			src := redHoldSources{Rigs: tt.rigs, Read: func(rig string) (tierSweepState, error) { return tt.states[rig], nil }, Waiting: tt.wait, Now: func() time.Time { return now }}
			require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, func(string, ...any) {}))
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, tt.want, rec.Reasons)
		})
	}
}

// TestRedHoldStepRollFeedsInputs pins that the step hands the evaluator the
// update outcome, the largest images retry count across the known rigs and the
// stuck pairs, and that a nil source contributes nothing.
func TestRedHoldStepRollFeedsInputs(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	green := "0123456789abcdef"
	tests := []struct {
		name   string
		states map[string]tierSweepState
		update func() (string, string, error)
		pairs  func() []string
		want   []dispatch.RedHoldReason
	}{
		// The retry counts differ: the largest across the known rigs is 3.
		{"an update failure, retries at the cap and a stuck pair hold all three",
			map[string]tierSweepState{
				"alpha": {LastGreenSHA: green, ImagesRetries: 1, ImagesRetrySHA: green},
				"beta":  {LastGreenSHA: green, ImagesRetries: 3, ImagesRetrySHA: green},
			},
			func() (string, string, error) { return "failed", green, nil }, func() []string { return []string{green} },
			[]dispatch.RedHoldReason{
				{Kind: dispatch.RedHoldImagesFailed, Since: now, Detail: "images build failed 3 times"},
				{Kind: dispatch.RedHoldPairStuck, Since: now, Detail: "stuck pair: 0123456789ab"},
				{Kind: dispatch.RedHoldRollFailed, Since: now, Detail: "update failed at 0123456789ab"},
			}},
		{"retries recorded for another tip read as zero",
			map[string]tierSweepState{"alpha": {LastGreenSHA: green, ImagesRetries: 3, ImagesRetrySHA: "fedcba9876543210"}},
			nil, nil, nil},
		{"a nil update and pair contribute nothing", map[string]tierSweepState{"alpha": {}}, nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			src := redHoldSources{Rigs: []string{"alpha", "beta"}, Read: func(rig string) (tierSweepState, error) { return tt.states[rig], nil }, Update: tt.update, PairStuck: tt.pairs, Now: func() time.Time { return now }}
			require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, func(string, ...any) {}))
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, tt.want, rec.Reasons)
		})
	}
}

// TestRedHoldStepRollUpdateError pins the keep-previous rule for an unreadable
// update state: the file is untouched, the outage logs once across heartbeats,
// and a later outage logs again.
func TestRedHoldStepRollUpdateError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	const line = "red hold: update state unreadable; keeping the previous state"
	var update func() (string, string, error)
	src := redHoldSources{Rigs: []string{"gastown"}, Read: func(string) (tierSweepState, error) { return tierSweepState{}, nil }, Update: func() (string, string, error) { return update() }}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	unreadLog := map[string]bool{}
	step := func() { require.NoError(t, runRedHold(townRoot, src, unreadLog, logf)) }
	boom := func() (string, string, error) { return "", "", errors.New("reading the update state") }

	update = boom
	step()
	step()
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, redHoldSentinel, rec, "an unreadable update state keeps the file")
	require.Equal(t, []string{line}, logs, "two heartbeats log one outage once")

	update = func() (string, string, error) { return "ok", "", nil }
	step()
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "the recovery evaluates again")

	update = boom
	step()
	require.Equal(t, []string{line, "red hold: cleared", line}, logs, "a second outage logs again")
}

// TestRedHoldStepDriftWaitingError pins the keep-previous rule for an unreadable
// count: only behind-main survives, every other trigger still reads, and a nil
// counter contributes 0.
func TestRedHoldStepDriftWaitingError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const line = "red hold: waiting landings unreadable; keeping the previous state"
	behind := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{{Kind: dispatch.RedHoldBehindMain, Detail: "5 landings behind main"}}}
	require.NoError(t, dispatch.WriteRedHold(townRoot, behind))
	boom := func() (int, error) { return 0, errors.New("counting landings") }
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	unreadLog := map[string]bool{}
	step := func(cycles int, wait func() (int, error)) {
		read := func(string) (tierSweepState, error) { return tierSweepState{RedCycles: cycles}, nil }
		require.NoError(t, runRedHold(townRoot, redHoldSources{Rigs: []string{"gastown"}, Read: read, Waiting: wait}, unreadLog, logf))
	}

	step(redHoldSweepCycles, boom)
	step(redHoldSweepCycles, boom)
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, []string{dispatch.RedHoldBehindMain, dispatch.RedHoldSweepRed}, redHoldKinds(rec), "the sweep reason publishes while an unread count keeps behind-main")
	require.Equal(t, []string{line, "red hold: set: behind-main, sweep-red"}, logs, "two heartbeats log one outage once")

	step(0, nil)
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "a nil count contributes 0 and the green record clears the hold")

	step(0, boom)
	require.Equal(t, []string{line, "red hold: set: behind-main, sweep-red", "red hold: cleared", line}, logs, "a second outage logs again")
}

// TestRedHoldStepUnchangedIsSilent pins the runs that leave the file and log alone.
func TestRedHoldStepUnchangedIsSilent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		read func(string) (tierSweepState, error)
	}{
		{"an unchanged kind set is not rewritten", func(string) (tierSweepState, error) { return tierSweepState{RedCycles: 3}, nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
			var logs []string
			logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
			require.NoError(t, runRedHold(townRoot, redHoldSources{Rigs: []string{"gastown"}, Read: tt.read}, map[string]bool{}, logf))
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, redHoldSentinel, rec, tt.name)
			require.Empty(t, logs, tt.name)
			require.NoFileExists(t, filepath.Join(townRoot, redHoldLedgerRel), "an unchanged hold records no lift")
		})
	}
}

// TestRedHoldStepUnknownKeepsPreviousState pins one outage logging once without
// touching the file, a recovery evaluating again, and a second outage logging again.
func TestRedHoldStepUnknownKeepsPreviousState(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	const line = "red hold: sweep record unreadable for gastown; keeping the previous state"
	var read func(string) (tierSweepState, error)
	setRead := func(err error) { read = func(string) (tierSweepState, error) { return tierSweepState{}, err } }
	src := redHoldSources{Rigs: []string{"gastown"}, Read: func(rig string) (tierSweepState, error) { return read(rig) }}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	unreadLog := map[string]bool{}
	step := func() { require.NoError(t, runRedHold(townRoot, src, unreadLog, logf)) }

	setRead(errors.New("reading the record"))
	step()
	step()
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, redHoldSentinel, rec, "an unreadable record keeps the file")
	require.Equal(t, []string{line}, logs, "two heartbeats log one outage once")

	setRead(nil)
	step()
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "the recovery evaluates again")
	require.Equal(t, []string{line, "red hold: cleared"}, logs)

	setRead(errors.New("reading the record"))
	step()
	require.Equal(t, []string{line, "red hold: cleared", line}, logs, "a second outage logs again")
}

// TestRedHoldStepMainRed pins what the main-red read feeds the evaluator: a red
// rig is named, a nil reader contributes nothing, and an unreadable read keeps
// the file, logging once across heartbeats.
func TestRedHoldStepMainRed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	now := time.Unix(1_700_000_000, 0).UTC()
	read := func(string) (tierSweepState, error) { return tierSweepState{}, nil }
	quiet := func(string, ...any) {}
	src := redHoldSources{Rigs: []string{"alpha", "beta"}, Read: read, MainState: func(rig string) (landworker.MainState, error) {
		if rig == "beta" {
			return landworker.MainState{LastGreen: "a", LastRun: "b"}, nil
		}
		return landworker.MainState{}, nil
	}, Now: func() time.Time { return now }}
	require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, quiet))
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, []dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: now, Detail: "main red: beta"}}, rec.Reasons, "a red main for one of two rigs names that rig")

	green := t.TempDir()
	src.MainState = nil
	require.NoError(t, runRedHold(green, src, map[string]bool{}, quiet))
	require.NoFileExists(t, dispatch.RedHoldPath(green), "a nil reader contributes nothing")
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	unreadLog := map[string]bool{}
	boom := func(string) (landworker.MainState, error) {
		return landworker.MainState{}, errors.New("reading main state")
	}
	src = redHoldSources{Rigs: []string{"gastown"}, Read: read, MainState: boom}
	require.NoError(t, runRedHold(townRoot, src, unreadLog, logf))
	require.NoError(t, runRedHold(townRoot, src, unreadLog, logf))
	rec, err = dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, redHoldSentinel, rec, "an unreadable read keeps the file")
	line := "red hold: main-red read unreadable; keeping the previous state"
	require.Equal(t, []string{line}, logs, "two heartbeats log one outage once")
	src.MainState = func(string) (landworker.MainState, error) { return landworker.MainState{}, nil }
	require.NoError(t, runRedHold(townRoot, src, unreadLog, logf))
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "the recovery evaluates again")
	src.MainState = boom
	require.NoError(t, runRedHold(townRoot, src, unreadLog, logf))
	require.Equal(t, []string{line, "red hold: cleared", line}, logs, "a second outage logs again")
}

// TestCheckRedHoldWaiting pins the waiting count to the update patrol's read:
// the checkout's landings against the installed build's commit, not RepoCommit.
func TestCheckRedHoldWaiting(t *testing.T) {
	t.Parallel()
	fx := newUpdateWaitingFixture(t)
	repo := createUpdateSource(t, fx.d.config.TownRoot)
	fx.git.InitRepo(t, repo)
	build := fx.git.Commit(t, repo, "main", "installed build", map[string]string{"a.txt": "a\n"})
	fx.appendLandings(t, fx.git.Commit(t, repo, "main", "landed after the build", map[string]string{"b.txt": "b\n"}))
	stale := &version.StaleBinaryInfo{BinaryCommit: build, RepoCommit: unknownCommit}
	fx.d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return stale }
	got, err := fx.d.redHoldWaiting()
	require.NoError(t, err)
	require.Equal(t, 1, got, "the landing after the build is waiting")
}

// TestCheckRedHoldWaitingStep pins the step's wiring: a readable count clears
// behind-main and logs no alarm, and each unreadable read keeps it, logging once.
func TestCheckRedHoldWaitingStep(t *testing.T) {
	t.Parallel()
	const alarm = "red hold: waiting landings unreadable; keeping the previous state\n"
	behind := dispatch.RedHoldRecord{Reasons: []dispatch.RedHoldReason{{Kind: dispatch.RedHoldBehindMain, Detail: "5 landings behind main"}}}
	for name, tc := range map[string]struct {
		info   *version.StaleBinaryInfo
		source bool
		alarm  bool
	}{
		"a readable count":           {&version.StaleBinaryInfo{BinaryCommit: unknownCommit}, true, false},
		"an unreadable staleness":    {nil, true, true},
		"an unknown installed build": {&version.StaleBinaryInfo{}, true, true},
		"no gt source checkout":      {&version.StaleBinaryInfo{}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			townRoot := t.TempDir()
			logs := &lockedBuffer{}
			d := &Daemon{config: DefaultConfig(townRoot), logger: log.New(logs, "", 0)}
			if tc.source {
				createUpdateSource(t, townRoot)
			}
			d.rebuildGTStaleFn = func(string) *version.StaleBinaryInfo { return tc.info }
			require.NoError(t, dispatch.WriteRedHold(townRoot, behind))
			for range 2 {
				d.checkRedHold()
			}
			if !tc.alarm {
				require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "a readable count clears behind-main")
				require.Equal(t, "red hold: cleared\n", logs.String(), "a readable count logs no alarm")
				return
			}
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, behind, rec, "an unreadable count keeps the previous behind-main reason")
			require.Equal(t, alarm, logs.String(), "two heartbeats log the alarm once")
		})
	}
}

// TestCheckRedHoldRollUpdate pins the step's update source to the run's state
// file: no state is not an error and clears the previous record, a failed
// outcome with its target holds roll-failed, and an unreadable state is unknown.
func TestCheckRedHoldRollUpdate(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	townRoot := t.TempDir()
	store := update.Store{Dir: update.Dir(townRoot)}
	d := &Daemon{config: DefaultConfig(townRoot), logger: log.New(io.Discard, "", 0), clock: clockwork.NewFakeClockAt(now)}
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	d.checkRedHold()
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "a town with no update state holds nothing")
	require.NoError(t, store.Write(update.State{Outcome: update.OutcomeFailed, Target: update.Target{SHA: "0123456789abcdef0123456789abcdef01234567"}}))
	d.checkRedHold()
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, []dispatch.RedHoldReason{{Kind: dispatch.RedHoldRollFailed, Since: now, Detail: "update failed at 0123456789ab"}}, rec.Reasons)
	require.NoError(t, os.WriteFile(store.Path(), []byte("{not json"), 0o600))
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	d.checkRedHold()
	rec, err = dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, redHoldSentinel, rec, "an unreadable state is unknown, not clean")
}

// TestCheckRedHoldRollPairStuck pins the step's stuck-pair source: the pair
// check's stuck commits hold, its ready pair does not.
func TestCheckRedHoldRollPairStuck(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	townRoot := t.TempDir()
	d := &Daemon{config: DefaultConfig(townRoot), logger: log.New(io.Discard, "", 0), clock: clockwork.NewFakeClockAt(now)}
	d.pairReady.held = map[string][]attention.Item{"alpha": {
		{Key: "deploy-pair-stuck:alpha:0123456789ab", Kind: attention.KindDeployPairStuck, SHA: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "deploy-pair-ready:alpha:fedcba987654", Kind: attention.KindDeployPairReady, SHA: "fedcba9876543210fedcba9876543210fedcba98"},
	}}
	d.checkRedHold()
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, []dispatch.RedHoldReason{{Kind: dispatch.RedHoldPairStuck, Since: now, Detail: "stuck pair: 0123456789ab"}}, rec.Reasons,
		"only the stuck item's commit holds, never the ready pair's")
}

// TestRedHoldMainRed pins the main-red read against the revert beads it names:
// a marker is red only while its bead is open, a closed or missing bead is over,
// and a bead that cannot be read keeps the previous record (gt-gqlbm1).
func TestRedHoldMainRed(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	green, next := "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98"
	red := []dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: now, Detail: "main red: gastown"}}
	revert := func(bead string) landworker.MainState {
		return landworker.MainState{LastGreen: green, LastRun: green, Revert: &landworker.PendingRevert{Culprit: next, Bead: bead}}
	}
	tests := []struct {
		name  string
		state landworker.MainState
		open  func(string, string) (bool, error)
		want  []dispatch.RedHoldReason
	}{
		{"a marker naming an open revert bead holds", revert("gt-rev"),
			func(string, string) (bool, error) { return true, nil }, red},
		{"a marker naming a closed revert bead holds nothing", revert("gt-rev"),
			func(string, string) (bool, error) { return false, nil }, nil},
		{"a marker still being built holds", revert(""),
			func(string, string) (bool, error) { return false, nil }, red},
		{"an unreadable revert bead keeps the previous record", revert("gt-rev"),
			func(string, string) (bool, error) { return false, errors.New("reading the bead") }, redHoldSentinel.Reasons},
		{"a marker with no reader holds", revert("gt-rev"), nil, red},
		{"no marker and a run equal to the last green holds nothing",
			landworker.MainState{LastGreen: green, LastRun: green}, nil, nil},
		{"a last run past the last green holds",
			landworker.MainState{LastGreen: green, LastRun: next}, nil, red},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
			src := redHoldSources{
				Rigs:       []string{"gastown"},
				Read:       func(string) (tierSweepState, error) { return tierSweepState{}, nil },
				MainState:  func(string) (landworker.MainState, error) { return tc.state, nil },
				RevertOpen: tc.open,
				Now:        func() time.Time { return now },
			}
			require.NoError(t, runRedHold(townRoot, src, map[string]bool{}, func(string, ...any) {}))
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, tc.want, rec.Reasons)
		})
	}
}

// TestCheckRedHoldMainRed pins the step's reader to the rig's landworker state
// file and to the revert bead the record names: only an open bead holds, and an
// unreadable file or bead keeps the previous record.
func TestCheckRedHoldMainRed(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	green, next := "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98"
	red := []dispatch.RedHoldReason{{Kind: dispatch.RedHoldMainRed, Since: now, Detail: "main red: gastown"}}
	revert := func(bead string) *landworker.MainState {
		return &landworker.MainState{LastGreen: green, LastRun: green, Revert: &landworker.PendingRevert{Culprit: next, Bead: bead}}
	}
	tests := map[string]struct {
		state   *landworker.MainState
		corrupt bool
		bead    *beads.Issue
		beadErr error
		held    bool
		want    []dispatch.RedHoldReason
	}{
		"a last run past the last green holds":                {&landworker.MainState{LastGreen: green, LastRun: next}, false, nil, nil, false, red},
		"a last run equal to the last green holds nothing":    {&landworker.MainState{LastGreen: green, LastRun: green}, false, nil, nil, false, nil},
		"a last green with no run holds nothing":              {&landworker.MainState{LastGreen: green}, false, nil, nil, false, nil},
		"a revert bead with no bead yet holds on a green run": {revert(""), false, nil, nil, false, red},
		"an open revert bead holds":                           {revert("gt-rev"), false, &beads.Issue{ID: "gt-rev", Status: "open"}, nil, false, red},
		"a closed revert bead is over":                        {revert("gt-rev"), false, &beads.Issue{ID: "gt-rev", Status: "closed"}, nil, false, nil},
		"a missing revert bead is over":                       {revert("gt-rev"), false, nil, beads.ErrNotFound, false, nil},
		"an unreadable revert bead keeps the previous record": {revert("gt-rev"), false, nil, errors.New("bd: timed out"), true, redHoldSentinel.Reasons},
		"no state file holds nothing":                         {nil, false, nil, nil, false, nil},
		"an unreadable state file keeps the previous record":  {nil, true, nil, nil, true, redHoldSentinel.Reasons},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			writeKnownRigs(t, townRoot, "gastown")
			path := RedMainStatePath(townRoot, "gastown")
			if tc.held {
				require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
			}
			switch {
			case tc.corrupt:
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
			case tc.state != nil:
				require.NoError(t, (fileMainState{path: path}).Save(*tc.state))
			}
			d := &Daemon{config: DefaultConfig(townRoot), logger: log.New(io.Discard, "", 0), clock: clockwork.NewFakeClockAt(now)}
			d.openWorkBeads = func([]string, time.Duration) workBeadReader { return redHoldBeadReader{tc.bead, tc.beadErr} }
			d.checkRedHold()
			rec, err := dispatch.ReadRedHold(townRoot)
			require.NoError(t, err)
			require.Equal(t, tc.want, rec.Reasons)
		})
	}
}

// redHoldBeadReader answers one revert-bead Show: the issue, or the error.
type redHoldBeadReader struct {
	issue *beads.Issue
	err   error
}

func (r redHoldBeadReader) Show(string) (*beads.Issue, error) { return r.issue, r.err }
func (redHoldBeadReader) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, nil
}

// redHoldLedgerRel is the lift ledger the step appends to, under the town root.
const redHoldLedgerRel = ".runtime/red-hold/ledger.jsonl"

// TestRedHoldStepLift pins the lift branch, its ledger record and its refusals.
func TestRedHoldStepLift(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0).UTC()
	hold := func(t *testing.T, root string) { require.NoError(t, dispatch.WriteRedHold(root, redHoldSentinel)) }
	lift := func(t *testing.T, root string) {
		hold(t, root)
		require.NoError(t, dispatch.WriteLift(root, dispatch.LiftRequest{Reason: "main fixed", By: "overseer", At: now}))
	}
	run := func(t *testing.T, setup func(*testing.T, string)) (error, []string, string) {
		t.Helper()
		root := t.TempDir()
		if setup != nil {
			setup(t, root)
		}
		var logs []string
		src := redHoldSources{Rigs: []string{"gastown"}, Read: func(string) (tierSweepState, error) { return tierSweepState{}, nil }, Now: func() time.Time { return now }}
		err := runRedHold(root, src, map[string]bool{}, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
		return err, logs, root
	}

	err, logs, root := run(t, lift)
	require.NoError(t, err)
	require.Equal(t, []string{"red hold: lifted by overseer: main fixed"}, logs)
	require.NoFileExists(t, dispatch.RedHoldPath(root), "the lift removes the hold file")
	require.NoFileExists(t, dispatch.RedHoldLiftPath(root), "the lift clears the request")
	data, rerr := os.ReadFile(filepath.Join(root, redHoldLedgerRel))
	require.NoError(t, rerr)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, map[string]any{"at": now.Format(time.RFC3339), "reason": "main fixed", "by": "overseer", "lifted": true}, got)

	err, _, root = run(t, nil)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(root, redHoldLedgerRel), "no request records no lift")
	err, _, root = run(t, func(t *testing.T, root string) {
		hold(t, root)
		path := dispatch.RedHoldLiftPath(root)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	})
	require.ErrorContains(t, err, "read lift request")
	require.FileExists(t, dispatch.RedHoldPath(root), "an unreadable request keeps the hold")
	require.FileExists(t, dispatch.RedHoldLiftPath(root), "an unreadable request is not cleared")

	err, _, root = run(t, func(t *testing.T, root string) {
		lift(t, root)
		require.NoError(t, os.MkdirAll(filepath.Join(root, redHoldLedgerRel), 0o755))
	})
	require.ErrorContains(t, err, "record red hold lift")
	err, _, root = run(t, func(t *testing.T, root string) {
		lift(t, root)
		require.NoError(t, os.WriteFile(filepath.Join(root, ".runtime"), nil, 0o600))
	})
	require.ErrorContains(t, err, "record red hold lift")
	require.FileExists(t, dispatch.RedHoldLiftPath(root), "a failed record keeps the request")
}

func TestRedHoldStepRefusals(t *testing.T) {
	t.Parallel()
	red := redHoldSources{Rigs: []string{"gastown"}, Read: func(string) (tierSweepState, error) { return tierSweepState{RedCycles: 2}, nil }}
	noLog := func(string, ...any) {}
	t.Run("an unreadable red hold record", func(t *testing.T) {
		townRoot := t.TempDir()
		require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
		require.NoError(t, os.WriteFile(dispatch.RedHoldPath(townRoot), []byte("{not json"), 0o600))
		require.ErrorContains(t, runRedHold(townRoot, red, map[string]bool{}, noLog), "read red hold")
	})
	t.Run("a red hold record that cannot be written", func(t *testing.T) {
		townRoot := t.TempDir()
		require.NoError(t, os.Symlink(filepath.Join(townRoot, "missing"), filepath.Dir(dispatch.RedHoldPath(townRoot))))
		require.ErrorContains(t, runRedHold(townRoot, red, map[string]bool{}, noLog), "write red hold")
	})
}
