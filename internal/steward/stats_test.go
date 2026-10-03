package steward

import (
	"path/filepath"
	"testing"
	"time"
)

// ended is a finished job that started at testEpoch+start and ran dur.
func ended(id, model string, outcome Outcome, start, dur time.Duration) Job {
	j := testJob(id, "review", "gt-"+id, id)
	j.Model = model
	j.Started = testEpoch.Add(start)
	j.Ended, j.Outcome = j.Started.Add(dur), outcome
	return j
}

func TestSummarizeCountsTheWindow(t *testing.T) {
	t.Parallel()
	now := testEpoch.Add(2 * time.Hour)
	rows := []Job{
		// A start row, superseded by the end row: one job.
		testJob("a", "review", "gt-a", "a"),
		ended("a", DefaultRoutineAgent, OutcomePass, 0, 10*time.Minute),
		// Ended before the window: not counted.
		ended("old", DefaultRoutineAgent, OutcomeError, 0, time.Minute),
		ended("b", DefaultRoutineAgent, OutcomeError, 70*time.Minute, 2*time.Minute),
		ended("c", DefaultHardAgent, OutcomeTimeout, 75*time.Minute, 45*time.Minute),
		ended("d", DefaultHardAgent, OutcomeEscalated, 100*time.Minute, 4*time.Minute),
		ended("e", DefaultRoutineAgent, OutcomeInterrupted, 105*time.Minute, time.Minute),
	}
	// "a" and "old" ended at or before the window start (now-1h = +1h); only
	// the rows ending after it are in.
	s := Summarize(rows, StatsOptions{Since: now.Add(-time.Hour), Now: now})
	if s.Jobs != 4 || s.Finished != 4 || s.Attempted != 3 {
		t.Fatalf("jobs/finished/attempted = %d/%d/%d, want 4/4/3", s.Jobs, s.Finished, s.Attempted)
	}
	if s.Broke != 2 || s.Escalated != 1 || s.Pro != 2 {
		t.Errorf("broke/escalated/pro = %d/%d/%d, want 2/1/2", s.Broke, s.Escalated, s.Pro)
	}
	if got := s.ErrorRate(); got < 0.66 || got > 0.67 {
		t.Errorf("error rate = %v, want 2/3: the interrupted job is not an attempt", got)
	}
	if s.Outcomes[OutcomeInterrupted] != 1 || s.Outcomes[OutcomeError] != 1 {
		t.Errorf("outcomes = %v", s.Outcomes)
	}
	if s.Models[DefaultHardAgent] != 2 || s.Models[DefaultRoutineAgent] != 2 {
		t.Errorf("models = %v", s.Models)
	}
	// Durations of the attempted jobs: 2m, 4m, 45m.
	if s.MedianSec != 240 || s.MaxSec != 2700 {
		t.Errorf("median/max = %v/%v, want 240/2700", s.MedianSec, s.MaxSec)
	}
	if len(s.Recent) != 4 || s.Recent[0].ID != "e" {
		t.Errorf("recent = %v, want newest first starting at e", s.Recent)
	}
}

func TestSummarizeFindsStuckJobs(t *testing.T) {
	t.Parallel()
	now := testEpoch.Add(3 * time.Hour)
	young := testJob("young", "review", "gt-young", "y")
	young.Started = now.Add(-10 * time.Minute)
	killing := testJob("killing", "review", "gt-k", "k")
	killing.Started = now.Add(-(45*time.Minute + 30*time.Second))
	stuck := testJob("stuck", "review", "gt-s", "s")
	stuck.Started = now.Add(-(45*time.Minute + StuckGrace + time.Second))
	// A running job counts whatever the window says.
	s := Summarize([]Job{stuck, young, killing}, StatsOptions{Since: now.Add(-time.Minute), Now: now, Timeout: 45 * time.Minute})
	if len(s.Running) != 3 {
		t.Fatalf("running = %d, want 3", len(s.Running))
	}
	if len(s.Stuck) != 1 || s.Stuck[0].ID != "stuck" {
		t.Errorf("stuck = %v, want only the job past timeout+grace", s.Stuck)
	}
	if s.Finished != 0 || s.ErrorRate() != 0 {
		t.Errorf("finished %d rate %v, want none", s.Finished, s.ErrorRate())
	}
}

func TestSummarizeLastBoundsRecent(t *testing.T) {
	t.Parallel()
	now := testEpoch.Add(time.Hour)
	var rows []Job
	for i := 0; i < 15; i++ {
		rows = append(rows, ended(string(rune('a'+i)), DefaultRoutineAgent, OutcomePass, time.Duration(i)*time.Minute, time.Minute))
	}
	if got := len(Summarize(rows, StatsOptions{Since: testEpoch, Now: now}).Recent); got != DefaultRecent {
		t.Errorf("default recent = %d, want %d", got, DefaultRecent)
	}
	if got := len(Summarize(rows, StatsOptions{Since: testEpoch, Now: now, Last: 3}).Recent); got != 3 {
		t.Errorf("last 3 = %d", got)
	}
	if s := Summarize(rows, StatsOptions{Since: testEpoch, Now: now, Last: -1}); len(s.Recent) != 0 || s.Recent == nil {
		t.Errorf("last -1 = %v, want an empty non-nil list", s.Recent)
	}
}

func TestAlertsRecordOnceKeyed(t *testing.T) {
	t.Parallel()
	a := NewAlerts(filepath.Join(t.TempDir(), "steward", "alerts.jsonl"))
	if got, err := a.Read(); err != nil || len(got) != 0 {
		t.Fatalf("empty store = %v, %v", got, err)
	}
	for _, al := range []Alert{
		{Key: "pro:s1", Kind: AlertPro, Job: "s1", At: testEpoch},
		{Key: "stuck:s2", Kind: AlertStuck, Job: "s2", At: testEpoch.Add(time.Hour)},
	} {
		if err := a.Record(al); err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.Read()
	if err != nil || len(got) != 2 {
		t.Fatalf("read = %v, %v", got, err)
	}
	if r := Raised(got); !r["pro:s1"] || !r["stuck:s2"] || r["pro:s3"] {
		t.Errorf("raised = %v", r)
	}
	if c := CountSince(got, testEpoch.Add(time.Minute)); c[AlertStuck] != 1 || c[AlertPro] != 0 {
		t.Errorf("count since = %v", c)
	}
}

// TestSummarizeCountsShadowJobs: the shadow count is the jobs whose verdicts
// were comments only; a row from before modes existed was a live job.
func TestSummarizeCountsShadowJobs(t *testing.T) {
	t.Parallel()
	shadow := ended("a", DefaultRoutineAgent, OutcomePass, 0, time.Minute)
	shadow.Mode = ModeShadow
	live := ended("b", DefaultRoutineAgent, OutcomePass, time.Minute, time.Minute)
	live.Mode = ModeLive
	legacy := ended("c", DefaultRoutineAgent, OutcomePass, 2*time.Minute, time.Minute)
	s := Summarize([]Job{shadow, live, legacy}, StatsOptions{Since: testEpoch.Add(-time.Hour), Now: testEpoch.Add(time.Hour)})
	if s.Jobs != 3 || s.Shadow != 1 {
		t.Errorf("jobs/shadow = %d/%d, want 3/1", s.Jobs, s.Shadow)
	}
}

// TestSummarizeLeavesProExemptJobsOutOfPro: a plan job runs on PlanAgent by
// design, so a town whose hard agent is that same preset counts no escalated
// usage for it. The counter agrees with the pro notice the monitor skips for
// the same reason; Models still counts the job by preset (gt-4k3fj.14).
func TestSummarizeLeavesProExemptJobsOutOfPro(t *testing.T) {
	t.Parallel()
	plan := ended("plan", PlanAgent, OutcomePass, 0, time.Minute)
	plan.Event = KindPlan
	review := ended("rev", PlanAgent, OutcomePass, 10*time.Minute, time.Minute)
	s := Summarize([]Job{plan, review}, StatsOptions{Since: testEpoch.Add(-time.Hour), Now: testEpoch.Add(time.Hour), HardAgent: PlanAgent, Last: -1})
	if s.Pro != 1 {
		t.Errorf("Pro = %d, want 1: the review job only", s.Pro)
	}
	if s.Models[PlanAgent] != 2 {
		t.Errorf("Models = %v, want both jobs counted by preset", s.Models)
	}
}
