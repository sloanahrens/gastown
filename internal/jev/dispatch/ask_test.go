package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
)

// askPass is a pass that can only ask: no log, cursor or retry paths, which
// Ask must not need.
func askPass(allows func() (bool, error), as Asker) *Pass {
	return &Pass{Asker: as, Allows: allows}
}

func allowAll() (bool, error) { return true, nil }

// Ask answers for a bead that is not a placement, and names no agent: the
// seat is not chosen yet, so neither the bead's agent: label nor its seat var
// belongs in the state.
func TestJevDispatchAskSendsNoAgent(t *testing.T) {
	t.Parallel()
	b := placed("gt-a", 10, specBody, "jev", "agent:claude-opus")
	b.Description = strings.Replace(b.Description, "dispatched_by:",
		`attached_vars: ["agent=claude-haiku"]`+"\ndispatched_by:", 1)
	f := &fakeAsker{}

	if _, err := askPass(allowAll, f).Ask(context.Background(), b); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(f.states) != 1 {
		t.Fatalf("asked %d times, want once", len(f.states))
	}

	var agent string
	found := false
	for _, line := range strings.Split(strings.TrimSuffix(f.states[0], "\n"), "\n") {
		value, ok := strings.CutPrefix(line, "agent: ")
		if !ok {
			continue
		}
		if err := json.Unmarshal([]byte(value), &agent); err != nil {
			t.Fatalf("agent value %q is not a JSON string: %v", value, err)
		}
		found = true
	}
	if !found {
		t.Fatalf("state has no agent line:\n%s", f.states[0])
	}
	if agent != "" {
		t.Errorf("agent = %q, want none before a seat is chosen", agent)
	}
}

// A pass over the same bead logs the answers Ask returned, so a caller can
// show a prediction before dispatch that the log will confirm.
func TestJevDispatchAskReturnsTheAnswersThePassLogs(t *testing.T) {
	t.Parallel()
	b := placed("gt-a", 10, specBody)
	h := newHarness(t, []*beads.Issue{b})

	got, err := h.pass.Ask(context.Background(), b)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	mustRun(t, h.pass)

	lines, err := ReadRecent(h.log, 1)
	if err != nil || len(lines) != 1 {
		t.Fatalf("log = %v, %v; want the pass's one line", lines, err)
	}
	if !reflect.DeepEqual(lines[0].Answers, got) {
		t.Errorf("Ask answers = %+v, log answers = %+v, want the same", got, lines[0].Answers)
	}
}

// Ask writes no log line, creates no cursor and touches no retry queue: the
// seeded log is byte-identical and the other two files stay absent.
func TestJevDispatchAskWritesNoLogCursorOrRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatch.jsonl")
	cursorPath := filepath.Join(dir, "dispatch.cursor.json")
	retryPath := filepath.Join(dir, "dispatch.retries.json")
	seed := []byte(`{"bead":"gt-old"}` + "\n")
	if err := os.WriteFile(logPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Pass{
		Asker:      &fakeAsker{},
		LogPath:    logPath,
		CursorPath: cursorPath,
		RetryPath:  retryPath,
		Allows:     allowAll,
	}

	if _, err := p.Ask(context.Background(), placed("gt-a", 10, specBody)); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seed) {
		t.Errorf("log = %q, want the seeded line unchanged %q", got, seed)
	}
	for name, path := range map[string]string{"cursor": cursorPath, "retry": retryPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s file at %s exists (stat error %v), want absent", name, path, err)
		}
	}
}

func TestJevDispatchAskStopsAtTheDailyCap(t *testing.T) {
	t.Parallel()
	f := &fakeAsker{}
	p := askPass(func() (bool, error) { return false, nil }, f)

	got, err := p.Ask(context.Background(), placed("gt-a", 10, specBody))

	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("Ask error = %v, want ErrCapReached", err)
	}
	if got != (Answers{}) {
		t.Errorf("answers = %+v, want none", got)
	}
	if len(f.states) != 0 {
		t.Errorf("asked %d times with the cap spent, want none", len(f.states))
	}
}

// A pass with no spend check is refused rather than asking uncapped.
func TestJevDispatchAskNeedsASpendCheck(t *testing.T) {
	t.Parallel()
	f := &fakeAsker{}

	_, err := askPass(nil, f).Ask(context.Background(), placed("gt-a", 10, specBody))

	if err == nil || errors.Is(err, ErrCapReached) {
		t.Fatalf("Ask error = %v, want a refusal that is not the cap", err)
	}
	if len(f.states) != 0 {
		t.Errorf("asked %d times with no spend check, want none", len(f.states))
	}
}

func TestJevDispatchAskReturnsAnUnreadableSpend(t *testing.T) {
	t.Parallel()
	f := &fakeAsker{}
	unreadable := errors.New("ledger unreadable")
	p := askPass(func() (bool, error) { return false, unreadable }, f)

	_, err := p.Ask(context.Background(), placed("gt-a", 10, specBody))

	if !errors.Is(err, unreadable) {
		t.Fatalf("Ask error = %v, want the ledger's cause", err)
	}
	if len(f.states) != 0 {
		t.Errorf("asked %d times with the spend unreadable, want none", len(f.states))
	}
}

// A 429 waits through the pass's Wait for the server's delay cut to
// maxRetryWait, retries once, and returns the retry's answers.
func TestJevDispatchAskRetriesOneRateLimit(t *testing.T) {
	t.Parallel()
	var waits []time.Duration
	f := &fakeAsker{script: []fakeResult{
		{err: &jev.RateLimitError{RetryAfter: time.Hour}},
		{ans: answers()},
	}}
	p := askPass(allowAll, f)
	p.Wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	got, err := p.Ask(context.Background(), placed("gt-a", 10, specBody))

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(f.states) != 2 {
		t.Fatalf("asked %d times, want the rate limit and one retry", len(f.states))
	}
	if !reflect.DeepEqual(waits, []time.Duration{maxRetryWait}) {
		t.Errorf("waits = %v, want the Retry-After cut to %v", waits, maxRetryWait)
	}
	if !reflect.DeepEqual(got, lineAnswers(answers())) {
		t.Errorf("answers = %+v, want the retry's answers %+v", got, lineAnswers(answers()))
	}
}

// A ctx cancelled before the wait ends the ask with the ctx error, and the
// retry is not sent.
func TestJevDispatchAskStopsWhenTheWaitIsCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeAsker{script: []fakeResult{
		{err: &jev.RateLimitError{RetryAfter: time.Hour}},
		{ans: answers()},
	}}
	p := askPass(allowAll, f) // no injected Wait: the pass's own timer

	_, err := p.Ask(ctx, placed("gt-a", 10, specBody))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask error = %v, want the ctx error", err)
	}
	if len(f.states) != 1 {
		t.Errorf("asked %d times, want only the rate-limited call", len(f.states))
	}
}

func TestJevDispatchAskReturnsTheAskerErrorUnchanged(t *testing.T) {
	t.Parallel()
	want := errors.New("jev unreachable")
	f := &fakeAsker{script: []fakeResult{{err: want}}}

	_, err := askPass(allowAll, f).Ask(context.Background(), placed("gt-a", 10, specBody))

	if err != want {
		t.Errorf("Ask error = %v, want the asker's error unchanged", err)
	}
	if len(f.states) != 1 {
		t.Errorf("asked %d times, want once", len(f.states))
	}
}
