package steward

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testEpoch = time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)

func testJob(id, event, bead, head string) Job {
	return Job{
		ID: id, Event: Kind(event), Bead: bead, Rig: "gastown", Branch: "polecat/a/" + bead,
		Head: head, Model: DefaultRoutineAgent, Started: testEpoch,
	}
}

// TestLedgerRecordShape pins the JSON key names: gt steward status and the
// daemon's own scans read this file, so a renamed field is a silent zero for
// every reader (gt-9bioi.3).
func TestLedgerRecordShape(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "steward", "jobs.jsonl"))
	job := testJob("steward-1", "review", "gt-x", "c0ffee")
	job.Ended, job.Outcome, job.Summary, job.Transcript = testEpoch.Add(time.Minute), OutcomePass, "reviewed", "/tmp/t.jsonl"
	if err := l.Append(job); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("ledger line does not parse: %v\n%s", err, raw)
	}
	want := map[string]any{
		"id": "steward-1", "event": "review", "bead": "gt-x", "rig": "gastown",
		"branch": "polecat/a/gt-x", "head": "c0ffee", "model": DefaultRoutineAgent,
		"outcome": "pass", "summary": "reviewed", "transcript": "/tmp/t.jsonl",
	}
	for key, wantVal := range want {
		if got[key] != wantVal {
			t.Errorf("ledger[%q] = %v, want %v", key, got[key], wantVal)
		}
	}
	if got["started"] == nil || got["ended"] == nil {
		t.Errorf("ledger lacks a timestamp: %v", got)
	}
	if _, err := time.Parse(time.RFC3339, got["started"].(string)); err != nil {
		t.Errorf("started is not RFC3339: %v", got["started"])
	}
}

// TestLedgerCapsAndDedupeKeys covers the two questions a scan asks: what has
// run for an event key, and what is running now.
func TestLedgerCapsAndDedupeKeys(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "jobs.jsonl"))
	first := testJob("steward-1", "review", "gt-x", "aaaa")
	first.Ended, first.Outcome = testEpoch.Add(time.Minute), OutcomeFail
	other := testJob("steward-2", "review", "gt-x", "bbbb") // a resubmission: new head
	other.Ended, other.Outcome = testEpoch.Add(2*time.Minute), OutcomePass
	capped := testJob("steward-3", "rejection", "gt-x", "bbbb") // same head, other event
	capped.Ended, capped.Outcome = testEpoch.Add(3*time.Minute), OutcomeFixed
	for _, j := range []Job{first, other, capped} {
		if err := l.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	history, err := l.History(first.Key())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ID != "steward-1" {
		t.Fatalf("history for %s = %+v", first.Key(), history)
	}
	if history[0].Key() == other.Key() {
		t.Error("a new head shares the old key: a resubmission would never run")
	}
	if history[0].Key() == capped.Key() {
		t.Error("the two events share a key")
	}
}

// TestLedgerSkipsTornLines: a process killed mid-write leaves a partial
// line, and the rows around it still have to be readable (gt-9bioi.3).
func TestLedgerSkipsTornLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "jobs.jsonl")
	l := NewLedger(path)
	good := testJob("steward-1", "review", "gt-x", "aaaa")
	good.Ended, good.Outcome = testEpoch, OutcomePass
	if err := l.Append(good); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"steward-2","event":"rev`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	second := testJob("steward-3", "rejection", "gt-y", "bbbb")
	second.Ended, second.Outcome = testEpoch, OutcomeFixed
	if err := l.Append(second); err != nil {
		t.Fatal(err)
	}
	jobs, err := l.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != "steward-1" || jobs[1].ID != "steward-3" {
		t.Fatalf("read %+v, want the two whole rows", jobs)
	}
}

// TestLedgerCloseRunning: a job's row with no end time is one whose daemon
// died, and nothing may leave it looking live forever.
func TestLedgerCloseRunning(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "jobs.jsonl"))
	running := testJob("steward-1", "review", "gt-x", "aaaa")
	done := testJob("steward-2", "review", "gt-y", "bbbb")
	done.Ended, done.Outcome = testEpoch, OutcomePass
	for _, j := range []Job{running, done} {
		if err := l.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	active, err := l.Active()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != "steward-1" {
		t.Fatalf("active = %+v, want only the running job", active)
	}
	n, err := l.CloseRunning(OutcomeError, "the daemon restarted", testEpoch.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CloseRunning = %d, %v; want 1, nil", n, err)
	}
	if active, err = l.Active(); err != nil || len(active) != 0 {
		t.Fatalf("active after closing = %+v, %v", active, err)
	}
	// The completed copy is what a reader sees; the original start row stays.
	jobs, err := l.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("ledger has %d rows, want 3 (two starts, one completion)", len(jobs))
	}
	last := jobs[2]
	if last.ID != "steward-1" || last.Outcome != OutcomeError || last.Ended.IsZero() {
		t.Errorf("closed row = %+v", last)
	}
}

func TestOutcomeValid(t *testing.T) {
	t.Parallel()
	for _, o := range Outcomes {
		if !o.Valid() {
			t.Errorf("%q not valid", o)
		}
	}
	for _, o := range []Outcome{"", "done", "complete", "PASS"} {
		if o.Valid() {
			t.Errorf("%q valid, want not", o)
		}
	}
	if !OutcomeFail.Failed() || !OutcomeTimeout.Failed() || OutcomePass.Failed() || OutcomeFixed.Failed() {
		t.Error("Failed() disagrees with the retry rule")
	}
}
