package omclass

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// A cursor that cannot be parsed stops the pass before it asks anything.
func TestOMClassRunRefusesAnUnreadableCursor(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{choice: "fail-open"}
	p := newPass(t, asker, fakeReviews{recs: []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), failOpen)}}, t0)
	if err := os.WriteFile(p.CursorPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run read an unparseable cursor without error")
	}
	if len(asker.states) != 0 {
		t.Errorf("Jev was asked %d times past an unreadable cursor, want none", len(asker.states))
	}
}

// A context that ends before the pass takes its next finding stops the pass
// with the context's error, and asks nothing.
func TestOMClassRunStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{choice: "fail-open"}
	p := newPass(t, asker, fakeReviews{recs: []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), failOpen)}}, t0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(asker.states) != 0 {
		t.Errorf("Jev was asked %d times after the context ended, want none", len(asker.states))
	}
}

// A log the pass cannot write stops the pass before the cursor moves, so the
// finding is asked again by the next pass.
func TestOMClassRunRefusesAnUnwritableLog(t *testing.T) {
	t.Parallel()
	p := newPass(t, &fakeAsker{choice: "fail-open"}, fakeReviews{recs: []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), failOpen)}}, t0)
	// The log's directory is a plain file, so the log cannot be created in it.
	blocker := filepath.Join(filepath.Dir(p.CursorPath), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.LogPath = filepath.Join(blocker, "omclass.jsonl")

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run wrote a log under a plain file without error")
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.Time.Equal(t0) {
		t.Errorf("cursor moved to %v past an unwritten log, want %v", cur.Time, t0)
	}
}

// A cursor the pass cannot write stops the pass, both when it has just skipped
// a finding and when it has just asked one.
func TestOMClassRunRefusesAnUnwritableCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		finding land.ReviewFindingText
	}{
		{"after a skipped finding", land.ReviewFindingText{File: "a.go"}},
		{"after an asked finding", failOpen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			recs := []land.ReviewRecord{review("gt-a", 1, t0.Add(time.Minute), c.finding)}
			p := newPass(t, &fakeAsker{choice: "fail-open"}, fakeReviews{recs: recs}, t0)
			// WriteState writes through a temporary file beside the cursor; a
			// directory in its place makes every cursor write fail.
			if err := os.Mkdir(p.CursorPath+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}

			if _, err := p.Run(context.Background()); err == nil {
				t.Error("Run moved an unwritable cursor without error")
			}
		})
	}
}
