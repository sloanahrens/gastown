package steward

import (
	"encoding/json"
	"errors"
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
	n, err := l.CloseRunning(func(int) error { return nil }, testEpoch.Add(time.Hour))
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
	if last.ID != "steward-1" || last.Outcome != OutcomeInterrupted || last.Ended.IsZero() {
		t.Errorf("closed row = %+v", last)
	}
}

// TestLedgerCloseRunningKillsTheGroupFirst: a job runs in a process group of
// its own, so one whose daemon died may still be running. Closing its row
// without killing the group lets the retry run beside it, and both can push
// (gt-9bioi.5).
func TestLedgerCloseRunningKillsTheGroupFirst(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "jobs.jsonl"))
	job := testJob("steward-1", "review", "gt-x", "aaaa")
	if err := l.Append(job); err != nil {
		t.Fatal(err)
	}
	job.Pgid = 4242 // the second row, written when the process started
	if err := l.Append(job); err != nil {
		t.Fatal(err)
	}
	var killed []int
	kill := func(pgid int) error {
		// The row is still open while the group dies: a crash between the
		// two leaves a row the next daemon kills again, never the reverse.
		if active, _ := l.Active(); len(active) != 1 {
			t.Errorf("the row closed before its group was killed: %+v", active)
		}
		killed = append(killed, pgid)
		return nil
	}
	n, err := l.CloseRunning(kill, testEpoch.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("CloseRunning = %d, %v; want 1, nil (one job, two rows)", n, err)
	}
	if len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("killed groups = %v, want [4242]", killed)
	}
	jobs, _ := l.Read()
	last := jobs[len(jobs)-1]
	if last.Outcome != OutcomeInterrupted || last.Pgid != 4242 || last.Ended.IsZero() {
		t.Errorf("closed row = %+v", last)
	}
	if active, _ := l.Active(); len(active) != 0 {
		t.Errorf("still active after closing: %+v", active)
	}
}

// TestLedgerCloseRunningLeavesAGroupItCannotKill: a job whose group survives
// the kill may still be pushing, so its row stays open and its bead stays
// busy.
func TestLedgerCloseRunningLeavesAGroupItCannotKill(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "jobs.jsonl"))
	stuck := testJob("steward-1", "review", "gt-x", "aaaa")
	stuck.Pgid = 4242
	// A row from before the field, or from before the process started: there
	// is no group to kill, and it still has to close.
	old := testJob("steward-2", "review", "gt-y", "bbbb")
	for _, j := range []Job{stuck, old} {
		if err := l.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	kill := func(int) error { return errors.New("operation not permitted") }
	n, err := l.CloseRunning(kill, testEpoch.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "steward-1") {
		t.Fatalf("err = %v, want one naming the job it could not clear", err)
	}
	if n != 1 {
		t.Errorf("closed %d, want only the row with no group", n)
	}
	active, _ := l.Active()
	if len(active) != 1 || active[0].ID != "steward-1" {
		t.Fatalf("active = %+v, want the stuck job to stay open", active)
	}
	// Once the group is gone a later pass closes it.
	n, err = l.CloseRunning(func(int) error { return nil }, testEpoch.Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("second pass = %d, %v; want 1, nil", n, err)
	}
}

// TestLedgerCloseOrphansSparesOwnJobs: a running daemon reaps the jobs a
// predecessor left, never its own.
func TestLedgerCloseOrphansSparesOwnJobs(t *testing.T) {
	t.Parallel()
	l := NewLedger(filepath.Join(t.TempDir(), "jobs.jsonl"))
	mine, theirs := testJob("steward-1", "review", "gt-x", "aaaa"), testJob("steward-2", "review", "gt-y", "bbbb")
	mine.Pgid, theirs.Pgid = 11, 22
	for _, j := range []Job{mine, theirs} {
		if err := l.Append(j); err != nil {
			t.Fatal(err)
		}
	}
	var killed []int
	n, err := l.CloseOrphans(func(pgid int) error { killed = append(killed, pgid); return nil }, map[string]bool{"steward-1": true}, testEpoch)
	if err != nil || n != 1 || len(killed) != 1 || killed[0] != 22 {
		t.Fatalf("CloseOrphans = %d, %v, killed %v; want 1, nil, [22]", n, err, killed)
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
