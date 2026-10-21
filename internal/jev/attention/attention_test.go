package attention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// t0 is the pass's clock. The cursor starts an hour before it, so an item
// first seen before t0-1h is behind the cursor.
var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// fakeAsker answers each Ask from a function and records the state it was
// given, so a test can read what the pass quoted.
type fakeAsker struct {
	states []string
	answer func(state string) (*jev.Answers, error)
}

func (f *fakeAsker) Ask(_ context.Context, state any, _ map[string]jev.Question) (*jev.Answers, error) {
	s := state.(string)
	f.states = append(f.states, s)
	return f.answer(s)
}

// answersFor is one Ask answer with the given needs_human probability and
// urgency score.
func answersFor(needs, score float64) *jev.Answers {
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul: map[string]float64{
			QNeedsHuman:         needs,
			QClearsOnItsOwn:     0.2,
			QRoutineActionFixes: 0.3,
			QRepeatsOpenItem:    0.4,
		},
		Score: map[string]jev.ScoreAnswer{QUrgency: {Score: score, Confidence: 0.6}},
	}
}

// item is an attention item first seen at minute after t0.
func item(key string, minute int, summary string) attention.Item {
	return attention.Item{
		Key:       key,
		Kind:      attention.KindRedMain,
		Severity:  attention.SeverityHigh,
		Rig:       "gastown",
		Bead:      "gt-1",
		SHA:       "abc123",
		Summary:   summary,
		FirstSeen: t0.Add(time.Duration(minute) * time.Minute),
	}
}

// raiseOf is the "new" transition an item's raise writes to events.jsonl.
func raiseOf(it attention.Item) attention.Event {
	return attention.Event{
		TS:       it.FirstSeen,
		Class:    it.Kind,
		Severity: it.Severity,
		Text:     it.Summary,
		Key:      it.Key,
		State:    attention.EventNew,
		Handled:  attention.HandledInfo,
	}
}

// harness is one pass over fakes, with its files in a temp directory and a
// cursor set so that raises first seen before t0-1h are behind it. The events
// log is a real events.jsonl in the temp directory, read back by the pass.
type harness struct {
	pass   *Pass
	asker  *fakeAsker
	items  []attention.Item
	spent  float64
	town   string
	log    string
	cursor string
	warn   *bytes.Buffer
}

// newHarness starts a pass over the items: each item is open in state.json and
// has a raise in events.jsonl at its first-seen time.
func newHarness(t *testing.T, items []attention.Item, answer func(string) (*jev.Answers, error)) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		asker:  &fakeAsker{answer: answer},
		items:  items,
		town:   dir,
		log:    filepath.Join(dir, "attention.jsonl"),
		cursor: filepath.Join(dir, "attention.cursor.json"),
		warn:   &bytes.Buffer{},
	}
	h.pass = &Pass{
		Asker:      h.asker,
		State:      func() (attention.State, error) { return attention.State{Items: h.items}, nil },
		Events:     func() ([]attention.Event, error) { return attention.ReadEvents(h.town) },
		LogPath:    h.log,
		CursorPath: h.cursor,
		Allows:     func() (bool, error) { return h.spent < 1, nil },
		Now:        func() time.Time { return t0.Add(time.Hour) },
		Warn:       h.warn,
	}
	var raises []attention.Event
	for _, it := range items {
		raises = append(raises, raiseOf(it))
	}
	h.appendEvents(t, raises...)
	if err := battery.WriteCursor(h.cursor, battery.Cursor{Time: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return h
}

// appendEvents adds transitions to the harness's events.jsonl.
func (h *harness) appendEvents(t *testing.T, evs ...attention.Event) {
	t.Helper()
	if err := attention.AppendEvents(h.town, evs); err != nil {
		t.Fatal(err)
	}
}

// lines reads the log back as generic maps, so a test sees every key.
func (h *harness) lines(t *testing.T) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(h.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func keysOf(lines []map[string]any) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l["key"].(string))
	}
	return out
}

func mustRun(t *testing.T, p *Pass) Summary {
	t.Helper()
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum
}

func TestJevAttentionPassLogsOneLinePerNewItem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{
		item("behind", -120, "raised before the cursor"),
		item("red-b", 2, "main is red"),
		item("red-a", 1, "main is red"),
	}, func(string) (*jev.Answers, error) { return answersFor(0.9, 2), nil })

	sum := mustRun(t, h.pass)

	if sum.Asked != 2 || len(h.asker.states) != 2 {
		t.Fatalf("Asked = %d with %d Ask calls, want 2 and 2", sum.Asked, len(h.asker.states))
	}
	if got := keysOf(h.lines(t)); !reflect.DeepEqual(got, []string{"red-a", "red-b"}) {
		t.Errorf("log keys = %v, want the two new items oldest first", got)
	}
	cur, ok, err := battery.ReadCursor(h.cursor)
	if err != nil || !ok {
		t.Fatalf("ReadCursor: %v (ok %v)", err, ok)
	}
	if cur.ID != "red-b" || !cur.Time.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("cursor = %+v, want red-b at t0+2m", cur)
	}
}

// A raise whose item cleared before the pass is still asked. Its line says
// live=false, and its state quotes no rig and no other open items.
func TestJevAttentionAsksAClearedRaiseWithoutLiveFields(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("open-a", 2, "still open"), item("open-b", 3, "also open")},
		func(string) (*jev.Answers, error) { return answersFor(0.9, 2), nil })
	h.appendEvents(t, raiseOf(item("cleared", 1, "cleared before the pass")))

	sum := mustRun(t, h.pass)

	if sum.Asked != 3 {
		t.Fatalf("Asked = %d, want all three raises asked", sum.Asked)
	}
	lines := h.lines(t)
	if got := keysOf(lines); !reflect.DeepEqual(got, []string{"cleared", "open-a", "open-b"}) {
		t.Fatalf("log keys = %v, want every raise oldest first", got)
	}
	if lines[0]["live"] != false || lines[0]["rig"] != "" || lines[0]["answers"] == nil {
		t.Errorf("cleared raise line = %v, want live false, no rig and answers", lines[0])
	}
	if lines[1]["live"] != true || lines[1]["rig"] != "gastown" {
		t.Errorf("open raise line = %v, want live true and the rig", lines[1])
	}
	cleared := h.asker.states[0]
	if !strings.Contains(cleared, "live: false\n") || !strings.Contains(cleared, "other open items: 0\n") || strings.Contains(cleared, `other "open-a"`) {
		t.Errorf("cleared raise was quoted with others:\n%s", cleared)
	}
	open := h.asker.states[1]
	if !strings.Contains(open, "live: true\n") || !strings.Contains(open, "other open items: 1\n") || !strings.Contains(open, `other "open-b"`) {
		t.Errorf("open raise was not quoted with its live context:\n%s", open)
	}
}

// Each raise of a key is asked, and a cleared transition between them is not.
func TestJevAttentionAsksEveryRaiseOfAKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("flaky", 1, "first raise")},
		func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })
	h.appendEvents(t,
		attention.Event{TS: t0.Add(3 * time.Minute), Class: attention.KindRedMain, Severity: attention.SeverityHigh, Text: "cleared", Key: "flaky", State: attention.EventCleared},
		attention.Event{TS: t0.Add(5 * time.Minute), Class: attention.KindRedMain, Severity: attention.SeverityHigh, Text: "second raise", Key: "flaky", State: attention.EventNew},
	)

	sum := mustRun(t, h.pass)

	if sum.Asked != 2 || len(h.asker.states) != 2 {
		t.Fatalf("summary %+v with %d calls, want two raises asked", sum, len(h.asker.states))
	}
	lines := h.lines(t)
	if got := keysOf(lines); !reflect.DeepEqual(got, []string{"flaky", "flaky"}) {
		t.Errorf("log keys = %v, want the key once per raise", got)
	}
	if lines[1]["first_seen"] == lines[0]["first_seen"] {
		t.Errorf("both raises logged the same first_seen %v", lines[0]["first_seen"])
	}
	cur, _, _ := battery.ReadCursor(h.cursor)
	if cur.ID != "flaky" || !cur.Time.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("cursor = %+v, want the second raise's time and key", cur)
	}
	if sum := mustRun(t, h.pass); sum.Asked != 0 {
		t.Errorf("second pass asked %d raises, want none", sum.Asked)
	}
}

// A cursor older than the first line of events.jsonl warns once and the pass
// continues from the start of the file.
func TestJevAttentionWarnsWhenTheCursorIsBehindTheLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil, func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })
	h.appendEvents(t, raiseOf(item("late", 1, "first line after the cursor")))

	sum := mustRun(t, h.pass)

	if sum.Asked != 1 {
		t.Errorf("Asked = %d, want the pass to continue from the file start", sum.Asked)
	}
	if n := strings.Count(h.warn.String(), "\n"); n != 1 || !strings.Contains(h.warn.String(), "older than the first line of events.jsonl") {
		t.Errorf("warnings = %q, want one line naming the gap", h.warn.String())
	}
}

// A first line at or before the cursor is no gap, so nothing is warned.
func TestJevAttentionNoWarningWhenTheLogReachesTheCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("behind", -120, "raised before the cursor"), item("after", 1, "after")},
		func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })

	mustRun(t, h.pass)

	if h.warn.Len() != 0 {
		t.Errorf("warnings = %q, want none", h.warn.String())
	}
}

// The first pass only sets the cursor at its own time, so it asks about
// nothing, even items already standing.
func TestJevAttentionFirstPassAsksNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("standing", 1, "already raised")},
		func(string) (*jev.Answers, error) { return answersFor(0.5, 0), nil })
	if err := os.Remove(h.cursor); err != nil {
		t.Fatal(err)
	}

	sum := mustRun(t, h.pass)

	if !sum.Started || sum.Asked != 0 || len(h.asker.states) != 0 {
		t.Errorf("summary %+v with %d calls, want a started pass that asks nothing", sum, len(h.asker.states))
	}
	if _, ok, _ := battery.ReadCursor(h.cursor); !ok {
		t.Error("the first pass did not save a cursor")
	}
}

// Items first seen at the same time are taken by key, and a second pass asks
// nothing about them again.
func TestJevAttentionCursorBreaksTiesByKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{
		item("key-b", 1, "same moment"),
		item("key-a", 1, "same moment"),
	}, func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })

	mustRun(t, h.pass)
	if got := keysOf(h.lines(t)); !reflect.DeepEqual(got, []string{"key-a", "key-b"}) {
		t.Errorf("first pass keys = %v, want key-a then key-b", got)
	}
	if sum := mustRun(t, h.pass); sum.Asked != 0 {
		t.Errorf("second pass asked %d items, want none", sum.Asked)
	}
}

// A noul of 0.1 and a score of 0 are logged as returned. Nothing is floored or
// rewritten.
func TestJevAttentionKeepsRawAnswers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("red", 1, "main is red")},
		func(string) (*jev.Answers, error) { return answersFor(0.1, 0), nil })

	mustRun(t, h.pass)

	lines := h.lines(t)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	answers := lines[0]["answers"].(map[string]any)
	if got := answers[QNeedsHuman]; got != 0.1 {
		t.Errorf("needs_human = %v, want the raw 0.1", got)
	}
	urgency := answers[QUrgency].(map[string]any)
	if got := urgency["score"]; got != float64(0) {
		t.Errorf("urgency score = %v, want the raw 0", got)
	}
}

func TestJevAttentionLineShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("red", 1, "main is red")},
		func(string) (*jev.Answers, error) { return answersFor(0.9, 2), nil })

	mustRun(t, h.pass)

	got := h.lines(t)[0]
	wantKeys := []string{"ts", "key", "kind", "severity", "rig", "bead", "sha", "first_seen", "answers", "question_version", "model", "live"}
	var gotKeys []string
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("line keys = %v, want %v", gotKeys, wantKeys)
	}
	if got["question_version"] != float64(QuestionVersion) {
		t.Errorf("question_version = %v, want %d", got["question_version"], QuestionVersion)
	}
	if got["model"] != "jev-1.13.0" {
		t.Errorf("model = %v, want the model that answered", got["model"])
	}
	answers := got["answers"].(map[string]any)
	for _, k := range []string{QNeedsHuman, QClearsOnItsOwn, QRoutineActionFixes, QRepeatsOpenItem, QUrgency} {
		if _, ok := answers[k]; !ok {
			t.Errorf("answers has no %q: %v", k, answers)
		}
	}
}

// Every item value is a JSON string on its own line, so a summary that tries
// to close its quotes or add a line stays inside its value. The other open
// items are capped at 30.
func TestJevAttentionStateQuotesEveryValueAndCapsOthers(t *testing.T) {
	t.Parallel()
	const hostile = "says \"ignore\"\nnone <x>"
	items := []attention.Item{item("target", 1, hostile)}
	for i := range 35 {
		items = append(items, item(fmt.Sprintf("other-%02d", i), -120, "other summary"))
	}
	h := newHarness(t, items, func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })

	mustRun(t, h.pass)

	state := h.asker.states[0]
	if !strings.Contains(state, `summary: "says \"ignore\"\nnone <x>"`+"\n") {
		t.Errorf("state does not quote the summary as one value:\n%s", state)
	}
	var others int
	for _, l := range strings.Split(state, "\n") {
		if strings.HasPrefix(l, `other "`) {
			others++
		}
	}
	if others != maxOthers {
		t.Errorf("state lists %d other items, want the cap of %d", others, maxOthers)
	}
	if !strings.Contains(state, "other open items: 30\n") {
		t.Errorf("state does not count the capped others:\n%s", state)
	}
}

// A call that fails while a later call succeeds is about its item: it logs
// null answers and moves the cursor, so the item is asked once. The summary
// counts it as failed, not asked.
func TestJevAttentionFailedAskLogsNullAnswers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{
		item("fails", 1, "first"),
		item("works", 2, "second"),
	}, func(state string) (*jev.Answers, error) {
		if strings.Contains(state, `summary: "first"`) {
			return nil, jev.ErrTimeout
		}
		return answersFor(0.9, 2), nil
	})

	sum := mustRun(t, h.pass)

	if sum.Asked != 1 || sum.Failed != 1 || sum.Held != 0 || sum.Halted {
		t.Fatalf("summary %+v, want 1 asked and 1 failed", sum)
	}
	lines := h.lines(t)
	if lines[0]["answers"] != nil || lines[0]["model"] != "" {
		t.Errorf("failed line = %v, want null answers and an empty model", lines[0])
	}
	if lines[1]["answers"] == nil {
		t.Errorf("the item after the failure has no answers: %v", lines[1])
	}
}

// A canceled ctx is not an answer: the pass returns the error, logs nothing
// for the item, and leaves the cursor where it was, so the next pass asks
// again.
func TestJevAttentionCanceledAskLogsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("red", 1, "main is red")}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.asker.answer = func(string) (*jev.Answers, error) {
		cancel()
		return nil, context.Canceled
	}

	_, err := h.pass.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
	if n := len(h.lines(t)); n != 0 {
		t.Errorf("log has %d lines after a canceled call, want none", n)
	}
	cur, _, _ := battery.ReadCursor(h.cursor)
	if !cur.Time.Equal(t0.Add(-time.Hour)) {
		t.Errorf("cursor moved to %v after a canceled call", cur.Time)
	}
}

func TestJevAttentionCapStopsBeforeAsking(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("red", 1, "main is red")},
		func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })
	h.spent = 1

	sum := mustRun(t, h.pass)

	if !sum.CapReached || sum.Asked != 0 || len(h.asker.states) != 0 {
		t.Errorf("summary %+v with %d calls, want the cap to stop the pass before any call", sum, len(h.asker.states))
	}
}

func TestJevAttentionQuestionsAreTheFiveAsked(t *testing.T) {
	t.Parallel()
	qs := Questions()
	for _, id := range []string{QNeedsHuman, QClearsOnItsOwn, QRoutineActionFixes, QRepeatsOpenItem} {
		if qs[id].Type != "noul" || qs[id].True == "" || qs[id].False == "" {
			t.Errorf("question %q = %+v, want a noul describing both outcomes", id, qs[id])
		}
	}
	urgency := qs[QUrgency]
	if urgency.Type != "score" || len(urgency.Levels) != 3 {
		t.Errorf("urgency = %+v, want a three-level score", urgency)
	}

	// A caller that edits one call's map must not change the next call's.
	delete(qs, QUrgency)
	if _, ok := Questions()[QUrgency]; !ok {
		t.Error("Questions shares its map between calls")
	}
}

func TestJevAttentionRecentReadsTheLastLines(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{
		item("a", 1, "one"), item("b", 2, "two"), item("c", 3, "three"),
	}, func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })
	mustRun(t, h.pass)

	got, err := ReadRecent(h.log, 2)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, l := range got {
		keys = append(keys, l.Key)
	}
	if !reflect.DeepEqual(keys, []string{"b", "c"}) {
		t.Errorf("recent keys = %v, want the last two oldest first", keys)
	}
}

func TestJevAttentionOpenNeedsAPrice(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), Settings{}, nil)
	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Errorf("Open error = %v, want the missing price named", err)
	}
}

// Calls that keep failing are an outage, not a fact about the items: the pass
// stops after maxConsecutiveFailures, logs nothing, leaves the cursor, and
// reports itself halted with nothing asked. Once Jev answers, the next pass
// asks the same items.
func TestJevAttentionOutageHaltsWithoutBurningItems(t *testing.T) {
	t.Parallel()
	var items []attention.Item
	for i := 1; i <= 5; i++ {
		items = append(items, item(fmt.Sprintf("item-%d", i), i, fmt.Sprintf("summary %d", i)))
	}
	down := true
	h := newHarness(t, items, func(string) (*jev.Answers, error) {
		if down {
			return nil, jev.ErrTimeout
		}
		return answersFor(0.5, 1), nil
	})

	sum := mustRun(t, h.pass)

	if sum.Asked != 0 || sum.Failed != 0 || sum.Held != maxConsecutiveFailures || !sum.Halted {
		t.Fatalf("summary %+v, want nothing asked and a halt with %d held", sum, maxConsecutiveFailures)
	}
	if len(h.asker.states) != maxConsecutiveFailures {
		t.Errorf("%d calls after the outage began, want the pass to stop at %d", len(h.asker.states), maxConsecutiveFailures)
	}
	if n := len(h.lines(t)); n != 0 {
		t.Errorf("log has %d lines from an outage, want none", n)
	}
	cur, _, _ := battery.ReadCursor(h.cursor)
	if !cur.Time.Equal(t0.Add(-time.Hour)) {
		t.Errorf("cursor moved to %v during an outage", cur.Time)
	}
	if r := sum.Report(); strings.Contains(r, "5 items") || !strings.Contains(r, "0 items answered") || !strings.Contains(r, "stopped after 3 failed calls") {
		t.Errorf("Report = %q, want an outage that does not read as asked items", r)
	}

	down = false
	sum = mustRun(t, h.pass)

	if sum.Asked != 5 || sum.Failed != 0 {
		t.Errorf("recovery summary %+v, want all 5 items asked", sum)
	}
	if got := keysOf(h.lines(t)); !reflect.DeepEqual(got, []string{"item-1", "item-2", "item-3", "item-4", "item-5"}) {
		t.Errorf("logged keys %v after recovery, want every item once in order", got)
	}
}

// Fewer failures than the limit at the end of a pass are held too: no later
// call showed they were about the items, so the next pass asks them again.
func TestJevAttentionTrailingFailuresAreHeld(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{
		item("ok", 1, "first"),
		item("fails", 2, "second"),
	}, func(state string) (*jev.Answers, error) {
		if strings.Contains(state, `summary: "second"`) {
			return nil, jev.ErrTimeout
		}
		return answersFor(0.5, 1), nil
	})

	sum := mustRun(t, h.pass)

	if sum.Asked != 1 || sum.Failed != 0 || sum.Held != 1 || sum.Halted {
		t.Fatalf("summary %+v, want 1 asked and 1 held", sum)
	}
	if got := keysOf(h.lines(t)); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Errorf("logged keys %v, want only the answered item", got)
	}
	cur, _, _ := battery.ReadCursor(h.cursor)
	if cur.ID != "ok" {
		t.Errorf("cursor at %q, want it on the last answered item", cur.ID)
	}
}

func TestJevAttentionReportNamesFailuresApartFromAnswers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sum  Summary
		want []string
	}{
		{"started", Summary{Started: true}, []string{"first pass"}},
		{"answered", Summary{Asked: 2}, []string{"2 items answered"}},
		{"failed and held", Summary{Asked: 1, Failed: 2, Held: 1}, []string{"1 items answered", "2 logged with null answers", "1 failed calls left"}},
		{"capped", Summary{Asked: 1, CapReached: true}, []string{"daily cap was reached"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.sum.Report()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("Report = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// A pass finding another pass's lock stops with ErrBusy before it reads the
// queue or calls Jev, and leaves the cursor and log alone.
func TestJevAttentionBusyPassAsksNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []attention.Item{item("red", 1, "main is red")},
		func(string) (*jev.Answers, error) { return answersFor(0.5, 1), nil })
	release, err := battery.Lock(h.cursor + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = h.pass.Run(context.Background())

	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Run error = %v, want ErrBusy", err)
	}
	if len(h.asker.states) != 0 || len(h.lines(t)) != 0 {
		t.Errorf("a busy pass made %d calls and %d log lines", len(h.asker.states), len(h.lines(t)))
	}
}

// Open wires the production pass: the files sit under the town's runtime
// directory, the cap defaults to a dollar, and the spend it reads is the
// town's usage ledger, so another caller's calls count against the pass.
func TestJevAttentionOpenWiresTheTownFiles(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	pass, err := open(town, Settings{USDPerBillionTokens: 1000}, nil, jev.WithKey("test-key"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if pass.LogPath != LogPath(town) || pass.CursorPath != CursorPath(town) {
		t.Errorf("paths %q and %q, want the town's log and cursor", pass.LogPath, pass.CursorPath)
	}
	if pass.Asker == nil || pass.State == nil {
		t.Fatal("Open left the Asker or the state reader unset")
	}
	if ok, err := pass.Allows(); err != nil || !ok {
		t.Errorf("Allows on an empty ledger = %v, %v, want true", ok, err)
	}

	// One million input tokens at 1000 USD per billion is one dollar, spent
	// by a caller other than the attention battery.
	ledger := jev.UsageLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"ts": time.Now().UTC(), "caller": "jev-dedupe", "input_tokens": 1_000_000, "output_tokens": 0})
	if err := os.WriteFile(ledger, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := pass.Allows()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Allows = true with another caller's dollar in the ledger, want false at the default cap")
	}
}
