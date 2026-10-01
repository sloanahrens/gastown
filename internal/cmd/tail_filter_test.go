package cmd

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTailVisible_HidesRoutineKeepsTheRest(t *testing.T) {
	t.Parallel()
	ev := func(text string) tailLine { return tailLine{Rig: "gastown", Kind: tailKindEvents, Text: text} }
	dm := func(text string) tailLine { return tailLine{Rig: "town", Kind: tailKindDaemon, Text: text} }
	cases := []struct {
		name string
		line tailLine
		show bool
	}{
		{"wisp create", ev("create gt-wisp-c3w9 status=open actor=daemon seq=1"), false},
		{"hq wisp close", ev("close hq-wisp-e0g8y7 status=closed seq=2"), false},
		{"bead create", ev("create gt-pf2ml status=open actor=sloan seq=3"), true},
		{"bead close", ev("close gt-pf2ml status=closed seq=4"), true},
		{"journal failure", ev("read failed: bd events tail: exit status 25"), true},
		{"wisp close detected", dm("Convoy: close detected: gt-wisp-liqn (from gastown)"), false},
		{"bead close detected", dm("Convoy: close detected: gt-7oc5s (from gastown)"), true},
		{"heartbeat start", dm("Heartbeat starting (recovery-focused)"), false},
		{"heartbeat done", dm("Heartbeat complete (#53)"), false},
		{"handler skip", dm("Handler: skipping plugin seat-refill (gate=manual, requires explicit trigger)"), false},
		{"handler run", dm("Handler: running script plugin seat-refill directly (timeout 5m0s)"), false},
		{"handler nothing to do", dm("Handler: script plugin seat-refill skipped (exit 3 after 2s); nothing to do this run"), false},
		{"seat-refill dispatched", dm("Handler: script plugin seat-refill ok (exit 0 after 41s)"), true},
		{"seat-refill failed", dm("Handler: script plugin seat-refill FAILED (exit 1 after 3s); escalating"), true},
		{"handler record failure", dm("Handler: failed to record script run for plugin seat-refill: signal: killed"), true},
		{"jsonl backup not due", dm("jsonl_git_backup: not due — last run 2m1s ago, interval 15m0s"), false},
		{"jsonl backup failed", dm("jsonl_git_backup: push failed: exit status 1"), true},
		{"checkpoint not due", dm("checkpoint_dog: not due — last run 1m ago, interval 5m"), false},
		{"checkpoint cycle", dm("dog_cycle: checkpoint_dog outcome=ran steps=[scan=done]"), false},
		{"patrol scan summary", dm("patrol_scan: gastown: checked 3 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 stranded reported, 0 error(s)"), false},
		{"patrol scan error", dm("patrol_scan: gastown: scan failed: dolt unreachable"), true},
		{"upgrade restart", dm("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary"), true},
		{"red main", dm("townhealth: RED tick 4ms ago: escalation=oldest_2h needs-human=1"), true},
		{"rejection marker", dm("Convoy hq-cv-zd1: gt-1go.1 carries a rejection marker, deferring to deacon, skipping"), true},
		{"landing", tailLine{Rig: "gastown", Kind: tailKindLandings, Text: "landed gt-1 polecat/opal/gt-1 -> main"}, true},
	}
	for _, c := range cases {
		if got := tailVisible(c.line); got != c.show {
			t.Errorf("%s: visible=%v, want %v (%q)", c.name, got, c.show, c.line.Text)
		}
	}
}

func TestNewTailView_FlagsSelectFilterAndClock(t *testing.T) {
	t.Parallel()
	wisp := tailLine{Kind: tailKindEvents, Text: "create gt-wisp-x status=open"}
	at := time.Date(2026, 10, 1, 16, 34, 5, 0, tailTestLoc)

	def := newTailView(tailTestLoc, false, false)
	if def.Show == nil || def.Show(wisp) {
		t.Error("the default view must hide wisp churn")
	}
	if got := renderTailLine(tailLine{At: at, Rig: "r", Kind: "k", Text: "t"}, def.Loc, def.Layout); got != "16:34:05 r k t" {
		t.Errorf("default time column: %q", got)
	}
	if v := newTailView(tailTestLoc, true, false); v.Show != nil {
		t.Error("--all must show every line")
	}
	iso := newTailView(tailTestLoc, false, true)
	if got := renderTailLine(tailLine{At: at, Rig: "r", Kind: "k", Text: "t"}, iso.Loc, iso.Layout); got != "2026-10-01T16:34:05-05:00 r k t" {
		t.Errorf("--iso time column: %q", got)
	}
}

// syncBuffer is a writer the test can read while the stream still writes.
type syncBuffer struct {
	mu    sync.Mutex
	buf   strings.Builder
	wrote chan struct{} // signaled on every write; see waitFor
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	ch := b.wrote
	b.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return n, err
}

// waitFor blocks until the buffer holds want, waking on each write rather
// than polling. A write that never comes hangs until the test binary's
// timeout, which is the bound.
func (b *syncBuffer) waitFor(want string) {
	b.mu.Lock()
	if b.wrote == nil {
		b.wrote = make(chan struct{}, 1)
	}
	ch := b.wrote
	b.mu.Unlock()
	for !strings.Contains(b.String(), want) {
		<-ch
	}
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockedTailSource answers its first poll only when release closes.
type blockedTailSource struct {
	release chan struct{}
	lines   []tailLine
	polled  bool
}

func (s *blockedTailSource) Poll() []tailLine {
	if s.polled {
		return nil
	}
	s.polled = true
	<-s.release
	return s.lines
}

// TestRunTailStream_FollowPrintsFastSourcesBeforeASlowJournal: gt tail -f took
// ~10s to print its first line because it waited for every store's bd read.
// The daemon's backlog prints while a journal is still being read, and the
// journal's lines follow when it answers.
func TestRunTailStream_FollowPrintsFastSourcesBeforeASlowJournal(t *testing.T) {
	t.Parallel()
	slow := &blockedTailSource{release: make(chan struct{}), lines: []tailLine{
		{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: tailKindEvents, Text: "close gt-1 status=closed"}}}
	fast := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T14:01:00Z"), Rig: "town", Kind: tailKindDaemon, Text: "Convoy: close detected: gt-1 (from gastown)"}}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- runTailStream(ctx, &out, []tailSource{slow, fast}, nil, true, make(chan time.Time), tailView{Loc: tailTestLoc})
	}()

	// The daemon line must print while the journal is still unread.
	out.waitFor("close detected")
	if strings.Contains(out.String(), "status=closed") {
		t.Fatal("the journal line printed before its read finished")
	}
	close(slow.release)
	out.waitFor("status=closed")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunTailStream_ViewHidesFilteredLines(t *testing.T) {
	t.Parallel()
	src := &scriptedTailSource{batches: [][]tailLine{{
		{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: tailKindEvents, Text: "create gt-wisp-a status=open"},
		{At: at("2026-09-30T14:00:01Z"), Rig: "gastown", Kind: tailKindEvents, Text: "create gt-a status=open"},
	}}}
	var out syncBuffer
	view := newTailView(tailTestLoc, false, false)
	if err := runTailStream(context.Background(), &out, []tailSource{src}, nil, false, nil, view); err != nil {
		t.Fatal(err)
	}
	if want := "09:00:01 gastown events create gt-a status=open\n"; out.String() != want {
		t.Fatalf("output %q, want %q", out.String(), want)
	}
}
