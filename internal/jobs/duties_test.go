package jobs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/reconcile"
)

func TestJobsDutiesRows(t *testing.T) {
	t.Parallel()
	lastGood := testNow.Add(-time.Hour)
	root := t.TempDir()
	obs := []reconcile.Observation{{Duty: "backup", Verdict: reconcile.VerdictOK, LastGood: lastGood},
		{Duty: "archive", Verdict: reconcile.VerdictDue, LastGood: lastGood},
		{Duty: "gc:hm", Verdict: reconcile.VerdictOverdue, LastGood: lastGood}}
	limits := map[string]time.Duration{"backup": 24 * time.Hour, "archive": 7 * 24 * time.Hour, "gc:hm": time.Hour}
	if err := daemon.WriteDutyState(root, testNow, obs, limits); err != nil {
		t.Fatal(err)
	}
	want := []Job{
		{Name: "duty:backup", State: StateOK, Outcome: OutcomeOK, Cadence: 24 * time.Hour},
		{Name: "duty:archive", State: StateOK, Outcome: OutcomeOK, Cadence: 7 * 24 * time.Hour},
		{Name: "duty:gc:hm", State: StateOverdue, Cadence: time.Hour},
	}
	for _, w := range want {
		j := jobsRow(t, root, Options{}, nil, w.Name)
		if j.State != w.State || j.Outcome != w.Outcome || j.Cadence != w.Cadence ||
			j.Kind != config.DaemonStepPatrol || j.LastRun == nil || !j.LastRun.Equal(lastGood) {
			t.Errorf("%s: state %q outcome %q cadence %v kind %q last run %v, want %q %q %v %q %v",
				w.Name, j.State, j.Outcome, j.Cadence, j.Kind, j.LastRun, w.State, w.Outcome, w.Cadence, config.DaemonStepPatrol, lastGood)
		}
	}
}

// TestJobsDutiesUnknownRaises covers a verdict the reconciler could not judge:
// its duty raises unknown with a failed outcome. TestJobsDutiesRows covers the
// judgeable verdicts' rows.
func TestJobsDutiesUnknownRaises(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := daemon.WriteDutyState(root, testNow,
		[]reconcile.Observation{{Duty: "mystery", Verdict: reconcile.VerdictUnknown}},
		map[string]time.Duration{"mystery": time.Hour}); err != nil {
		t.Fatal(err)
	}
	unknown := jobsRow(t, root, Options{}, nil, "duty:mystery")
	if unknown.State != StateUnknown || unknown.Outcome != OutcomeFailed || unknown.Kind != config.DaemonStepPatrol {
		t.Errorf("duty:mystery = state %q outcome %q kind %q, want %q %q %q",
			unknown.State, unknown.Outcome, unknown.Kind, StateUnknown, OutcomeFailed, config.DaemonStepPatrol)
	}
}

// TestJobsDutiesStaleRecord covers the record as a whole: a record the pass
// stopped writing raises every duty, a fresh one keeps its verdicts, an absent
// record is no duties yet, and a record that exists but cannot be decoded is
// the one failed duty:record row.
func TestJobsDutiesStaleRecord(t *testing.T) {
	t.Parallel()
	obs := []reconcile.Observation{{Duty: "backup", Verdict: reconcile.VerdictOK, LastGood: testNow.Add(-time.Hour)}}
	limits := map[string]time.Duration{"backup": 24 * time.Hour}

	t.Run("record 31 minutes old raises every duty", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := daemon.WriteDutyState(root, testNow.Add(-31*time.Minute), obs, limits); err != nil {
			t.Fatal(err)
		}
		j := jobsRow(t, root, Options{}, nil, "duty:backup")
		if j.State != StateUnknown || j.Outcome != OutcomeFailed {
			t.Errorf("duty:backup = state %q outcome %q, want %q %q", j.State, j.Outcome, StateUnknown, OutcomeFailed)
		}
	})

	t.Run("record 29 minutes old keeps its verdict", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := daemon.WriteDutyState(root, testNow.Add(-29*time.Minute), obs, limits); err != nil {
			t.Fatal(err)
		}
		j := jobsRow(t, root, Options{}, nil, "duty:backup")
		if j.State != StateOK || j.Outcome != OutcomeOK {
			t.Errorf("duty:backup = state %q outcome %q, want %q %q", j.State, j.Outcome, StateOK, OutcomeOK)
		}
	})

	t.Run("absent record yields no duty rows", func(t *testing.T) {
		t.Parallel()
		rows, err := List(t.TempDir(), Options{}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, j := range rows {
			if strings.HasPrefix(j.Name, dutyPrefix) {
				t.Errorf("row %q, want no duty rows", j.Name)
			}
		}
	})

	t.Run("undecodable record yields the one failed duty row", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "daemon", "duty_state.json"), "not a record")
		rows, err := List(root, Options{}, testNow)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var duty []Job
		for _, j := range rows {
			if strings.HasPrefix(j.Name, dutyPrefix) {
				duty = append(duty, j)
			}
		}
		if len(duty) != 1 {
			t.Fatalf("duty rows = %d %v, want exactly 1", len(duty), duty)
		}
		j := duty[0]
		if j.Name != "duty:record" || j.Kind != config.DaemonStepPatrol ||
			j.State != StateUnknown || j.Outcome != OutcomeFailed {
			t.Errorf("duty row = name %q kind %q state %q outcome %q, want %q %q %q %q",
				j.Name, j.Kind, j.State, j.Outcome, "duty:record", config.DaemonStepPatrol, StateUnknown, OutcomeFailed)
		}
	})
}
