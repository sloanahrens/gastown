// Package steward runs the daemon's on-demand landing-queue jobs: one
// headless agent session per event, recorded in an append-only job ledger.
// The daemon side of gt-9bioi.
package steward

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcome is how one job ended. The set is closed: the ledger records only
// what a reader can act on, so a job that cannot classify itself records
// OutcomeError with its reason rather than a new word (gt-9bioi.3 reads it).
type Outcome string

const (
	// OutcomePass: the reviewed submission is good; the landing worker's
	// verdict is the authority.
	OutcomePass Outcome = "pass"
	// OutcomeFail: the job's verdict is that the work must not land.
	OutcomeFail Outcome = "fail"
	// OutcomeFixed: the job repaired the branch and requeued it.
	OutcomeFixed Outcome = "fixed"
	// OutcomeRequeued: the branch was already correct after a rebase, or the
	// job requeued it without a content change.
	OutcomeRequeued Outcome = "requeued"
	// OutcomeResling: the job re-slung the bead for rework by its polecat.
	OutcomeResling Outcome = "resling"
	// OutcomeEscalated: the job could not decide and escalated.
	OutcomeEscalated Outcome = "escalated"
	// OutcomeError: the job failed before it could act.
	OutcomeError Outcome = "error"
	// OutcomeTimeout: the job outlived its timeout and was killed.
	OutcomeTimeout Outcome = "timeout"
)

// Outcomes lists every outcome the ledger accepts, in the order the bead
// names them.
var Outcomes = []Outcome{OutcomePass, OutcomeFail, OutcomeFixed, OutcomeRequeued, OutcomeResling, OutcomeEscalated, OutcomeError, OutcomeTimeout}

// Valid reports whether o is an outcome the ledger accepts.
func (o Outcome) Valid() bool {
	for _, want := range Outcomes {
		if o == want {
			return true
		}
	}
	return false
}

// Failed reports whether o is an outcome a retry with a harder model could
// still improve on.
func (o Outcome) Failed() bool {
	return o == OutcomeFail || o == OutcomeError || o == OutcomeTimeout || o == OutcomeEscalated
}

// Job is one steward job's ledger record. Started is written when the job
// starts, the record is rewritten complete when it ends; Ended's zero value
// is "still running" (see Ledger.CloseRunning).
type Job struct {
	ID         string    `json:"id"`
	Event      Kind      `json:"event"`
	Bead       string    `json:"bead"`
	Rig        string    `json:"rig"`
	Branch     string    `json:"branch,omitempty"`
	Head       string    `json:"head,omitempty"`
	Model      string    `json:"model,omitempty"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitempty"`
	Outcome    Outcome   `json:"outcome,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
}

// Key identifies the work a job did: one landing-queue event on one head of
// one bead. A resubmission moves Head, so its key differs (gt-9bioi.1).
func (j Job) Key() string {
	return strings.Join([]string{string(j.Event), j.Bead, j.Head}, "|")
}

// LedgerPath is the job ledger for a town.
func LedgerPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "steward", "jobs.jsonl")
}

// Ledger is one town's append-only job ledger. Every daemon process is its
// only writer; readers are `gt steward status` and the daemon's own scans.
type Ledger struct {
	path string
}

// NewLedger returns the ledger at path.
func NewLedger(path string) *Ledger { return &Ledger{path: path} }

// Path is the ledger file.
func (l *Ledger) Path() string { return l.path }

// Append adds j as one JSON line. The open is O_APPEND, so a status reader
// never sees a torn line from a job ending while it reads (gt-9bioi.3).
func (l *Ledger) Append(j Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("encoding job %s: %w", j.ID, err)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// A process killed mid-write leaves a record with no newline. Appending
	// onto it would splice this record into that wreck and lose both, so the
	// wreck gets its line ending first and Read skips it on its own.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Read returns every record in the ledger, oldest first. A line that does
// not parse is skipped: a process killed mid-write leaves one, and losing a
// job's row beats failing the scan that lists the rest.
func (l *Ledger) Read() ([]Job, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Job
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var j Job
		if err := json.Unmarshal([]byte(line), &j); err != nil {
			continue
		}
		out = append(out, j)
	}
	if err := scanner.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// History returns the records for one event key, oldest first.
func (l *Ledger) History(key string) ([]Job, error) {
	all, err := l.Read()
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, j := range all {
		if j.Key() == key {
			out = append(out, j)
		}
	}
	return out, nil
}

// CloseRunning writes outcome for every job with no end time and returns how
// many it closed. Only the daemon that starts jobs calls it, at startup: jobs
// are children of the daemon process, so a record without an end time is a
// job whose daemon died, never one still running (gt-9bioi.1).
func (l *Ledger) CloseRunning(outcome Outcome, summary string, now time.Time) (int, error) {
	all, err := l.Read()
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, j := range all {
		if !j.Ended.IsZero() {
			continue
		}
		j.Ended, j.Outcome, j.Summary = now, outcome, summary
		j.Transcript = ""
		if err := l.Append(j); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// Active returns the records with no end time, the jobs a previous daemon
// left behind. The running Ledger view is Runner's, not this file's.
func (l *Ledger) Active() ([]Job, error) {
	all, err := l.Read()
	if err != nil {
		return nil, err
	}
	// CloseRunning appends a completed copy; the stale start row stays in the
	// file, so a job is running only if no completed row has its id.
	done := make(map[string]bool, len(all))
	for _, j := range all {
		if !j.Ended.IsZero() {
			done[j.ID] = true
		}
	}
	var out []Job
	for _, j := range all {
		if j.Ended.IsZero() && !done[j.ID] {
			out = append(out, j)
		}
	}
	return out, nil
}
