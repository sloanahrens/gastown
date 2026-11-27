package jobs

import (
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

func TestJobsDutiesNoRow(t *testing.T) {
	t.Parallel()
	roots := map[string]string{"unknown verdict": t.TempDir(), "no record": t.TempDir()}
	if err := daemon.WriteDutyState(roots["unknown verdict"], testNow,
		[]reconcile.Observation{{Duty: "mystery", Verdict: reconcile.VerdictUnknown}},
		map[string]time.Duration{"mystery": time.Hour}); err != nil {
		t.Fatal(err)
	}
	for name, root := range roots {
		rows, err := List(root, Options{}, testNow)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, j := range rows {
			if strings.HasPrefix(j.Name, dutyPrefix) {
				t.Errorf("%s: row %q, want none", name, j.Name)
			}
		}
	}
}
