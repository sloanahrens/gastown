package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat/spans"
)

// spanEvent is one line of a bead's span log as a test reads it back.
type spanEvent struct {
	Seat   string `json:"seat"`
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

func readSpanLog(t *testing.T, townRoot, bead string) []spanEvent {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(spans.DefaultDir(townRoot), bead+".jsonl"))
	if err != nil {
		t.Fatalf("reading the span log for %s: %v", bead, err)
	}
	var events []spanEvent
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var ev spanEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decoding span line %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

// submittedSpanTown seeds the town and the store a polecat's submitted
// completion runs against: gt-base-123 is gt:ready-to-land, the agent bead is
// gastown/nux's.
func submittedSpanTown(t *testing.T) (string, *beadsfake.Fake) {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", filepath.Join(".beads", "locks"), "gastown"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix":"gt-","path":"gastown"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes.jsonl: %v", err)
	}
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(doneAgentBead(), beads.Issue{
		ID:     "gt-base-123",
		Title:  "the work",
		Type:   "task",
		Status: string(beads.StatusInProgress),
		Labels: []string{land.LabelReadyToLand},
	})
	return townRoot, db
}

func submittedSpanEnv(db beads.Client) doneStateEnv {
	return doneStateEnv{
		getenv: envMap(map[string]string{"GT_ROLE": "polecat", "GT_RIG": "gastown", "GT_POLECAT": "nux"}),
		routed: func(string) beads.Client { return db },
		source: func(string, string) beads.Client { return db },
		purge:  func(string, string) {},
		reviewHead: func(string) (string, error) {
			return "2222222222222222222222222222222222222222", nil
		},
	}
}

// TestCompletedDoneClosesTheSpanItsSlingStarted: the seat's attempt opens when
// the sling's session comes up and closes when gt done submits the branch, so
// the log answers how long the polecat held the bead.
func TestCompletedDoneClosesTheSpanItsSlingStarted(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := spans.New(spans.DefaultDir(townRoot), nil)
	if err := store.Start("gt-base-123", "gastown/nux"); err != nil {
		t.Fatalf("start span: %v", err)
	}

	if _, err := updateAgentStateOnDoneIn(submittedSpanEnv(db), filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", pendingCompletion()); err != nil {
		t.Fatalf("completed gt done: %v", err)
	}

	events := readSpanLog(t, townRoot, "gt-base-123")
	if len(events) != 2 {
		t.Fatalf("span log = %+v, want the start and one end", events)
	}
	if end := events[1]; end.Event != spans.EventEnd || end.Reason != string(spans.ReasonDone) || end.Seat != "gastown/nux" {
		t.Errorf("end event = %+v, want gastown/nux closing with reason %q", end, spans.ReasonDone)
	}
	sum, err := store.Summarize("gt-base-123")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.LowerBound {
		t.Errorf("summary = %+v, want one closed attempt", sum)
	}
}

// TestDoneEndsTheSpanOnEveryExit: the span is what the bead's landing record
// reports, so every exit that ends the seat's turn ends the attempt — not just
// the submission — and the reason says which way the turn ended (gt-8yk78).
func TestDoneEndsTheSpanOnEveryExit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		exit string
		bead beads.Issue
		want spans.Reason
	}{
		{
			name: "deferred keeps the bead and ends the turn",
			exit: done.ExitDeferred,
			bead: beads.Issue{ID: "gt-base-123", Title: "paused work", Type: "task",
				Status: string(beads.StatusHooked), Assignee: "gastown/polecats/nux"},
			want: spans.ReasonDeferred,
		},
		{
			name: "completed on a bead the seat closed itself",
			exit: done.ExitCompleted,
			bead: beads.Issue{ID: "gt-base-123", Title: "nothing to implement", Type: "task",
				Status: string(beads.StatusClosed), Assignee: "gastown/polecats/nux"},
			want: spans.ReasonDone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := escalatedTown(t)
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(doneAgentBead(), tt.bead)
			store := spans.New(spans.DefaultDir(townRoot), nil)
			if err := store.Start("gt-base-123", "gastown/nux"); err != nil {
				t.Fatalf("start span: %v", err)
			}

			if _, err := updateAgentStateOnDoneIn(submittedSpanEnv(db), filepath.Join(townRoot, "gastown"), townRoot, tt.exit, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
				t.Fatalf("gt done %s: %v", tt.exit, err)
			}

			events := readSpanLog(t, townRoot, "gt-base-123")
			if len(events) != 2 {
				t.Fatalf("span log = %+v, want the start and one end", events)
			}
			if end := events[1]; end.Event != spans.EventEnd || end.Reason != string(tt.want) || end.Seat != "gastown/nux" {
				t.Errorf("end event = %+v, want gastown/nux closing with reason %q", end, tt.want)
			}
		})
	}
}

// TestCrewDoneWritesNoPolecatSpan: only a polecat seat opens an attempt, so
// gt done as a crew member ends none (gt-8yk78).
func TestCrewDoneWritesNoPolecatSpan(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	env := submittedSpanEnv(db)
	env.getenv = envMap(map[string]string{"GT_ROLE": "crew", "GT_RIG": "gastown", "GT_CREW": "sloan"})
	store := spans.New(spans.DefaultDir(townRoot), nil)
	if err := store.Start("gt-base-123", "gastown/nux"); err != nil {
		t.Fatalf("start span: %v", err)
	}

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("crew gt done: %v", err)
	}

	if events := readSpanLog(t, townRoot, "gt-base-123"); len(events) != 1 {
		t.Errorf("span log = %+v, want the start alone", events)
	}
}

// TestEscalatedDoneClosesTheSpanItsSlingStarted: an ESCALATED exit ends the
// attempt like any other turn — the seat stopped on a blocker — so a bead that
// is resumed and escalated again reads as two attempts, not one that never
// ended (gt-dz78b).
func TestEscalatedDoneClosesTheSpanItsSlingStarted(t *testing.T) {
	t.Parallel()
	townRoot := escalatedTown(t)
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedEscalatedBead(db)
	store := spans.New(spans.DefaultDir(townRoot), nil)
	if err := store.Start("gt-blocked", "gastown/nux"); err != nil {
		t.Fatalf("start span: %v", err)
	}

	runEscalatedDone(t, db, filepath.Join(townRoot, "gastown"), townRoot)

	events := readSpanLog(t, townRoot, "gt-blocked")
	if len(events) != 2 {
		t.Fatalf("span log = %+v, want the start and one end", events)
	}
	if end := events[1]; end.Event != spans.EventEnd || end.Reason != string(spans.ReasonEscalated) || end.Seat != "gastown/nux" {
		t.Errorf("end event = %+v, want gastown/nux closing with reason %q", end, spans.ReasonEscalated)
	}
	sum, err := store.Summarize("gt-blocked")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || sum.Attempts[0].Open || sum.LowerBound {
		t.Errorf("summary = %+v, want one closed attempt", sum)
	}

	// The bead stays open for the operator, so gt done can run again: the
	// attempt it would find is already ended, and a second end is not written.
	runEscalatedDone(t, db, filepath.Join(townRoot, "gastown"), townRoot)
	if again := readSpanLog(t, townRoot, "gt-blocked"); len(again) != 2 {
		t.Errorf("span log = %+v after a repeated escalated exit, want it unchanged", again)
	}
}

// TestCompletedDoneWritesNoSecondEnd: a repeated gt done on the same submitted
// bead finds the attempt already closed, so it leaves the log alone instead of
// appending an end pair every run.
func TestCompletedDoneWritesNoSecondEnd(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := spans.New(spans.DefaultDir(townRoot), nil)
	if err := store.Start("gt-base-123", "gastown/nux"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	env := submittedSpanEnv(db)
	for run := 0; run < 2; run++ {
		if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
			t.Fatalf("gt done run %d: %v", run+1, err)
		}
	}

	if events := readSpanLog(t, townRoot, "gt-base-123"); len(events) != 2 {
		t.Errorf("span log = %+v, want the start and a single end after two gt done runs", events)
	}
}

// fakeSpanStore is the span log gt done sees: a fixed summary, and ends
// recorded rather than written.
type fakeSpanStore struct {
	sum    spans.Summary
	sumErr error
	endErr error
	ends   []string
}

func (f *fakeSpanStore) Summarize(string) (spans.Summary, error) { return f.sum, f.sumErr }

func (f *fakeSpanStore) End(bead, seat string, reason spans.Reason, at time.Time) error {
	f.ends = append(f.ends, bead+" "+seat+" "+string(reason))
	return f.endErr
}

// TestCompletedDoneSurvivesAFailedSpanEnd: the span is a measurement, not a
// gate — a log that cannot be written is reported and the submission stands.
func TestCompletedDoneSurvivesAFailedSpanEnd(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := &fakeSpanStore{
		sum:    spans.Summary{Attempts: []spans.Attempt{{Seat: "gastown/nux", Open: true}}},
		endErr: errors.New("read-only file system"),
	}
	env := submittedSpanEnv(db)
	env.spanLog = func(string) spanStore { return store }

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("gt done failed on a span write: %v", err)
	}
	if len(store.ends) != 1 {
		t.Fatalf("ends = %v, want the write attempted once", store.ends)
	}
	fields := beads.ParseAgentFields(mustShowBead(t, db, "gt-gastown-polecat-nux").Description)
	if fields.AgentState != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want done after a failed span write", fields.AgentState)
	}
}

// showFailingBeads answers no Show, the shape of a bead read that failed for a
// reason other than absence: gt done refuses the turn instead of guessing.
type showFailingBeads struct{ beads.Client }

func (showFailingBeads) Show(string) (*beads.Issue, error) {
	return nil, errors.New("dolt: connection refused")
}

// TestAFailedDoneLeavesTheSpanOpen: an exit that returns an error does not end
// the seat's turn — the session stays up to retry the same turn — so the
// attempt stays open rather than reading as time the seat did not spend
// (gt-8yk78).
func TestAFailedDoneLeavesTheSpanOpen(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := spans.New(spans.DefaultDir(townRoot), nil)
	if err := store.Start("gt-base-123", "gastown/nux"); err != nil {
		t.Fatalf("start span: %v", err)
	}
	env := submittedSpanEnv(db)
	env.source = func(string, string) beads.Client { return showFailingBeads{Client: db} }

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err == nil {
		t.Fatal("gt done = nil, want the failed bead read to refuse the turn")
	}

	if events := readSpanLog(t, townRoot, "gt-base-123"); len(events) != 1 {
		t.Errorf("span log = %+v, want the start alone after a failed turn", events)
	}
	sum, err := store.Summarize("gt-base-123")
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Attempts) != 1 || !sum.Attempts[0].Open {
		t.Errorf("summary = %+v, want the attempt left open for the retry", sum)
	}
}

// TestCompletedDoneSurvivesAnUnreadableSpanLog: the span is a measurement, not
// a gate — a log that cannot be read is reported and the submission stands,
// with no end written on the strength of a read that failed (gt-8yk78).
func TestCompletedDoneSurvivesAnUnreadableSpanLog(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := &fakeSpanStore{sumErr: errors.New("permission denied")}
	env := submittedSpanEnv(db)
	env.spanLog = func(string) spanStore { return store }

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("gt done failed on an unreadable span log: %v", err)
	}
	if len(store.ends) != 0 {
		t.Errorf("ends = %v, want none written from a log that could not be read", store.ends)
	}
	fields := beads.ParseAgentFields(mustShowBead(t, db, "gt-gastown-polecat-nux").Description)
	if fields.AgentState != string(beads.AgentStateDone) {
		t.Errorf("agent_state = %q, want done after an unreadable span log", fields.AgentState)
	}
}

// TestCompletedDoneWithNoOpenSpanWritesNothing: a seat with no open attempt on
// the bead (a sling that never recorded a start) has nothing to close.
func TestCompletedDoneWithNoOpenSpanWritesNothing(t *testing.T) {
	t.Parallel()
	townRoot, db := submittedSpanTown(t)
	store := &fakeSpanStore{}
	env := submittedSpanEnv(db)
	env.spanLog = func(string) spanStore { return store }

	if _, err := updateAgentStateOnDoneIn(env, filepath.Join(townRoot, "gastown"), townRoot, done.ExitCompleted, "gt-base-123", beads.AgentFieldUpdates{}); err != nil {
		t.Fatalf("gt done failed with no open span: %v", err)
	}
	if len(store.ends) != 0 {
		t.Errorf("ends = %v, want none without an open start", store.ends)
	}
}
