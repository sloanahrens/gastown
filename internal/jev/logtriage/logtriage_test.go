package logtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// The fixture log's lines. Two tick lines and two merge lines share a key,
// the warning stands alone, and the last line has no word the pass reads.
const (
	lineTick1  = "2026/10/21 03:31:35 spec_dispatch: tick: 57 candidate(s), 2 refused, 0 planning"
	lineTick2  = "2026/10/21 03:31:50 spec_dispatch: tick: 58 candidate(s), 1 refused, 0 planning"
	lineMerge1 = "2026/10/21 03:32:01 landing_worker: gt-rpdhw: merge failed at 77b9028a after 3s"
	lineMerge2 = "2026/10/21 03:33:12 landing_worker: gt-4mtmy: merge failed at 34cd71ae after 14s"
	lineWarn   = "2026/10/21 03:34:00 kernel: WARNING: disk nearly full"
	lineQuiet  = "2026/10/21 03:34:01 jsonl_git_backup: events: exported 2786 rows in 306ms"

	keyTick  = "<time> spec_dispatch: tick: <n> candidate(s), <n> refused, <n> planning"
	keyMerge = "<time> landing_worker: <id>: merge failed at <hash> after <dur>"
	keyWarn  = "<time> kernel: WARNING: disk nearly full"
)

// jevReply answers the five questions for one group: routine, a severity of
// watch and the expected_noise category.
const jevReply = `{
  "model": "jev-1.13.0",
  "answers": {
    "is_routine_noise": {"type": "noul", "noul": 0.9},
    "needs_action": {"type": "noul", "noul": 0.1},
    "auto_remediable": {"type": "noul", "noul": 0.2},
    "severity": {"type": "score", "score": 1, "confidence": 0.7,
      "legend": {"info": "Routine.", "watch": "Note it.", "review": "Look soon.", "page": "Act now."},
      "probabilities": {"info": 0.1, "watch": 0.6, "review": 0.2, "page": 0.1}},
    "category": {"type": "choice", "choice": "expected_noise", "confidence": 0.8,
      "probabilities": {"dolt": 0.02, "landing": 0.02, "dispatch": 0.02, "polecat": 0.02, "gate": 0.02,
        "network": 0.02, "config": 0.02, "daemon_bug": 0.02, "expected_noise": 0.82, "unclear": 0.02}}
  },
  "usage": {"input_tokens": 100, "output_tokens": 20}
}`

// fakeJev is a transport that records each request body and answers with
// body and status, so the test drives the real Jev client without a port. A
// call numbered failCall answers a server error instead, so a test can fail one
// call of a run.
type fakeJev struct {
	status   int
	body     string
	failCall int
	requests [][]byte
}

func (f *fakeJev) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	f.requests = append(f.requests, body)
	status, reply := f.status, f.body
	if f.failCall > 0 && len(f.requests) == f.failCall {
		status, reply = http.StatusBadGateway, `{"error":"down"}`
	}
	rec := httptest.NewRecorder()
	if status != 0 {
		rec.WriteHeader(status)
	}
	_, _ = io.WriteString(rec, reply)
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

// fixture is a town's daemon log, triage log and cursor under one temp dir.
type fixture struct {
	t    *testing.T
	pass *Pass
	jev  *fakeJev
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fakeJev{body: jevReply}
	fx := &fixture{t: t, jev: f}
	fx.pass = &Pass{
		Asker:      newJevClient(t, f),
		DaemonLog:  DaemonLogPath(dir),
		LogPath:    LogPath(dir),
		CursorPath: CursorPath(dir),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return time.Date(2026, 10, 21, 3, 40, 0, 0, time.UTC) },
	}
	return fx
}

// appendLog adds text to the daemon log as it is, so a test can end a line
// without its newline.
func (fx *fixture) appendLog(text string) {
	fx.t.Helper()
	path := fx.pass.DaemonLog
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(text); err != nil {
		fx.t.Fatal(err)
	}
}

// logLines appends the lines to the daemon log, each with its newline.
func (fx *fixture) logLines(lines ...string) {
	fx.t.Helper()
	fx.appendLog(strings.Join(lines, "\n") + "\n")
}

// triage reads every line the triage log holds.
func (fx *fixture) triage() []Line {
	fx.t.Helper()
	var lines []Line
	if err := battery.ReadRecent(fx.pass.LogPath, math.MaxInt, &lines); err != nil {
		fx.t.Fatal(err)
	}
	return lines
}

// run does one pass and fails the test on an error.
func (fx *fixture) run() Summary {
	fx.t.Helper()
	sum, err := fx.pass.Run(context.Background())
	if err != nil {
		fx.t.Fatalf("Run: %v", err)
	}
	return sum
}

// groupLines returns n groups of letter-named components, group i holding i+1
// lines, so the largest groups are the last ones.
func groupLines(n int) []string {
	var out []string
	for i := range n {
		name := string(rune('a'+i/26)) + string(rune('a'+i%26))
		for range i + 1 {
			out = append(out, fmt.Sprintf("2026/10/21 03:31:34 error: component %s crashed", name))
		}
	}
	return out
}

// TestJevLogtriagePassGroupsTheFixtureLog is the acceptance fixture through the
// real Jev client: the warning and error lines group by key with their counts,
// the quiet line is never read, and each group's line carries five answers.
func TestJevLogtriagePassGroupsTheFixtureLog(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineTick1, lineMerge1, lineQuiet, lineWarn, lineTick2, lineMerge2)

	sum := fx.run()
	if sum.Matched != 5 || sum.Groups != 3 || sum.Asked != 3 || sum.Unanswered != 0 || sum.Dropped != 0 {
		t.Fatalf("summary = %+v, want 5 matched, 3 groups, 3 asked", sum)
	}
	if len(fx.jev.requests) != 3 {
		t.Fatalf("made %d Jev calls, want one per group", len(fx.jev.requests))
	}

	byKey := map[string]Line{}
	for _, l := range fx.triage() {
		byKey[l.Key] = l
	}
	if got := byKey[keyTick].Count; got != 2 {
		t.Errorf("tick count = %d, want 2", got)
	}
	if got := byKey[keyMerge].Count; got != 2 {
		t.Errorf("merge count = %d, want 2", got)
	}
	if got := byKey[keyWarn].Count; got != 1 {
		t.Errorf("warning count = %d, want 1", got)
	}
	tick := byKey[keyTick]
	if !tick.First.Equal(time.Date(2026, 10, 21, 3, 31, 35, 0, time.Local)) ||
		!tick.Last.Equal(time.Date(2026, 10, 21, 3, 31, 50, 0, time.Local)) {
		t.Errorf("tick range = %v to %v, want 03:31:35 to 03:31:50", tick.First, tick.Last)
	}
	if len(tick.Samples) != 2 || tick.Samples[0] != lineTick1 || tick.Samples[1] != lineTick2 {
		t.Errorf("tick samples = %q, want its two lines in file order", tick.Samples)
	}

	for key, l := range byKey {
		if l.Reason != "" || l.Model != "jev-1.13.0" || l.QuestionVersion != Version {
			t.Errorf("%s: reason %q model %q version %d, want an answered line", key, l.Reason, l.Model, l.QuestionVersion)
		}
		assertFiveAnswers(t, key, l.Answers)
	}

	var samples []string
	for _, body := range fx.jev.requests {
		var req struct {
			State struct {
				Samples []string `json:"samples"`
			} `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Questions) != 5 {
			t.Errorf("request asks %d questions, want the five", len(req.Questions))
		}
		samples = append(samples, req.State.Samples...)
	}
	if !strings.Contains(strings.Join(samples, "\n"), "1 refused") {
		t.Errorf("requests quote samples %q, want the tick group's own lines", samples)
	}
}

// assertFiveAnswers checks that a line holds every question's answer with the
// right shape: three nouls, a severity score and a category choice.
func assertFiveAnswers(t *testing.T, key string, got map[string]Answer) {
	t.Helper()
	for _, id := range []string{QRoutineNoise, QNeedsAction, QAutoRemediate} {
		if a := got[id]; a.Noul == nil {
			t.Errorf("%s: %s has no noul answer", key, id)
		}
	}
	if a := got[QSeverity]; a.Score == nil || a.Confidence == nil || *a.Score != 1 {
		t.Errorf("%s: severity = %+v, want score 1 with a confidence", key, a)
	}
	if a := got[QCategory]; a.Choice != "expected_noise" || len(a.Probabilities) != 10 {
		t.Errorf("%s: category = %+v, want expected_noise over ten options", key, a)
	}
	if n := len(got); n != 5 {
		t.Errorf("%s: %d answers, want 5", key, n)
	}
}

// TestJevLogtriageSecondPassAsksNothingTwice: lines already read are not read
// again, and a line appended later is the only one a pass asks about.
func TestJevLogtriageSecondPassAsksNothingTwice(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineTick1)
	fx.run()
	fx.jev.requests = nil

	fx.logLines(lineWarn)
	sum := fx.run()
	if sum.Matched != 1 || sum.Asked != 1 || len(fx.jev.requests) != 1 {
		t.Errorf("pass after one new line = %+v with %d calls, want one line asked", sum, len(fx.jev.requests))
	}
	if n := len(fx.triage()); n != 2 {
		t.Errorf("triage log has %d lines, want one per group asked", n)
	}
}

// TestJevLogtriageCursorSkipsAPartialLine: a line the daemon has not finished
// writing waits for its newline, and is read whole by the next pass.
func TestJevLogtriageCursorSkipsAPartialLine(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.appendLog("2026/10/21 03:34:00 kernel: WARNING: disk near")
	if sum := fx.run(); sum.Matched != 0 {
		t.Fatalf("pass over a partial line matched %d, want 0", sum.Matched)
	}

	fx.appendLog("ly full\n")
	if sum := fx.run(); sum.Matched != 1 || sum.Asked != 1 {
		t.Errorf("pass after the newline = %+v, want the whole line asked", sum)
	}
	if lines := fx.triage(); len(lines) != 1 || lines[0].Key != "<time> kernel: WARNING: disk nearly full" {
		t.Errorf("triage log = %+v, want the whole line's group", lines)
	}
}

// TestJevLogtriageCapsGroupsLargestFirst: the pass asks about at most
// MaxGroupsPerPass groups, the largest, and counts the rest as dropped.
func TestJevLogtriageCapsGroupsLargestFirst(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(groupLines(MaxGroupsPerPass + 5)...)

	sum := fx.run()
	if sum.Groups != MaxGroupsPerPass+5 || sum.Asked != MaxGroupsPerPass || sum.Dropped != 5 {
		t.Fatalf("summary = %+v, want %d groups, %d asked, 5 dropped", sum, MaxGroupsPerPass+5, MaxGroupsPerPass)
	}
	if len(fx.jev.requests) != MaxGroupsPerPass {
		t.Errorf("made %d calls, want %d", len(fx.jev.requests), MaxGroupsPerPass)
	}
	for _, l := range fx.triage() {
		if l.Count < 6 {
			t.Errorf("logged a group of %d lines, want only the largest 50 (counts 6 and up)", l.Count)
		}
	}
}

// TestJevLogtriageCapStopsTheCallsWhenSpent: a cap that runs out mid-pass ends
// the asks there, logs what it asked, and drops the rest without reading them
// again.
func TestJevLogtriageCapStopsTheCallsWhenSpent(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(groupLines(5)...)
	// The first check is the pass's own, then one per group: two groups fit.
	allowed := []bool{true, true, true, false}
	fx.pass.Allows = func() (bool, error) {
		ok := allowed[0]
		if len(allowed) > 1 {
			allowed = allowed[1:]
		}
		return ok, nil
	}

	sum := fx.run()
	if sum.Asked != 2 || sum.Dropped != 3 || !sum.CapReached || len(fx.jev.requests) != 2 {
		t.Errorf("summary = %+v with %d calls, want 2 asked, 3 dropped, cap reached", sum, len(fx.jev.requests))
	}
	if _, err := os.Stat(fx.pass.CursorPath); err != nil {
		t.Errorf("cursor not written after a capped pass: %v", err)
	}
}

// TestJevLogtriageNothingReadWhenCapIsSpentAtStart: a pass that starts with
// the cap spent reads nothing, and the cursor stays where it was so the lines
// wait for the next pass.
func TestJevLogtriageNothingReadWhenCapIsSpentAtStart(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)
	fx.pass.Allows = func() (bool, error) { return false, nil }

	sum := fx.run()
	if !sum.CapReached || sum.Matched != 0 || len(fx.jev.requests) != 0 {
		t.Errorf("summary = %+v with %d calls, want the cap reached and nothing read", sum, len(fx.jev.requests))
	}
	if _, err := os.Stat(fx.pass.CursorPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cursor exists after a pass that read nothing: %v", err)
	}
	if _, err := os.Stat(fx.pass.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("triage log exists after a pass that asked nothing: %v", err)
	}
}

// TestJevLogtriageFailedAskLogsItsReason: a call that got no usable reply logs
// its reason and no answers, and the lines are not asked again.
func TestJevLogtriageFailedAskLogsItsReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{"server error", http.StatusInternalServerError, `{"error":"down"}`, ReasonAskFailed},
		{"unusable reply", http.StatusOK, `{"answers":{}}`, ReasonBadResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t)
			fx.jev.status, fx.jev.body = tc.status, tc.body
			fx.logLines(lineWarn)

			sum := fx.run()
			if sum.Asked != 0 || sum.Unanswered != 1 {
				t.Fatalf("summary = %+v, want one unanswered group", sum)
			}
			lines := fx.triage()
			if len(lines) != 1 || lines[0].Reason != tc.reason || lines[0].Answers != nil {
				t.Errorf("line = %+v, want reason %q and no answers", lines, tc.reason)
			}
			if again := fx.run(); again.Matched != 0 {
				t.Errorf("second pass matched %d, want the failed lines not read again", again.Matched)
			}
		})
	}
}

// cancelDuring is an Asker whose nth call ends the pass's context during the
// call, as a budget running out mid-request would.
type cancelDuring struct {
	asker  Asker
	n      int
	calls  int
	cancel context.CancelFunc
}

func (c *cancelDuring) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	c.calls++
	if c.calls == c.n {
		c.cancel()
		return nil, ctx.Err()
	}
	return c.asker.Ask(ctx, state, qs)
}

// TestJevLogtriageDeadlineKeepsAnswers: a deadline that ends a pass partway
// keeps the answers it has and carries the groups it did not reach. The next
// pass asks those groups and adds the lines they gain in between to their
// counts.
func TestJevLogtriageDeadlineKeepsAnswers(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineTick1, lineMerge1, lineQuiet, lineWarn, lineTick2, lineMerge2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jevAsker := fx.pass.Asker
	fx.pass.Asker = &cancelDuring{asker: jevAsker, n: 3, cancel: cancel}

	sum, err := fx.pass.Run(ctx)
	if err != nil {
		t.Fatalf("Run = %v, want the deadline to end the pass without an error", err)
	}
	if sum.Asked != 2 || sum.Deferred != 1 || len(fx.jev.requests) != 2 {
		t.Fatalf("summary = %+v with %d calls, want 2 asked and 1 deferred", sum, len(fx.jev.requests))
	}
	answered := map[string]bool{}
	for _, l := range fx.triage() {
		answered[l.Key] = true
	}
	if len(answered) != 2 || !answered[keyMerge] || !answered[keyTick] || answered[keyWarn] {
		t.Fatalf("triage log holds groups %v, want the two answered ones and not the deferred warning", answered)
	}

	fx.pass.Asker = jevAsker
	fx.logLines("2026/10/21 03:35:00 kernel: WARNING: disk nearly full")
	fx.jev.requests = nil
	sum = fx.run()
	if sum.Asked != 1 || sum.Deferred != 0 || len(fx.jev.requests) != 1 {
		t.Fatalf("next pass summary = %+v with %d calls, want the deferred group asked alone", sum, len(fx.jev.requests))
	}
	lines := fx.triage()
	if last := lines[len(lines)-1]; last.Key != keyWarn || last.Count != 2 || len(last.Samples) != 2 {
		t.Errorf("next pass logged %s with %d lines and %d samples, want the warning with its two lines", last.Key, last.Count, len(last.Samples))
	}
}

// TestJevLogtriageCancelDefersTheGroups: a pass whose context is done before it
// asks reads its lines, writes no triage line, and carries every group to the
// next pass, which asks them.
func TestJevLogtriageCancelDefersTheGroups(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sum, err := fx.pass.Run(ctx)
	if err != nil {
		t.Fatalf("Run = %v, want no error from a deadline", err)
	}
	if sum.Asked != 0 || sum.Deferred != 1 || len(fx.jev.requests) != 0 {
		t.Errorf("summary = %+v with %d calls, want the one group deferred", sum, len(fx.jev.requests))
	}
	if _, err := os.Stat(fx.pass.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("triage line written by a pass that asked nothing: %v", err)
	}

	sum = fx.run()
	if sum.Asked != 1 || len(fx.jev.requests) != 1 {
		t.Fatalf("next pass summary = %+v with %d calls, want the deferred group asked", sum, len(fx.jev.requests))
	}
	if lines := fx.triage(); len(lines) != 1 || lines[0].Key != keyWarn || lines[0].Count != 1 {
		t.Errorf("triage log = %+v, want the deferred warning group", lines)
	}
}

// TestJevLogtriageMissingLogIsEmpty: a town with no daemon log yet has nothing
// to triage and is not an error.
func TestJevLogtriageMissingLogIsEmpty(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	sum := fx.run()
	if sum.Matched != 0 || len(fx.jev.requests) != 0 {
		t.Errorf("summary = %+v with %d calls, want an empty pass", sum, len(fx.jev.requests))
	}
}

// TestJevLogtriageLockRefusesASecondPass: a pass that finds the run lock held
// returns ErrBusy and reads nothing.
func TestJevLogtriageLockRefusesASecondPass(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)
	release, err := battery.Lock(fx.pass.CursorPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := fx.pass.Run(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("Run = %v, want ErrBusy", err)
	}
	if len(fx.jev.requests) != 0 {
		t.Errorf("a busy pass made %d calls", len(fx.jev.requests))
	}
}

// TestJevLogtriageQuestionSet pins the question ids, types, levels and the
// category options the scorecard reads.
func TestJevLogtriageQuestionSet(t *testing.T) {
	t.Parallel()
	qs := Questions()
	wantTypes := map[string]string{
		QRoutineNoise: "noul", QNeedsAction: "noul", QAutoRemediate: "noul",
		QSeverity: "score", QCategory: "choice",
	}
	if len(qs) != len(wantTypes) {
		t.Errorf("%d questions, want %d", len(qs), len(wantTypes))
	}
	for id, typ := range wantTypes {
		if qs[id].Type != typ {
			t.Errorf("%s type = %q, want %q", id, qs[id].Type, typ)
		}
	}
	if got := strings.Join(qs[QSeverity].Levels, ","); got != "info,watch,review,page" {
		t.Errorf("severity levels = %s, want info,watch,review,page", got)
	}
	if n := len(qs[QCategory].Options); n != 10 {
		t.Errorf("category has %d options, want 10", n)
	}
	if Version != 1 {
		t.Errorf("Version = %d, want 1 until a question changes", Version)
	}
}

// TestJevLogtriageMatchedOn runs the filter over the daemon's own summary lines.
// A summary whose counts are all zero keeps nothing; the same summary with a
// count above zero keeps the line under the keyword that holds that count.
func TestJevLogtriageMatchedOn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, line, want string
	}{
		{
			name: "patrol summary with every count zero",
			line: "2026/10/21 11:32:44 patrol_scan: devops: checked 7 polecat(s): 0 restarted, 0 refused, 0 unknown, 0 molecule(s) closed, 0 reopened, 0 error(s), 0 reaped, 0 would-reap, 0 blocked",
			want: "",
		},
		{
			name: "landing pass with 0 failed",
			line: "2026/10/21 03:32:10 landing_worker: gastown: pass: 1 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed",
			want: "",
		},
		{
			name: "patrol summary with refused above zero",
			line: "2026/10/21 11:32:44 patrol_scan: devops: checked 7 polecat(s): 0 restarted, 2 refused, 0 unknown, 0 error(s), 0 blocked",
			want: "refused",
		},
		{
			name: "landing pass with failed above zero",
			line: "2026/10/21 03:32:10 landing_worker: gastown: pass: 1 landed, 0 repaired, 0 rejected, 0 skipped, 1 failed",
			want: "failed",
		},
		{
			name: "error count zero, timeout count above zero",
			line: "2026/10/21 03:32:10 dispatch: 0 error(s), 3 timeout",
			want: "timeout",
		},
		{
			name: "WARNING: anywhere in the line",
			line: lineWarn,
			want: "warning",
		},
		{
			name: "warning at the start of the message",
			line: "2026/10/21 03:36:00 warning: slow query",
			want: "warning",
		},
		{
			name: "warning mid-message in lower case",
			line: "2026/10/21 03:36:00 dolt: warning: slow query",
			want: "",
		},
		{
			name: "error at the start of the message",
			line: "2026/10/21 03:35:00 error: component cd crashed",
			want: "error",
		},
		{
			name: "a keyword with no count before it",
			line: "2026/10/21 03:37:00 jsonl_git_backup: push failed",
			want: "failed",
		},
		{
			name: "a zero inside a bead id is not a count",
			line: "2026/10/21 03:37:00 polecat gt-r0 refused",
			want: "refused",
		},
		{
			name: "a comma-grouped count above zero",
			line: "2026/10/21 03:37:00 dispatch: 1,000 refused",
			want: "refused",
		},
		{
			name: "a comma-grouped count of zero",
			line: "2026/10/21 03:37:00 dispatch: 0,000 refused",
			want: "",
		},
		{
			name: "warning inside a count above zero",
			line: "2026/10/21 03:37:00 patrol_scan: checked 7 polecat(s): 2 warning(s)",
			want: "warning",
		},
		{
			name: "warning inside a zero count",
			line: "2026/10/21 03:37:00 patrol_scan: checked 7 polecat(s): 0 warning(s)",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matchedOn(tc.line); got != tc.want {
				t.Errorf("matchedOn(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// TestJevLogtriageMatchedOnIsLogged: a zero-count summary is not read, so it
// asks nothing, and each logged group names the keyword that kept it.
func TestJevLogtriageMatchedOnIsLogged(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(
		"2026/10/21 03:32:10 landing_worker: gastown: pass: 1 landed, 0 repaired, 0 rejected, 0 skipped, 0 failed",
		lineMerge1, lineWarn)

	sum := fx.run()
	if sum.Matched != 2 || sum.Groups != 2 || len(fx.jev.requests) != 2 {
		t.Fatalf("summary = %+v with %d calls, want 2 matched in 2 groups asked", sum, len(fx.jev.requests))
	}
	byKey := map[string]Line{}
	for _, l := range fx.triage() {
		byKey[l.Key] = l
	}
	if got := byKey[keyMerge].MatchedOn; got != "failed" {
		t.Errorf("merge matched_on = %q, want failed", got)
	}
	if got := byKey[keyWarn].MatchedOn; got != "warning" {
		t.Errorf("warning matched_on = %q, want warning", got)
	}
}

// TestJevLogtriagePendingGroupWithoutMatchedOn: a group carried in a cursor
// written before matched_on existed still logs the keyword that kept its first
// line.
func TestJevLogtriagePendingGroupWithoutMatchedOn(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fx.pass.Run(ctx); err != nil {
		t.Fatalf("Run = %v, want the deadline to defer the group", err)
	}
	var cur Cursor
	if _, err := battery.ReadState(fx.pass.CursorPath, &cur); err != nil {
		t.Fatal(err)
	}
	if len(cur.Pending) != 1 {
		t.Fatalf("cursor carries %d pending groups, want 1", len(cur.Pending))
	}
	cur.Pending[0].MatchedOn = ""
	if err := battery.WriteState(fx.pass.CursorPath, cur); err != nil {
		t.Fatal(err)
	}

	fx.run()
	lines := fx.triage()
	if len(lines) != 1 || lines[0].Key != keyWarn {
		t.Fatalf("triage log = %+v, want the deferred warning group", lines)
	}
	if got := lines[0].MatchedOn; got != "warning" {
		t.Errorf("matched_on = %q, want warning", got)
	}
}
