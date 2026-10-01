package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/steward"
)

var stewardNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// stewardTown writes a ledger and alert record into a fresh town.
func stewardTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	ledger := steward.NewLedger(steward.LedgerPath(town))
	job := func(id, model string, outcome steward.Outcome, startedAgo, ran time.Duration, summary string) steward.Job {
		j := steward.Job{ID: id, Event: steward.KindReview, Bead: "gt-" + id, Rig: "gastown", Model: model, Started: stewardNow.Add(-startedAgo)}
		if outcome != "" {
			j.Ended, j.Outcome, j.Summary = j.Started.Add(ran), outcome, summary
		}
		return j
	}
	for _, j := range []steward.Job{
		job("a", steward.DefaultRoutineAgent, steward.OutcomePass, 50*time.Minute, 4*time.Minute, "reviewed"),
		job("b", steward.DefaultRoutineAgent, steward.OutcomeError, 40*time.Minute, time.Minute, "no verdict"),
		job("c", steward.DefaultHardAgent, steward.OutcomeEscalated, 30*time.Minute, 9*time.Minute, "needs the overseer"),
		job("d", steward.DefaultRoutineAgent, "", 20*time.Minute, 0, ""),
		job("old", steward.DefaultRoutineAgent, steward.OutcomePass, 5*time.Hour, time.Minute, "outside the window"),
	} {
		if err := ledger.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	alerts := steward.NewAlerts(steward.AlertsPath(town))
	if err := alerts.Record(steward.Alert{Key: "pro:c", Kind: steward.AlertPro, Job: "c", At: stewardNow.Add(-29 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return town
}

func TestStewardStatusText(t *testing.T) {
	t.Parallel()
	town := stewardTown(t)
	v, err := buildStewardStatus(town, stewardNow.Add(-time.Hour), stewardNow, steward.DefaultRecent)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderStewardStatus(&buf, v, time.UTC)
	out := buf.String()
	for _, want := range []string{
		"steward off: 2026-10-01 11:00 to 12:00 (job timeout 45m0s, hard preset deepseek-pro)",
		"mode        shadow",
		"jobs        4 (3 finished, 1 running)",
		"outcomes    error 1, escalated 1, pass 1",
		"broke       1 of 3 attempted ended in error or timeout (33%)",
		"models      deepseek-flash 3, deepseek-pro 1",
		"pro jobs    1 on deepseek-pro",
		"duration    median 4m, max 9m",
		"escalations 1 job(s) ended escalated; the daemon raised pro 1",
		"running     gt-d (review, deepseek-flash, d, running 20m)",
		"stuck       none",
		"11:40:00  review    gt-d",
		"11:30:00  review    gt-c         deepseek-pro    escalated     9m  needs the overseer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "outside the window") {
		t.Errorf("a job that ended before the window is listed:\n%s", out)
	}
}

func TestStewardStatusJSONShape(t *testing.T) {
	t.Parallel()
	v, err := buildStewardStatus(stewardTown(t), stewardNow.Add(-time.Hour), stewardNow, 2)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	// The overseer's hourly report reads these keys.
	for _, key := range []string{"enabled", "job_timeout", "hard_agent", "jobs", "finished", "attempted", "outcomes", "models", "pro", "broke", "escalated",
		"median_seconds", "max_seconds", "running", "stuck", "recent", "alerts"} {
		if _, ok := got[key]; !ok {
			t.Errorf("--json lacks %q: %s", key, raw)
		}
	}
	if recent, _ := got["recent"].([]any); len(recent) != 2 {
		t.Errorf("--last 2 listed %d jobs", len(recent))
	}
	if got["median_seconds"] != float64(240) || got["pro"] != float64(1) {
		t.Errorf("median %v pro %v, want 240 and 1", got["median_seconds"], got["pro"])
	}
}

func TestStewardStatusEmptyTown(t *testing.T) {
	t.Parallel()
	v, err := buildStewardStatus(t.TempDir(), stewardNow.Add(-time.Hour), stewardNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderStewardStatus(&buf, v, time.UTC)
	for _, want := range []string{"jobs        0 (0 finished, 0 running)", "outcomes    none", "running     none", "stuck       none"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("empty output lacks %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "recent jobs") {
		t.Errorf("an empty ledger printed a recent-jobs header:\n%s", buf.String())
	}
}
