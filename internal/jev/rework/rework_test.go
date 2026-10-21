package rework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/land"
)

// jevReply answers both questions about the fixture refusal.
const jevReply = `{
  "model": "jev-1.13.0",
  "answers": {
    "reworked_to_land": {"type": "noul", "noul": 0.82},
    "cause": {"type": "choice", "choice": "tests_missing", "confidence": 0.7,
      "probabilities": {"tests_missing": 0.7, "scope_creep": 0.05, "logic_error": 0.1,
        "wrong_file": 0.05, "docs_stale": 0.02, "convention": 0.03, "unclear": 0.05}}
  },
  "usage": {"input_tokens": 900, "output_tokens": 120}
}`

// fakeJev is a transport that records each request body and answers with body
// and status, so the test drives the real Jev client without a port.
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

// fakeReviews lists the review records a test hands the pass.
type fakeReviews struct {
	recs []land.ReviewRecord
	err  error
}

func (f fakeReviews) Since(time.Time) ([]land.ReviewRecord, error) { return f.recs, f.err }

// fakeRigs serves one bead's notes and one numstat diff per bead.
type fakeRigs struct {
	notes map[string]string
	// notesErr is keyed by bead.
	notesErr map[string]error
	files    map[string]string
	filesErr error
	// diffs records the (rig, target, head) each Numstat was called with.
	diffs [][3]string
}

func (f *fakeRigs) Notes(_, bead string) (string, error) {
	if err := f.notesErr[bead]; err != nil {
		return "", err
	}
	notes, ok := f.notes[bead]
	if !ok {
		return "", beads.ErrNotFound
	}
	return notes, nil
}

func (f *fakeRigs) Numstat(_ context.Context, rig, target, head string) (string, error) {
	f.diffs = append(f.diffs, [3]string{rig, target, head})
	if f.filesErr != nil {
		return "", f.filesErr
	}
	return f.files[rig], nil
}

const (
	fixtureBead = "gt-abc"
	fixtureHead = "abc123def456"
)

// fixtureFindings is what om said about the fixture submission.
var fixtureFindings = []land.Finding{
	{ID: "f1", Severity: "major", Path: "internal/daemon/jev_rework.go", Line: 42, Title: "the patrol is never triggered from the heartbeat"},
	{ID: "f2", Severity: "minor", Path: "internal/jev/rework/rework.go", Line: 7, Title: "the doc comment does not say the pass is opt-in"},
}

// fixtureNotes is the work bead's notes after the refusal: the MERGE REJECTION
// block the landing wrote for attempt 1.
func fixtureNotes(t *testing.T) string {
	t.Helper()
	return "Some earlier note.\n\n" + land.FormatRejectionNote(land.RejectionNote{
		Attempt:  1,
		Kind:     string(land.RejectReview),
		Reason:   "om requested changes (score 0.60, 2 finding(s))",
		Branch:   "polecat/angharad/gt-abc",
		Target:   "main",
		MR:       fixtureBead,
		Head:     fixtureHead,
		Findings: fixtureFindings,
		Receipt:  &land.Receipt{Score: 0.6},
	})
}

// fixtureFail is the review log line the landing wrote for that refusal.
func fixtureFail() land.ReviewRecord {
	return land.ReviewRecord{
		BeadID: fixtureBead, Rig: "gastown", Attempt: 1, Score: 0.6,
		Verdict: land.VerdictRequestChanges, Rejected: true, Findings: 2,
		At: time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC),
	}
}

const fixtureNumstat = "24\t3\tinternal/jev/rework/rework.go\n2\t0\tinternal/daemon/jev_rework.go\n"

func fixtureRigs(t *testing.T) *fakeRigs {
	t.Helper()
	return &fakeRigs{
		notes: map[string]string{fixtureBead: fixtureNotes(t)},
		files: map[string]string{"gastown": fixtureNumstat},
	}
}

// newPass is a pass over a fresh log with a fake Jev that answers jevReply,
// and a cursor set in the past so the fixtures are pending.
func newPass(t *testing.T, f *fakeJev, rigs *fakeRigs, recs ...land.ReviewRecord) *Pass {
	t.Helper()
	dir := t.TempDir()
	p := &Pass{
		Asker:      newJevClient(t, f),
		Reviews:    fakeReviews{recs: recs},
		Rigs:       rigs,
		LogPath:    LogPath(dir),
		CursorPath: CursorPath(dir),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC) },
	}
	if err := battery.WriteCursor(p.CursorPath, battery.Cursor{Time: time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return p
}

// readLines decodes every line of the rework log at path.
func readLines(t *testing.T, path string) []Line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Line
	for _, raw := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if raw == "" {
			continue
		}
		var l Line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

// A fixture FAIL produces one line with both answers, and the state carries
// the findings and the diff stat.
func TestRunLogsOneLineWithBothAnswers(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	rigs := fixtureRigs(t)
	p := newPass(t, f, rigs, fixtureFail())

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Asked != 1 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v, want one asked and none skipped", sum)
	}

	lines := readLines(t, p.LogPath)
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1", len(lines))
	}
	l := lines[0]
	if l.Rig != "gastown" || l.Bead != fixtureBead || l.Attempt != 1 || l.Head != fixtureHead {
		t.Errorf("line names %s/%s attempt %d head %q, want gastown/%s attempt 1 head %q",
			l.Rig, l.Bead, l.Attempt, l.Head, fixtureBead, fixtureHead)
	}
	if !l.Refused.Equal(fixtureFail().At) {
		t.Errorf("line refused at %s, want the review's own %s", l.Refused, fixtureFail().At)
	}
	if l.Verdict != land.VerdictRequestChanges || l.Score != 0.6 {
		t.Errorf("line verdict %q at %.2f, want request_changes at 0.60", l.Verdict, l.Score)
	}
	if l.QuestionVersion != Version || l.Model != "jev-1.13.0" {
		t.Errorf("line version v%d %q, want v%d jev-1.13.0", l.QuestionVersion, l.Model, Version)
	}
	rework, ok := l.Answers[QReworkedToLand]
	if !ok || rework.Noul == nil || *rework.Noul != 0.82 {
		t.Errorf("reworked_to_land = %+v, want 0.82", rework)
	}
	if cause := l.Answers[QCause]; cause.Choice != "tests_missing" {
		t.Errorf("cause = %q, want tests_missing", cause.Choice)
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
	if !strings.Contains(sent.State.Findings, "major internal/daemon/jev_rework.go:42 — the patrol is never triggered") ||
		!strings.Contains(sent.State.Findings, "minor internal/jev/rework/rework.go:7") {
		t.Errorf("findings = %q, want both findings with their severities", sent.State.Findings)
	}
	if len(sent.State.Files) != 2 || sent.State.Files[0].Path != "internal/jev/rework/rework.go" || sent.State.Files[0].Added != 24 {
		t.Errorf("files = %+v, want the fixture's two files", sent.State.Files)
	}
	if len(rigs.diffs) != 1 || rigs.diffs[0] != [3]string{"gastown", "main", fixtureHead} {
		t.Errorf("diffed %v, want main...%s in gastown", rigs.diffs, fixtureHead)
	}
}

// A second pass over the same review log asks nothing: the cursor moved past
// the refusal it logged.
func TestRunAsksEachFailOnce(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, fixtureRigs(t), fixtureFail())

	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if sum.Asked != 0 || sum.CapReached || sum.Started {
		t.Errorf("second pass = %+v, want nothing asked", sum)
	}
	if len(f.requests) != 1 {
		t.Errorf("made %d Jev calls over two passes, want 1", len(f.requests))
	}
	if lines := readLines(t, p.LogPath); len(lines) != 1 {
		t.Errorf("logged %d lines over two passes, want 1", len(lines))
	}
}

// The first pass only sets the cursor: the town's past refusals are never
// billed.
func TestFirstPassStartsTheCursor(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	dir := t.TempDir()
	p := &Pass{
		Asker:      newJevClient(t, f),
		Reviews:    fakeReviews{recs: []land.ReviewRecord{fixtureFail()}},
		Rigs:       fixtureRigs(t),
		LogPath:    LogPath(dir),
		CursorPath: CursorPath(dir),
		Allows:     func() (bool, error) { return true, nil },
	}
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.Started || sum.Asked != 0 {
		t.Errorf("first pass = %+v, want started with nothing asked", sum)
	}
	if len(f.requests) != 0 {
		t.Errorf("the first pass made %d Jev calls, want 0", len(f.requests))
	}
}

// A Jev outage writes no line and leaves the cursor where it was, so the next
// pass asks again.
func TestJevOutageLogsNothing(t *testing.T) {
	t.Parallel()
	f := &fakeJev{status: http.StatusInternalServerError, body: `{"error":"down"}`}
	p := newPass(t, f, fixtureRigs(t), fixtureFail())

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run returned no error for a Jev outage")
	}
	if _, err := os.Stat(p.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Jev outage wrote %s", p.LogPath)
	}

	// The next pass, with Jev back, asks about the same refusal.
	f.status, f.body = 0, jevReply
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if sum.Asked != 1 {
		t.Errorf("the pass after the outage asked %d refusals, want 1", sum.Asked)
	}
}

// A cap already reached asks nothing and writes nothing.
func TestCapReachedLogsNothing(t *testing.T) {
	t.Parallel()
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, fixtureRigs(t), fixtureFail())
	p.Allows = func() (bool, error) { return false, nil }

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.CapReached || sum.Asked != 0 {
		t.Errorf("summary = %+v, want the cap reached with nothing asked", sum)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls under a reached cap, want 0", len(f.requests))
	}
	if _, err := os.Stat(p.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a pass under a reached cap wrote %s", p.LogPath)
	}
}

// A review the landing did not refuse is not a FAIL: an approval, a skipped
// review and an errored one get no line and no call.
func TestOnlyRefusedReviewsAreAsked(t *testing.T) {
	t.Parallel()
	approved := fixtureFail()
	approved.Verdict, approved.Rejected = land.VerdictApprove, false
	skipped := fixtureFail()
	skipped.Verdict, skipped.Rejected = land.VerdictSkipped, false
	errored := fixtureFail()
	errored.Verdict, errored.Rejected = land.VerdictErrorPrefix+"timeout", false

	f := &fakeJev{body: jevReply}
	p := newPass(t, f, fixtureRigs(t), approved, skipped, errored)
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Asked != 0 || sum.Skipped != 0 {
		t.Errorf("summary = %+v, want nothing asked", sum)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d Jev calls for reviews that were not refusals, want 0", len(f.requests))
	}
}

// An approval whose findings refused the landing is a FAIL like
// request_changes: the landing records it with the flag and an approve
// verdict (gt-vu18r).
func TestApprovalRefusedOnFindingsIsAsked(t *testing.T) {
	t.Parallel()
	rec := fixtureFail()
	rec.Verdict, rec.Rejected, rec.Score = land.VerdictApprove, true, 0.9

	f := &fakeJev{body: jevReply}
	p := newPass(t, f, fixtureRigs(t), rec)
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Asked != 1 {
		t.Fatalf("asked %d refusals, want 1", sum.Asked)
	}
	if lines := readLines(t, p.LogPath); len(lines) != 1 || lines[0].Verdict != land.VerdictApprove {
		t.Errorf("lines = %+v, want one naming the approve verdict", lines)
	}
}

// A bead whose notes hold no rejection block for the attempt is skipped, and
// the pass moves past it rather than holding up the refusals behind it.
func TestMissingRejectionBlockIsSkipped(t *testing.T) {
	t.Parallel()
	rigs := fixtureRigs(t)
	rigs.notes[fixtureBead] = "no rejection here"
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, rigs, fixtureFail())

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Skipped != 1 || sum.Asked != 0 {
		t.Errorf("summary = %+v, want one skipped", sum)
	}
	if _, err := os.Stat(p.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a skipped refusal wrote %s", p.LogPath)
	}
}

// A bead that does not exist is skipped, not an error.
func TestMissingBeadIsSkipped(t *testing.T) {
	t.Parallel()
	rigs := &fakeRigs{notes: map[string]string{}}
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, rigs, fixtureFail())

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Skipped != 1 {
		t.Errorf("summary = %+v, want one skipped", sum)
	}
}

// A Dolt outage reading the bead stops the pass with the error, so the next
// pass asks about the refusal again.
func TestBeadReadErrorStopsThePass(t *testing.T) {
	t.Parallel()
	rigs := fixtureRigs(t)
	rigs.notesErr = map[string]error{fixtureBead: errors.New("dolt: connection refused")}
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, rigs, fixtureFail())

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("Run returned no error for a bead read that is not a missing bead")
	}
	if len(f.requests) != 0 {
		t.Errorf("asked Jev %d times with no state to ask about", len(f.requests))
	}
}

// A diff that cannot be read leaves the files out of the state and the
// question is still asked: the findings are the verdict's own.
func TestUnreadableDiffStillAsks(t *testing.T) {
	t.Parallel()
	rigs := fixtureRigs(t)
	rigs.filesErr = errors.New("fatal: bad revision")
	f := &fakeJev{body: jevReply}
	var warned []string
	p := newPass(t, f, rigs, fixtureFail())
	p.Warnf = func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sum.Asked != 1 {
		t.Fatalf("asked %d refusals, want 1", sum.Asked)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "bad revision") {
		t.Errorf("warnings = %v, want one naming the diff error", warned)
	}
	var sent struct {
		State state `json:"state"`
	}
	if err := json.Unmarshal(f.requests[0], &sent); err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(sent.State.Files) != 0 {
		t.Errorf("files = %+v, want none", sent.State.Files)
	}
	if sent.State.Findings == "" {
		t.Error("the findings were dropped with the diff")
	}
}

// The findings cap keeps a runaway review from filling the state: the excerpt
// is cut at maxFindingsRunes.
func TestFindingsAreCapped(t *testing.T) {
	t.Parallel()
	var findings []land.Finding
	for range 40 {
		findings = append(findings, land.Finding{Severity: "minor", Path: "internal/x.go", Line: 1,
			Title: strings.Repeat("a verbose finding. ", 20)})
	}
	rigs := fixtureRigs(t)
	rigs.notes[fixtureBead] = land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: string(land.RejectReview), Target: "main", MR: fixtureBead,
		Head: fixtureHead, Findings: findings,
	})
	f := &fakeJev{body: jevReply}
	p := newPass(t, f, rigs, fixtureFail())
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sent struct {
		State state `json:"state"`
	}
	if err := json.Unmarshal(f.requests[0], &sent); err != nil {
		t.Fatalf("request: %v", err)
	}
	if n := len([]rune(sent.State.Findings)); n > maxFindingsRunes+1 {
		t.Errorf("findings are %d runes, want at most %d", n, maxFindingsRunes+1)
	}
}

// The cause options are the same, in the same order, on every call: the
// question is part of what the log's answers mean.
func TestCauseOptionsAreFixedAndOrdered(t *testing.T) {
	t.Parallel()
	want := []string{"tests_missing", "scope_creep", "logic_error", "wrong_file", "docs_stale", "convention", "unclear"}
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
	p := newPass(t, f, fixtureRigs(t), fixtureFail(), fixtureFail())
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.requests) != 2 {
		t.Fatalf("made %d Jev calls for two refusals, want 2", len(f.requests))
	}
	if !bytes.Equal(f.requests[0], f.requests[1]) {
		t.Error("two refusals of one fixture sent different requests: the question is not fixed")
	}
}
