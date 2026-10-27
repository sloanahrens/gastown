package attention

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// scoreBase is the raise time the fixture item shares.
var scoreBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// The score joins the life both surfaces draw. The queue matches an item on the
// key and first_seen in state.json, and the dashboard matches a row on the raise
// event's key and ts in events.jsonl; either join silently shows a dash if the
// daemon's raise, the pass's log line and the reader's timestamp shape disagree
// about that instant. This runs the production writers and readers end to end,
// so a disagreement fails here rather than in the panel (gt-b9j3o.2).
func TestScoreJoinsTheLifeTheQueueAndThePanelDraw(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	raised := scoreBase
	res := attention.Reconcile(attention.State{}, []attention.Item{{
		Key:      "red-main:gastown",
		Kind:     attention.KindRedMain,
		Severity: attention.SeverityHigh,
		Rig:      "gastown",
		Summary:  "main is red",
	}}, attention.Acks{}, raised)
	if err := attention.WriteState(town, res.State); err != nil {
		t.Fatalf("writing state.json: %v", err)
	}
	if err := attention.AppendEvents(town, res.Events); err != nil {
		t.Fatalf("writing events.jsonl: %v", err)
	}

	pass := &Pass{
		Asker:      &fakeAsker{answer: func(string) (*jev.Answers, error) { return answersFor(0.73, 0.4), nil }},
		State:      func() (attention.State, error) { return attention.ReadState(town) },
		Events:     func() ([]attention.Event, error) { return attention.ReadEvents(town) },
		LogPath:    LogPath(town),
		CursorPath: CursorPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return raised.Add(time.Hour) },
	}
	if err := battery.WriteCursor(pass.CursorPath, battery.Cursor{Time: raised.Add(-time.Minute)}); err != nil {
		t.Fatalf("writing the cursor: %v", err)
	}
	mustRun(t, pass)
	scores, err := attention.NewScoreReader(pass.LogPath).Scores()
	if err != nil {
		t.Fatalf("reading the answers: %v", err)
	}

	// The queue's row: the item's own life as state.json names it.
	state, err := attention.ReadState(town)
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	if len(state.Items) != 1 {
		t.Fatalf("state.json holds %d items, want the one raised", len(state.Items))
	}
	it := state.Items[0]
	if got := scores[attention.LifeKey(it.Key, it.FirstSeen)]; got != 0.73 {
		t.Errorf("the queue's row joins on %s@%s to %v, want 0.73", it.Key, it.FirstSeen, got)
	}

	// The panel's row: the raise as the dashboard's follower reads events.jsonl.
	events, err := attention.ReadRetainedEvents(town)
	if err != nil {
		t.Fatalf("reading events.jsonl: %v", err)
	}
	raisedOnce := false
	for _, ev := range events {
		if ev.State != attention.EventNew {
			continue
		}
		raisedOnce = true
		if got := scores[attention.LifeKey(ev.Key, ev.TS)]; got != 0.73 {
			t.Errorf("the panel's row joins on %s@%s to %v, want 0.73", ev.Key, ev.TS, got)
		}
	}
	if !raisedOnce {
		t.Fatalf("events.jsonl holds no new transition: %v", events)
	}
}
