package failcause

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/land"
)

// jevReply answers both questions about the fixture's test.
const jevReply = `{
  "model": "jev-1.13.0",
  "answers": {
    "will_recur": {"type": "noul", "noul": 0.8},
    "cause": {"type": "choice", "choice": "shared_state", "confidence": 0.6,
      "probabilities": {"shared_state": 0.6, "timing_assumption": 0.1, "host_environment": 0.1,
        "test_bug": 0.05, "product_regression": 0.05, "unclear": 0.1}}
  },
  "usage": {"input_tokens": 120, "output_tokens": 24}
}`

// fakeJev is a transport that records each request body and answers with
// body and status, so the test drives the real Jev client without a port.
type fakeJev struct {
	status   int
	body     string
	requests [][]byte
}

func (f *fakeJev) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	f.requests = append(f.requests, body)
	rec := httptest.NewRecorder()
	if f.status != 0 {
		rec.WriteHeader(f.status)
	}
	_, _ = io.WriteString(rec, f.body)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func newJevClient(t *testing.T, f *fakeJev) *jev.Client {
	t.Helper()
	c, err := jev.NewClient(jev.WithBaseURL("http://jev.invalid/v1/systemone"), jev.WithKey("test-key"),
		jev.WithHTTPClient(&http.Client{Transport: f}))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fixtureTail is the excerpt a red gate left: one failing test in one package.
func fixtureTail(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "excerpt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// newPass is a pass over a fresh town with a fake Jev that answers jevReply.
func newPass(t *testing.T, f *fakeJev) *Pass {
	t.Helper()
	return &Pass{
		Asker:   newJevClient(t, f),
		LogPath: LogPath(t.TempDir()),
		Allows:  func() (bool, error) { return true, nil },
		Now:     func() time.Time { return time.Date(2026, 10, 21, 14, 2, 11, 0, time.UTC) },
	}
}

// readLines decodes every line of the cause log at path.
func readLines(t *testing.T, path string) []Line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Line
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var l Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

// A fixture failing-test excerpt produces one line with both answers.
func TestJudgeLogsOneLineWithBothAnswers(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)

	err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	lines := readLines(t, p.LogPath)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want one for the one failing test", len(lines))
	}
	l := lines[0]
	if l.Rig != "gastown" || l.Bead != "gt-abc" {
		t.Errorf("line names %s/%s, want gastown/gt-abc", l.Rig, l.Bead)
	}
	if l.Package != "github.com/steveyegge/gastown/internal/render" || l.Test != "TestRenderTable" {
		t.Errorf("line names %s %s, want the render package's TestRenderTable", l.Package, l.Test)
	}
	if got := l.Answers[QWillRecur].Noul; got == nil || *got != 0.8 {
		t.Errorf("will_recur noul = %v, want 0.8", got)
	}
	if got := l.Answers[QCause].Choice; got != "shared_state" {
		t.Errorf("cause choice = %q, want shared_state", got)
	}
	if len(l.Answers[QCause].Probabilities) != len(causes) {
		t.Errorf("cause probabilities hold %d options, want %d", len(l.Answers[QCause].Probabilities), len(causes))
	}
	if l.Model != "jev-1.13.0" || l.QuestionVersion != Version {
		t.Errorf("model %q version %d, want jev-1.13.0 and %d", l.Model, l.QuestionVersion, Version)
	}
	if !l.TS.Equal(time.Date(2026, 10, 21, 14, 2, 11, 0, time.UTC)) {
		t.Errorf("TS = %s, want the judging time", l.TS)
	}
	if len(f.requests) != 1 {
		t.Errorf("made %d Jev calls, want one call per failing test", len(f.requests))
	}
}

// A Jev outage writes no line and returns the error, so the next red verdict
// is judged afresh.
func TestJudgeOutageLeavesNoLine(t *testing.T) {
	t.Parallel()
	f := &fakeJev{status: http.StatusServiceUnavailable, body: `{"error":"down"}`}
	p := newPass(t, f)

	err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)})
	if err == nil {
		t.Fatal("Judge against an outage returned nil, want the error")
	}
	if _, statErr := os.Stat(p.LogPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the log exists after an outage (stat err %v), want no line", statErr)
	}
}

// A reply that is missing one of the two answers is not a usable answer: no
// line is written for it.
func TestJudgeBadReplyLeavesNoLine(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: `{"model":"jev-1.13.0","answers":{"will_recur":{"type":"noul","noul":0.8}},"usage":{}}`}
	p := newPass(t, f)

	err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)})
	if !errors.Is(err, jev.ErrBadResponse) {
		t.Errorf("Judge = %v, want ErrBadResponse", err)
	}
	if _, statErr := os.Stat(p.LogPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the log exists after a bad reply (stat err %v), want no line", statErr)
	}
}

// A reached spend cap asks nothing, writes nothing, and says so, so the
// shadow can tell a battery stopped by spend from a verdict with no test in its
// excerpt.
func TestJudgeCapReachedAsksNothing(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	p.Allows = func() (bool, error) { return false, nil }

	err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)})
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("Judge = %v, want ErrCapReached", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls with the cap reached, want none", len(f.requests))
	}
	if _, statErr := os.Stat(p.LogPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the log exists with the cap reached (stat err %v), want no line", statErr)
	}
}

// A spend ledger that cannot be read stops the judging with its error, and no
// line is written for the test that was never judged.
func TestJudgeSpendReadFailureIsAnError(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	p.Allows = func() (bool, error) { return false, errors.New("the ledger is locked") }

	err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)})
	if !errors.Is(err, ErrSpendUnreadable) || !strings.Contains(err.Error(), "the ledger is locked") {
		t.Fatalf("Judge = %v, want ErrSpendUnreadable carrying the ledger's failure", err)
	}
	if errors.Is(err, ErrCapReached) {
		t.Error("a spend ledger failure reads as the cap being reached, want the two told apart")
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls on an unreadable ledger, want none", len(f.requests))
	}
}

// An excerpt that names no failing test asks nothing.
func TestJudgeWithoutAFailingTestAsksNothing(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)

	if err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: "ok  \texample.com/x\t0.1s\n"}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls for an excerpt with no failing test, want none", len(f.requests))
	}
}

// The excerpt Jev reads is capped, so a long log cannot price a call out of
// the battery.
func TestJudgeCapsTheExcerptJevReads(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	tail := fixtureTail(t) + strings.Repeat("x", 10*maxExcerptRunes)

	if err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: tail}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(f.requests) != 1 {
		t.Fatalf("made %d Jev calls, want 1", len(f.requests))
	}
	var sent struct {
		State state `json:"state"`
	}
	if err := json.Unmarshal(f.requests[0], &sent); err != nil {
		t.Fatalf("request: %v", err)
	}
	if n := len([]rune(sent.State.Excerpt)); n > maxExcerptRunes+1 {
		t.Errorf("excerpt is %d runes, want at most %d", n, maxExcerptRunes+1)
	}
}

// The cause options are the same, in the same order, on every call: the
// question is part of what the log's answers mean.
func TestCauseOptionsAreFixedAndOrdered(t *testing.T) {
	t.Parallel()
	want := []string{"shared_state", "timing_assumption", "host_environment", "test_bug", "product_regression", "unclear"}
	var got []string
	for _, c := range causes {
		got = append(got, c.name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cause options = %v, want %v", got, want)
	}

	opts := Questions()[QCause].Options
	if len(opts) != len(want) {
		t.Fatalf("the choice holds %d options, want %d", len(opts), len(want))
	}
	for _, name := range want {
		if opts[name] == "" {
			t.Errorf("option %q has no description", name)
		}
	}

	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	tail := fixtureTail(t)
	for range 2 {
		if err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: tail}); err != nil {
			t.Fatalf("Judge: %v", err)
		}
	}
	if len(f.requests) != 2 {
		t.Fatalf("made %d Jev calls for two judgements, want 2", len(f.requests))
	}
	if !bytes.Equal(f.requests[0], f.requests[1]) {
		t.Error("two judgements of one excerpt sent different requests: the question is not fixed")
	}
}

// fakeGate is a candidate gate that returns one canned result and counts its
// runs.
type fakeGate struct {
	res  land.CandidateResult
	runs int
}

func (g *fakeGate) Run(context.Context, land.Repo, string, land.Work, string) land.CandidateResult {
	g.runs++
	return g.res
}

func (g *fakeGate) Discard(context.Context, land.Work, land.CandidateResult) {}

// The shadow returns the gate's verdict exactly as the gate produced it, and
// the red verdict's excerpt is judged beside it.
func TestShadowReturnsTheVerdictUntouched(t *testing.T) {
	t.Parallel()
	red := land.CandidateResult{State: land.CandidateFailed, Branch: "land/gt-abc", SHA: "abc123", Tail: fixtureTail(t), RunStatus: "failure"}
	gate := &fakeGate{res: red}
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	s := Shadow{
		Gate:  gate,
		Pass:  p,
		Rig:   "gastown",
		Async: func(call func()) { call() },
	}

	got := s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

	if !reflect.DeepEqual(got, red) {
		t.Errorf("Run returned %+v, want the gate's %+v", got, red)
	}
	if len(readLines(t, p.LogPath)) != 1 {
		t.Error("the red verdict was not judged into one line")
	}
}

// A verdict that is not red, or a red one with no excerpt, asks nothing.
func TestShadowJudgesOnlyRedVerdictsWithAnExcerpt(t *testing.T) {
	t.Parallel()
	cases := map[string]land.CandidateResult{
		"passed":         {State: land.CandidatePassed, Tail: fixtureTail(t)},
		"silent":         {State: land.CandidateSilent},
		"red no excerpt": {State: land.CandidateFailed},
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := &fakeJev{body: jevReply}
			p := newPass(t, f)
			s := Shadow{Gate: &fakeGate{res: res}, Pass: p, Rig: "gastown", Async: func(call func()) { call() }}

			s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

			if len(f.requests) != 0 {
				t.Errorf("made %d Jev calls, want none", len(f.requests))
			}
		})
	}
}

// By default the judging runs off the landing's goroutine, so the landing does
// not wait on Jev. The verdict comes back before any call is made.
func TestShadowDefaultsToBackground(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	var pending []func()
	s := Shadow{
		Gate:  &fakeGate{res: land.CandidateResult{State: land.CandidateFailed, Tail: fixtureTail(t)}},
		Pass:  p,
		Rig:   "gastown",
		Async: func(call func()) { pending = append(pending, call) },
	}

	s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

	if len(f.requests) != 0 || len(pending) != 1 {
		t.Fatalf("Run made %d calls and queued %d judgements, want the judging queued and no call yet", len(f.requests), len(pending))
	}
	pending[0]()
	if len(f.requests) != 1 {
		t.Errorf("the queued judgement made %d calls, want 1", len(f.requests))
	}
}

// The shadow says which stop ended the judging, in its own words for each, so
// the two read apart in the daemon log. Neither changes the verdict.
func TestShadowSaysWhyTheJudgingStopped(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		allows func() (bool, error)
		want   []string
	}{
		"cap reached": {func() (bool, error) { return false, nil },
			[]string{"the day's Jev spend cap was reached", "remaining failing tests were not judged"}},
		"ledger error": {func() (bool, error) { return false, errors.New("the ledger is locked") },
			[]string{"the day's Jev spend could not be read", "no failing test was judged", "the ledger is locked"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			red := land.CandidateResult{State: land.CandidateFailed, Tail: fixtureTail(t)}
			f := &fakeJev{body: jevReply}
			p := newPass(t, f)
			p.Allows = tc.allows
			var logged []string
			s := Shadow{
				Gate:  &fakeGate{res: red},
				Pass:  p,
				Rig:   "gastown",
				Async: func(call func()) { call() },
				Logf:  func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
			}

			got := s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

			if !reflect.DeepEqual(got, red) {
				t.Errorf("Run returned %+v, want the gate's %+v", got, red)
			}
			if len(logged) != 1 {
				t.Fatalf("logged %d lines, want the one stop reported: %v", len(logged), logged)
			}
			for _, want := range append(tc.want, "gt-abc") {
				if !strings.Contains(logged[0], want) {
					t.Errorf("logged %q, want %q in it", logged[0], want)
				}
			}
		})
	}
}

// A judgement that fails is reported through Logf, naming the bead and the
// battery, and the verdict still comes back untouched.
func TestShadowReportsAFailedJudgement(t *testing.T) {
	t.Parallel()
	red := land.CandidateResult{State: land.CandidateFailed, Tail: fixtureTail(t)}
	var logged []string
	s := Shadow{
		Gate:  &fakeGate{res: red},
		Pass:  newPass(t, &fakeJev{status: http.StatusServiceUnavailable, body: `{"error":"down"}`}),
		Rig:   "gastown",
		Async: func(call func()) { call() },
		Logf:  func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}

	got := s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

	if !reflect.DeepEqual(got, red) {
		t.Errorf("Run returned %+v, want the gate's %+v", got, red)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d lines, want the one failure reported: %v", len(logged), logged)
	}
	for _, want := range []string{"jev_failcause:", "gt-abc", "not logged"} {
		if !strings.Contains(logged[0], want) {
			t.Errorf("logged %q, want %q in it", logged[0], want)
		}
	}
}

// A shadow with no Logf stays quiet rather than panicking on a failed
// judgement.
func TestShadowWithoutLogfReportsNothing(t *testing.T) {
	t.Parallel()
	red := land.CandidateResult{State: land.CandidateFailed, Tail: fixtureTail(t)}
	s := Shadow{
		Gate:  &fakeGate{res: red},
		Pass:  newPass(t, &fakeJev{status: http.StatusServiceUnavailable, body: `{"error":"down"}`}),
		Rig:   "gastown",
		Async: func(call func()) { call() },
	}

	if got := s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123"); !reflect.DeepEqual(got, red) {
		t.Errorf("Run returned %+v, want the gate's %+v", got, red)
	}
}

// oneAtATime is an Asker that holds every call until the test releases it and
// records any moment two calls are in flight together. A call answers both
// questions, except the ones bad names, which answer nothing usable.
type oneAtATime struct {
	started chan struct{}
	release chan struct{}

	mu       sync.Mutex
	inFlight int
	overlap  bool
	done     int
	calls    int
	bad      map[int]bool
}

func (a *oneAtATime) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	a.mu.Lock()
	a.inFlight++
	if a.inFlight > 1 {
		a.overlap = true
	}
	n := a.calls
	a.calls++
	a.mu.Unlock()

	a.started <- struct{}{}
	<-a.release

	a.mu.Lock()
	a.inFlight--
	a.done++
	a.mu.Unlock()

	got := &jev.Answers{
		Model:  "jev-1.13.0",
		Noul:   map[string]float64{QWillRecur: 0.8},
		Choice: map[string]jev.ChoiceAnswer{QCause: {Choice: "test_bug", Probabilities: map[string]float64{"test_bug": 1}}},
	}
	if a.bad[n] {
		got.Noul = nil
	}
	return got, nil
}

func (a *oneAtATime) finished() (done int, overlap bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done, a.overlap
}

// One judgement asks Jev at a time: a red verdict that arrives while another is
// being judged waits for the slot instead of asking beside it, and is judged
// once the slot frees rather than dropped.
func TestShadowJudgesOneVerdictAtATime(t *testing.T) {
	t.Parallel()
	// The first judgement is held in Jev while the test hears for the second,
	// so a second ask arriving inside that window is the overlap this test is
	// about. The window only has to outlast a goroutine that is already ready.
	quiet, quietCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer quietCancel()
	// The waits for what a serialized battery must do are generous, so a
	// regression fails as a message rather than as a package timeout.
	patience, patienceCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer patienceCancel()

	// The second judgement answers nothing usable, so its error report is the
	// signal that it ran to the end: nothing this test started is still going
	// when it returns.
	asker := &oneAtATime{started: make(chan struct{}), release: make(chan struct{}), bad: map[int]bool{1: true}}
	p := newPass(t, &fakeJev{body: jevReply})
	p.Asker = asker
	reported := make(chan struct{}, 1)
	s := Shadow{
		Gate: &fakeGate{res: land.CandidateResult{State: land.CandidateFailed, Tail: fixtureTail(t)}},
		Pass: p,
		Rig:  "gastown",
		Logf: func(string, ...any) { reported <- struct{}{} },
	}

	s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-one"}, "abc123")
	s.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-two"}, "abc123")

	select {
	case <-asker.started: // the first judgement is in Jev, holding the slot
	case <-patience.Done():
		t.Fatal("neither judgement reached Jev")
	}
	select {
	case <-asker.started:
		t.Fatal("a second judgement asked Jev while the first was still asking, want one at a time")
	case <-quiet.Done(): // expected: the second waits for the slot
	}
	asker.release <- struct{}{}
	select {
	case <-asker.started: // the second judgement begins once the first is done
	case <-patience.Done():
		t.Fatal("the second judgement never ran: the wait must queue it, not drop it")
	}
	asker.release <- struct{}{}
	select {
	case <-reported:
	case <-patience.Done():
		t.Fatal("the second judgement never finished")
	}

	if _, overlap := asker.finished(); overlap {
		t.Error("two judgements asked Jev at once, want one at a time")
	}
	if lines := readLines(t, p.LogPath); len(lines) != 1 {
		t.Errorf("the log holds %d lines, want the one line the answering judgement wrote", len(lines))
	}
}

// The battery's own words never use the word the spec bars: no file in the
// package, its fixtures or its docs says it. The needle is assembled so this
// file does not match itself.
func TestNoBannedWordInThePackage(t *testing.T) {
	t.Parallel()
	needle := "fl" + "ak"
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(data)), needle) {
			t.Errorf("%s says the banned word", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
