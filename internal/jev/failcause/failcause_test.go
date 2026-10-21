package failcause

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// A reached spend cap asks nothing and writes nothing.
func TestJudgeCapReachedAsksNothing(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f)
	p.Allows = func() (bool, error) { return false, nil }

	if err := p.Judge(context.Background(), Landing{Bead: "gt-abc", Rig: "gastown", Tail: fixtureTail(t)}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls with the cap reached, want none", len(f.requests))
	}
	if _, statErr := os.Stat(p.LogPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the log exists with the cap reached (stat err %v), want no line", statErr)
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
