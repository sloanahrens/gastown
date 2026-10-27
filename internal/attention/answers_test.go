package attention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// answersBase is the raise time the fixture lines share.
var answersBase = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// answersLine is one answers-log line for an item raised at answersBase +
// raisedMin, in the shape the battery writes.
func answersLine(key string, raisedMin int, needsHuman float64, answered bool) []byte {
	l := map[string]any{
		"ts":         answersBase.Add(time.Hour),
		"key":        key,
		"kind":       string(KindRedMain),
		"severity":   string(SeverityHigh),
		"first_seen": answersBase.Add(time.Duration(raisedMin) * time.Minute),
		"model":      "jev-test",
		"answers":    nil,
	}
	if answered {
		l["answers"] = map[string]any{"needs_human": needsHuman}
	}
	raw, err := json.Marshal(l)
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func appendAnswers(t *testing.T, path string, lines ...[]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: a test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, l := range lines {
		if _, err := f.Write(l); err != nil {
			t.Fatal(err)
		}
	}
}

// mustScores reads the scores, failing the test on a read error.
func mustScores(t *testing.T, r *ScoreReader) Scores {
	t.Helper()
	got, err := r.Scores()
	if err != nil {
		t.Fatalf("reading the answers: %v", err)
	}
	return got
}

// The reader answers per item life, newest answer first, and a line whose call
// failed leaves the answer it already had. A missing log is no answers rather
// than an error: the queue still lists every item, unscored.
func TestScoreReaderReadsTheNewestAnswerPerLife(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	r := NewScoreReader(path)
	if got := mustScores(t, r); len(got) != 0 {
		t.Fatalf("a missing log yielded %v, want no answers", got)
	}

	appendAnswers(t, path,
		answersLine("red-main:gastown", 0, 0.30, true),
		answersLine("stall:gastown/opal", 1, 0.80, true),
	)
	got := mustScores(t, r)
	if lost := LifeKey("red-main:gastown", answersBase); got[lost] != 0.30 {
		t.Errorf("the first item scores %v, want 0.30", got[lost])
	}
	if stall := LifeKey("stall:gastown/opal", answersBase.Add(time.Minute)); got[stall] != 0.80 {
		t.Errorf("the second item scores %v, want 0.80", got[stall])
	}

	// The same raise asked again gets a newer answer, and a failed call after
	// it leaves that answer standing.
	appendAnswers(t, path,
		answersLine("red-main:gastown", 0, 0.65, true),
		answersLine("stall:gastown/opal", 1, 0, false),
	)
	got = mustScores(t, r)
	if red := LifeKey("red-main:gastown", answersBase); got[red] != 0.65 {
		t.Errorf("the re-asked item scores %v, want the newer 0.65", got[red])
	}
	if stall := LifeKey("stall:gastown/opal", answersBase.Add(time.Minute)); got[stall] != 0.80 {
		t.Errorf("a failed re-ask lost the 0.80 answer, leaving %v", got[stall])
	}

	// A recurrence is a new life with its own answer, and the first life's
	// answer is not handed to it.
	appendAnswers(t, path, answersLine("red-main:gastown", 30, 0.10, true))
	got = mustScores(t, r)
	if again := LifeKey("red-main:gastown", answersBase.Add(30*time.Minute)); got[again] != 0.10 {
		t.Errorf("the recurrence scores %v, want 0.10", got[again])
	}
	if first := LifeKey("red-main:gastown", answersBase); got[first] != 0.65 {
		t.Errorf("the first life lost its 0.65 answer when the item recurred: %v", got[first])
	}
}

// A caller's map is its own: a later read must not change the answers it was
// handed, which the dashboard holds while it publishes the next one.
func TestScoreReaderHandsEachCallItsOwnMap(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	r := NewScoreReader(path)
	appendAnswers(t, path, answersLine("red-main:gastown", 0, 0.4, true))
	held := mustScores(t, r)
	if got := held[LifeKey("red-main:gastown", answersBase)]; got != 0.4 {
		t.Fatalf("the first read scores %v, want 0.4", got)
	}

	appendAnswers(t, path, answersLine("red-main:gastown", 0, 0.9, true))
	fresh := mustScores(t, r)
	if got := fresh[LifeKey("red-main:gastown", answersBase)]; got != 0.9 {
		t.Errorf("the new read scores %v, want the newer 0.9", got)
	}
	if got := held[LifeKey("red-main:gastown", answersBase)]; got != 0.4 {
		t.Errorf("the map the first call returned changed to %v under the second read, want the 0.4 it was given", got)
	}
}

// A line the pass is still writing is not consumed: the reader stops at the
// last newline, so the finished line's answer is read rather than lost to a
// half-written one.
func TestScoreReaderWaitsForTheEndOfALine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	full := answersLine("red-main:gastown", 0, 0.4, true)
	appendAnswers(t, path, full)
	r := NewScoreReader(path)
	if got := mustScores(t, r)[LifeKey("red-main:gastown", answersBase)]; got != 0.4 {
		t.Fatalf("the finished line scores %v, want 0.4", got)
	}

	// Half of the next line, with no newline yet.
	half := answersLine("stall:gastown/opal", 1, 0.9, true)
	appendAnswers(t, path, half[:len(half)/2])
	if got := mustScores(t, r); len(got) != 1 {
		t.Errorf("a half-written line added an answer: %v", got)
	}

	appendAnswers(t, path, half[len(half)/2:])
	if got := mustScores(t, r)[LifeKey("stall:gastown/opal", answersBase.Add(time.Minute))]; got != 0.9 {
		t.Errorf("the finished second line scores %v, want 0.9", got)
	}
}

// A log replaced with a shorter one is read from the start: an answer written
// before the replacement must not score the new file's items.
func TestScoreReaderStartsOverWhenTheLogShrinks(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	r := NewScoreReader(path)
	appendAnswers(t, path,
		answersLine("red-main:gastown", 0, 0.4, true),
		answersLine("stall:gastown/opal", 1, 0.9, true),
	)
	_ = mustScores(t, r)

	if err := os.WriteFile(path, answersLine("stall:gastown/opal", 1, 0.2, true), 0o644); err != nil { //nolint:gosec // G304: a test's own temp file
		t.Fatal(err)
	}
	got := mustScores(t, r)
	if _, ok := got[LifeKey("red-main:gastown", answersBase)]; ok {
		t.Errorf("the replaced log still scores the item it no longer names: %v", got)
	}
	if stall := LifeKey("stall:gastown/opal", answersBase.Add(time.Minute)); got[stall] != 0.2 {
		t.Errorf("the new file's item scores %v, want 0.2", got[stall])
	}
}

// A log replaced with one that is not shorter is read from the start too: the
// previous read offset lands inside the new file's lines, so continuing from it
// would keep every answer the old file gave.
func TestScoreReaderStartsOverWhenTheLogIsReplacedLarger(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	r := NewScoreReader(path)
	appendAnswers(t, path,
		answersLine("red-main:gastown", 0, 0.4, true),
		answersLine("stall:gastown/opal", 1, 0.9, true),
	)
	_ = mustScores(t, r)

	// Rewritten in place, longer than before, and naming different lives.
	replacement := append(answersLine("landing:gt-2", 2, 0.3, true), answersLine("host:disk", 3, 0.6, true)...)
	replacement = append(replacement, answersLine("red-main:gastown", 9, 0.7, true)...)
	if err := os.WriteFile(path, replacement, 0o644); err != nil { //nolint:gosec // G304: a test's own temp file
		t.Fatal(err)
	}

	got := mustScores(t, r)
	for _, stale := range []string{
		LifeKey("red-main:gastown", answersBase),
		LifeKey("stall:gastown/opal", answersBase.Add(time.Minute)),
	} {
		if _, ok := got[stale]; ok {
			t.Errorf("the replaced log still scores the old life %s: %v", stale, got)
		}
	}
	want := map[string]float64{
		LifeKey("landing:gt-2", answersBase.Add(2*time.Minute)):     0.3,
		LifeKey("host:disk", answersBase.Add(3*time.Minute)):        0.6,
		LifeKey("red-main:gastown", answersBase.Add(9*time.Minute)): 0.7,
	}
	for life, score := range want {
		if got[life] != score {
			t.Errorf("the new file's life %s scores %v, want %.1f (all: %v)", life, got[life], score, got)
		}
	}
}

// An unreadable log is named, not drawn as unanswered: the read returns the
// error beside the answers of its last good read, so the queue can say the
// scores are unreadable rather than showing every item as one Jev never
// answered (gt-b9j3o.2).
func TestScoreReaderReportsALogItCannotRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	r := NewScoreReader(path)
	appendAnswers(t, path, answersLine("red-main:gastown", 0, 0.4, true))
	if got := mustScores(t, r)[LifeKey("red-main:gastown", answersBase)]; got != 0.4 {
		t.Fatalf("the first read scores %v, want 0.4", got)
	}

	// The log is replaced by something that is not a file: it stats, it opens,
	// and it cannot be read.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil { //nolint:gosec // G301: a test's own temp dir
		t.Fatal(err)
	}
	got, err := r.Scores()
	if err == nil {
		t.Fatalf("a log that could not be read reported no error, scores %v", got)
	}
	if score := got[LifeKey("red-main:gastown", answersBase)]; score != 0.4 {
		t.Errorf("the last good answer is %v after a failed read, want the 0.4 it read", score)
	}

	// The read recovers once the log is a file again.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	appendAnswers(t, path, answersLine("stall:gastown/opal", 1, 0.8, true))
	got = mustScores(t, r)
	if score := got[LifeKey("stall:gastown/opal", answersBase.Add(time.Minute))]; score != 0.8 {
		t.Errorf("the answer after the log came back is %v, want 0.8", score)
	}
}

// A path the reader cannot even open is reported too, and it is not read as a
// log the battery never wrote: the error is what tells the two apart.
func TestScoreReaderReportsALogItCannotOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "attention.jsonl")
	// A symlink to itself: every stat and open of it fails, and not because
	// the log is missing.
	if err := os.Symlink(path, path); err != nil {
		t.Fatalf("creating the self-referential symlink fixture: %v", err)
	}
	got, err := NewScoreReader(path).Scores()
	if err == nil {
		t.Fatalf("a log that could not be opened reported no error, scores %v", got)
	}
	if len(got) != 0 {
		t.Errorf("a log that was never read yielded %v", got)
	}
}
