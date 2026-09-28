package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// These tests open the tail themselves, before writing anything, and run each
// poll synchronously with pollTailForRig. The old tests raced a writer
// goroutine against the wait's own OpenTail behind a fixed sleep: bytes
// written before the open were treated as history, so under load the event
// was never seen and the wait ran to its deadline (gt-u3x6).

func writeEventsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendEvents(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func openTestTail(t *testing.T, path string) *events.Tail {
	t.Helper()
	tail, err := events.OpenTail(path)
	if err != nil {
		t.Fatalf("OpenTail: %v", err)
	}
	t.Cleanup(func() { _ = tail.Close() })
	return tail
}

// pollRig runs one poll and fails the test on a read error.
func pollRig(t *testing.T, tail *events.Tail, rig string) *AwaitSignalResult {
	t.Helper()
	res, err := pollTailForRig(tail, rig)
	if err != nil {
		t.Fatalf("pollTailForRig: %v", err)
	}
	return res
}

func wantNoSignal(t *testing.T, res *AwaitSignalResult) {
	t.Helper()
	if res != nil {
		t.Fatalf("woke on %q, want no signal", res.Signal)
	}
}

func wantSignal(t *testing.T, res *AwaitSignalResult, line string) {
	t.Helper()
	if res == nil {
		t.Fatalf("no signal, want %q", line)
	}
	if res.Reason != "signal" || res.Signal != line {
		t.Fatalf("got reason=%q signal=%q, want signal %q", res.Reason, res.Signal, line)
	}
}

func TestWaitOnTail_ReturnsSignalOnTick(t *testing.T) {
	t.Parallel()
	path := writeEventsFile(t, `{"ts":"old","type":"ignore"}`+"\n")
	tail := openTestTail(t, path)
	line := `{"ts":"new","type":"sling","actor":"test"}`
	appendEvents(t, path, line+"\n")

	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	res, err := waitOnTail(context.Background(), tail, "", ticks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantSignal(t, res, line)
}

func TestWaitOnTail_TimesOutWhenContextDone(t *testing.T) {
	t.Parallel()
	path := writeEventsFile(t, `{"ts":"old"}`+"\n")
	tail := openTestTail(t, path)
	appendEvents(t, path, `{"type":"done","actor":"om/polecats/jasper"}`+"\n")

	// No tick ever arrives, so the relevant line is never read: only the
	// context ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := waitOnTail(ctx, tail, "om", make(chan time.Time))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q", res.Reason)
	}
}

func TestWaitForActivitySignal_PathWiring(t *testing.T) {
	t.Parallel()
	// waitForActivitySignal tails <townRoot>/.events.jsonl: the wait creates
	// that file, and a context that is already done ends it.
	townRoot := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := waitForActivitySignal(ctx, townRoot, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q", res.Reason)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".events.jsonl")); err != nil {
		t.Errorf("events file not opened at <townRoot>/.events.jsonl: %v", err)
	}
}

func TestWaitForEventsFile_MissingFile(t *testing.T) {
	t.Parallel()
	// A missing events file is created and tailed from its (empty) start.
	path := filepath.Join(t.TempDir(), "nonexistent.jsonl")
	tail := openTestTail(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("events file not created: %v", err)
	}
	wantNoSignal(t, pollRig(t, tail, ""))
	line := `{"ts":"new","type":"sling"}`
	appendEvents(t, path, line+"\n")
	wantSignal(t, pollRig(t, tail, ""), line)
}

func TestWaitForEventsFile_Timeout(t *testing.T) {
	t.Parallel()
	// Lines present before the wait began never wake it.
	path := writeEventsFile(t, `{"ts":"2024-01-01","type":"test"}`+"\n")
	tail := openTestTail(t, path)
	wantNoSignal(t, pollRig(t, tail, ""))
	wantNoSignal(t, pollRig(t, tail, ""))
}

func TestWaitForEventsFile_Signal(t *testing.T) {
	t.Parallel()
	path := writeEventsFile(t, `{"ts":"old","type":"ignore"}`+"\n")
	tail := openTestTail(t, path)
	wantNoSignal(t, pollRig(t, tail, ""))
	line := `{"ts":"new","type":"sling","actor":"test"}`
	appendEvents(t, path, line+"\n")
	wantSignal(t, pollRig(t, tail, ""), line)
}

func TestWaitForEventsFile_CrossRigActivityTimesOut(t *testing.T) {
	t.Parallel()
	// The gt-qwfp bug: an idle rig's witness was woken by any town event, so
	// idle backoff never engaged. Cross-rig activity must not wake it.
	path := writeEventsFile(t, `{"ts":"old"}`+"\n")
	tail := openTestTail(t, path)
	appendEvents(t, path,
		`{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}`+"\n"+
			`{"type":"done","actor":"gastown/polecats/garnet","payload":{"bead":"gt-2bj"}}`+"\n"+
			`{"type":"mail","actor":"mayor/","payload":{"to":"gastown/witness"}}`+"\n")
	wantNoSignal(t, pollRig(t, tail, "om"))
	wantNoSignal(t, pollRig(t, tail, "om"))
}

func TestWaitForEventsFile_WakesOnOwnRigAfterSkippingOthers(t *testing.T) {
	t.Parallel()
	// Foreign events arriving first must be skipped, not swallowed, so a later
	// event for this rig, in a later poll, still wakes the waiter.
	path := writeEventsFile(t, `{"ts":"old"}`+"\n")
	tail := openTestTail(t, path)
	appendEvents(t, path, `{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}`+"\n")
	wantNoSignal(t, pollRig(t, tail, "om"))
	line := `{"type":"sling","actor":"mayor","payload":{"target":"om/polecats/jasper"}}`
	appendEvents(t, path, line+"\n")
	wantSignal(t, pollRig(t, tail, "om"), line)
}

func TestWaitForEventsFile_WakesOnEventSplitAcrossWrites(t *testing.T) {
	t.Parallel()
	// A poller can catch a line mid-write. The prefix must be held until its
	// newline arrives, or the event is judged unparseable and skipped.
	path := writeEventsFile(t, `{"ts":"old"}`+"\n")
	tail := openTestTail(t, path)
	appendEvents(t, path, `{"type":"done","actor":"om/pole`)
	for i := 0; i < 3; i++ {
		wantNoSignal(t, pollRig(t, tail, "om"))
	}
	appendEvents(t, path, `cats/jasper","payload":{}}`+"\n")
	wantSignal(t, pollRig(t, tail, "om"), `{"type":"done","actor":"om/polecats/jasper","payload":{}}`)
}

func TestWaitForEventsFile_DrainsBacklogOfOtherRigs(t *testing.T) {
	t.Parallel()
	// Skipped lines must all be consumed each poll. If only one were drained
	// per poll, a town producing events faster than the poll rate would starve
	// the wait and a later signal for this rig would never be seen.
	const backlog = 500
	path := writeEventsFile(t, `{"ts":"old"}`+"\n")
	tail := openTestTail(t, path)
	line := `{"type":"done","actor":"om/polecats/jasper","payload":{}}`
	appendEvents(t, path,
		strings.Repeat(`{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}`+"\n", backlog)+line+"\n")
	wantSignal(t, pollRig(t, tail, "om"), line) // one poll drains the whole backlog
}

// TestWaitForEventsFile_WakesAfterRenameRotation reproduces the 19:42 incident
// (claude-9jq): the KRC pruner renamed a rewritten events file over the path
// mid-wait and the waiter, still reading the old inode, slept to its timeout.
// A line written to the new file must wake it, and the history the pruner
// retained must not.
func TestWaitForEventsFile_WakesAfterRenameRotation(t *testing.T) {
	t.Parallel()
	path := writeEventsFile(t, `{"ts":"old","type":"patrol_started","actor":"expired"}`+"\n"+
		`{"ts":"kept","type":"mail","actor":"kept"}`+"\n")
	tail := openTestTail(t, path)
	wantNoSignal(t, pollRig(t, tail, ""))

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"ts":"kept","type":"mail","actor":"kept"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	// A poll that sees the rotation with nothing new: a replay of retained
	// history would signal here.
	wantNoSignal(t, pollRig(t, tail, ""))

	fresh := `{"ts":"new","type":"sling","actor":"after-rotation"}`
	appendEvents(t, path, fresh+"\n")
	wantSignal(t, pollRig(t, tail, ""), fresh)
}

// TestWaitForEventsFile_WakesAfterTruncateInPlace covers the other rotation
// shape: the file is truncated in place, so the old offset is past its end.
func TestWaitForEventsFile_WakesAfterTruncateInPlace(t *testing.T) {
	t.Parallel()
	path := writeEventsFile(t, strings.Repeat(`{"ts":"old","type":"patrol_started","actor":"old"}`+"\n", 5))
	tail := openTestTail(t, path)
	wantNoSignal(t, pollRig(t, tail, ""))
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	fresh := `{"ts":"new","type":"nudge","actor":"after-truncate"}`
	appendEvents(t, path, fresh+"\n")
	wantSignal(t, pollRig(t, tail, ""), fresh)
}
