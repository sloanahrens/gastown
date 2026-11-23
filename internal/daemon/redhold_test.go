package daemon

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
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

// TestRedHoldStepDriftWaitingError pins the keep-previous rule for an unreadable
// count and that a nil counter contributes 0.
func TestRedHoldStepDriftWaitingError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	require.NoError(t, dispatch.WriteRedHold(townRoot, redHoldSentinel))
	const line = "red hold: waiting landings unreadable; keeping the previous state"
	boom := func() (int, error) { return 0, errors.New("counting landings") }
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	unreadLog := map[string]bool{}
	step := func(wait func() (int, error)) {
		src := redHoldSources{Rigs: []string{"gastown"}, Read: func(string) (tierSweepState, error) { return tierSweepState{}, nil }, Waiting: wait}
		require.NoError(t, runRedHold(townRoot, src, unreadLog, logf))
	}

	step(boom)
	step(boom)
	rec, err := dispatch.ReadRedHold(townRoot)
	require.NoError(t, err)
	require.Equal(t, redHoldSentinel, rec, "an unreadable count keeps the file")
	require.Equal(t, []string{line}, logs, "two heartbeats log one outage once")

	step(nil)
	require.NoFileExists(t, dispatch.RedHoldPath(townRoot), "a nil count contributes 0 and the green record clears the hold")

	step(boom)
	require.Equal(t, []string{line, "red hold: cleared", line}, logs, "a second outage logs again")
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
