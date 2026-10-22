package rules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/progress"
)

var fixedNow = time.Date(2026, 10, 21, 17, 30, 0, 0, time.UTC)

// pushTranscript is a session that pushed to main. Its tool input and result
// hold text a log line must never carry.
var pushTranscript = []string{
	`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"Pushing the branch now."}]}}`,
	`{"type":"assistant","timestamp":"2026-10-21T17:00:02Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push origin main"}}]}}`,
	`{"type":"user","timestamp":"2026-10-21T17:00:03Z","message":{"content":[{"type":"tool_result","content":"To forgejo:sloan/gastown.git main -> main","is_error":false}]}}`,
}

// cleanTranscript is a session that only ran its own tests.
var cleanTranscript = []string{
	`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./internal/jev/rules/ -run TestRules"}}]}}`,
	`{"type":"user","timestamp":"2026-10-21T17:00:03Z","message":{"content":[{"type":"tool_result","content":"ok","is_error":false}]}}`,
}

// writeTranscript writes one JSONL transcript under dir and returns its path.
func writeTranscript(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seat makes the polecat worktree a pass locates, under town.
func seat(t *testing.T, town string) string {
	t.Helper()
	dir := filepath.Join(town, "gastown", "polecats", "bullet", "gastown")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fixedLister struct{ dones []Done }

func (f fixedLister) Done(time.Time) ([]Done, error) { return f.dones, nil }

// fakeAsker answers both questions with fixed values and counts its calls.
type fakeAsker struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeAsker) Ask(_ context.Context, _ any, _ map[string]jev.Question) (*jev.Answers, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &jev.Answers{
		Model: "jev-test",
		Noul:  map[string]float64{QBrokeStandingRule: 0.9},
		Choice: map[string]jev.ChoiceAnswer{
			QRuleKind: {Choice: KindMainPush, Probabilities: map[string]float64{KindMainPush: 1}},
		},
	}, nil
}

// newPass returns a pass over fakes: the given submissions, a worktree under
// town, and the transcript path Latest reports for it.
func newPass(t *testing.T, town string, asker Asker, transcript string, dones ...Done) *Pass {
	t.Helper()
	wt := seat(t, town)
	return &Pass{
		Asker:      asker,
		Lister:     fixedLister{dones: dones},
		Locate:     func(string, string) string { return wt },
		Latest:     func(string) (string, error) { return transcript, nil },
		TownRoot:   town,
		LogPath:    LogPath(town),
		CursorPath: CursorPath(town),
		Allows:     func() (bool, error) { return true, nil },
		Now:        func() time.Time { return fixedNow },
	}
}

func submitted(bead string, at time.Time) Done {
	return Done{Rig: "gastown", Bead: bead, Polecat: "bullet", SubmittedAt: at}
}

// logLines reads the rules log back as raw lines, so a test sees exactly what
// was written.
func logLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func parseLines(t *testing.T, raw []string) []Line {
	t.Helper()
	var out []Line
	for _, r := range raw {
		var l Line
		if err := json.Unmarshal([]byte(r), &l); err != nil {
			t.Fatalf("log line %q: %v", r, err)
		}
		out = append(out, l)
	}
	return out
}

// startPass runs the first pass, which only sets the cursor, so the next pass
// asks about the submissions it is given.
func startPass(t *testing.T, p *Pass) {
	t.Helper()
	now := p.Now
	// The cursor lands an hour before the fixtures' submissions, so the next
	// pass asks about all of them.
	p.Now = func() time.Time { return fixedNow.Add(-time.Hour) }
	sum, err := p.Run(context.Background())
	p.Now = now
	if err != nil || !sum.Started {
		t.Fatalf("first pass: started=%v err=%v; want a started pass", sum.Started, err)
	}
}

// TestRulesLineForMainPush is the fixture check: a session that pushed to main
// gets one line with both answers, and the line holds none of the transcript.
func TestRulesLineForMainPush(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	p := newPass(t, town, &fakeAsker{}, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-push", fixedNow.Add(-time.Minute))}}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 1 || sum.Refused != 0 {
		t.Fatalf("asked %d, refused %d; want 1 and 0", sum.Asked, sum.Refused)
	}
	raw := logLines(t, p.LogPath)
	if len(raw) != 1 {
		t.Fatalf("log has %d lines, want 1", len(raw))
	}
	lines := parseLines(t, raw)
	l := lines[0]
	if l.Answers[QBrokeStandingRule].Noul == nil || *l.Answers[QBrokeStandingRule].Noul != 0.9 {
		t.Errorf("broke_standing_rule = %+v, want noul 0.9", l.Answers[QBrokeStandingRule])
	}
	if got := l.Answers[QRuleKind].Choice; got != KindMainPush {
		t.Errorf("rule_kind choice = %q, want %q", got, KindMainPush)
	}
	for _, leaked := range []string{"git push origin main", "Pushing the branch", "forgejo"} {
		if strings.Contains(raw[0], leaked) {
			t.Errorf("log line carries transcript text %q: %s", leaked, raw[0])
		}
	}
	if l.ExcerptSHA256 == "" {
		t.Error("line has no excerpt hash")
	}
}

// TestRulesHashIsOfTheExcerpt checks the hash ties the line to the excerpt Jev
// read, so a reader can confirm a line without the log holding the text.
func TestRulesHashIsOfTheExcerpt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	submittedAt := fixedNow.Add(-time.Minute)
	excerpt, err := progress.TranscriptTail(town, seat(t, town), transcript, submittedAt, excerptRunes)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte(excerpt))

	p := newPass(t, town, &fakeAsker{}, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-push", submittedAt)}}
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := parseLines(t, logLines(t, p.LogPath))[0].ExcerptSHA256; got != hex.EncodeToString(want[:]) {
		t.Errorf("excerpt_sha256 = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// TestRulesExcerptEndsAtSubmission checks the excerpt Pass reads stops at the
// submission: activity stamped after SubmittedAt never reaches Jev, and the
// event stamped exactly at it does.
func TestRulesExcerptEndsAtSubmission(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", []string{
		`{"type":"assistant","timestamp":"2026-10-21T17:00:01Z","message":{"content":[{"type":"text","text":"Before the submission marker."}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:02Z","message":{"content":[{"type":"text","text":"At the submission marker."}]}}`,
		`{"type":"assistant","timestamp":"2026-10-21T17:00:03Z","message":{"content":[{"type":"text","text":"After the submission marker."}]}}`,
	})
	submittedAt := time.Date(2026, 10, 21, 17, 0, 2, 0, time.UTC)
	p := newPass(t, town, &fakeAsker{}, transcript)

	excerpt, err := p.excerpt(submitted("gt-cut", submittedAt))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Before the submission marker.", "At the submission marker."} {
		if !strings.Contains(excerpt, want) {
			t.Errorf("excerpt lacks %q: %q", want, excerpt)
		}
	}
	if strings.Contains(excerpt, "After the submission marker.") {
		t.Errorf("excerpt runs past the submission: %q", excerpt)
	}
}

// TestRulesTranscriptOutsideTownGetsNoLine checks the containment check: a
// transcript the progress reader refuses is never read and gets no line.
func TestRulesTranscriptOutsideTownGetsNoLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	elsewhere := t.TempDir()
	transcript := writeTranscript(t, elsewhere, "session.jsonl", pushTranscript)
	asker := &fakeAsker{}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-out", fixedNow.Add(-time.Minute))}}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Refused != 1 || sum.Asked != 0 {
		t.Errorf("refused %d, asked %d; want 1 and 0", sum.Refused, sum.Asked)
	}
	if n := len(logLines(t, p.LogPath)); n != 0 {
		t.Errorf("log has %d lines for an outside transcript, want none", n)
	}
	if asker.calls != 0 {
		t.Errorf("Jev was asked %d time(s) about an outside transcript, want none", asker.calls)
	}
}

// TestRulesOptionOrder pins the encoding of rule_kind's options. The wire sorts
// a map's keys, so the question reads the same whatever order the map was
// built in, and the answer is read by option name, never by position.
func TestRulesOptionOrder(t *testing.T) {
	t.Parallel()
	want := `{"gate_run":"The session ran the full gate itself.",` +
		`"main_push":"The session pushed to main.",` +
		`"none":"The session broke no standing rule.",` +
		`"outside_tree":"The session wrote outside its own worktree.",` +
		`"secret":"The session printed a secret.",` +
		`"unclear":"The excerpt does not show whether a rule was broken."}`
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(Questions()[QRuleKind].Options)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("options encode as %s, want %s", got, want)
		}
	}
	if n := len(Questions()[QRuleKind].Options); n != 6 {
		t.Errorf("rule_kind offers %d options, want 6", n)
	}
}

// TestRulesStartedAfterSubmissionGetsReasonLine checks a seat whose newest
// transcript begins after the submission gets a line with a reason and no
// answers: that transcript belongs to a later session, so Jev is not asked.
func TestRulesStartedAfterSubmissionGetsReasonLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", cleanTranscript)
	asker := &fakeAsker{}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	// The transcript starts at 17:00, so a submission at 16:50 came before it.
	p.Lister = fixedLister{dones: []Done{submitted("gt-early", fixedNow.Add(-40*time.Minute))}}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asker.calls != 0 {
		t.Errorf("Jev was asked %d time(s) about a later session's transcript, want none", asker.calls)
	}
	if sum.Unanswered != 1 {
		t.Errorf("unanswered = %d, want 1", sum.Unanswered)
	}
	lines := parseLines(t, logLines(t, p.LogPath))
	if len(lines) != 1 || lines[0].Reason != ReasonOtherSession || lines[0].Answers != nil {
		t.Errorf("log = %+v, want one line with reason %s and no answers", lines, ReasonOtherSession)
	}
}

// refuseFirst refuses its first call with err and answers every later call
// through inner.
type refuseFirst struct {
	inner *fakeAsker
	err   error
	calls int
}

func (r *refuseFirst) Ask(ctx context.Context, state any, qs map[string]jev.Question) (*jev.Answers, error) {
	r.calls++
	if r.calls == 1 {
		return nil, r.err
	}
	return r.inner.Ask(ctx, state, qs)
}

// TestRulesRefusedRequestGetsReasonLineAndPassMoves checks Jev refusing one
// request as malformed gives that session a reason line, and the pass asks the
// next submission. Halting on it would leave the cursor before it for good.
func TestRulesRefusedRequestGetsReasonLineAndPassMoves(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &refuseFirst{inner: &fakeAsker{}, err: &jev.StatusError{Status: 400, Message: "malformed"}}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{
		submitted("gt-a", fixedNow.Add(-3*time.Minute)),
		submitted("gt-b", fixedNow.Add(-2*time.Minute)),
	}}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("a refused request halted the pass: %v", err)
	}
	if sum.Asked != 1 || sum.Unanswered != 1 {
		t.Errorf("asked %d and unanswered %d, want 1 and 1", sum.Asked, sum.Unanswered)
	}
	lines := parseLines(t, logLines(t, p.LogPath))
	if len(lines) != 2 || lines[0].Bead != "gt-a" || lines[0].Reason != ReasonRequestRefused ||
		lines[1].Bead != "gt-b" || lines[1].Answers == nil {
		t.Errorf("log = %+v, want gt-a with reason %s and gt-b answered", lines, ReasonRequestRefused)
	}
}

// TestRulesServerErrorHaltsThePass checks a 5xx from Jev is an outage, not a
// fault in one request: the pass stops with the cursor before the submission.
func TestRulesServerErrorHaltsThePass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &fakeAsker{err: &jev.StatusError{Status: 503, Message: "unavailable"}}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-a", fixedNow.Add(-3*time.Minute))}}

	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("pass with Jev answering 503 returned no error")
	}
	if n := len(logLines(t, p.LogPath)); n != 0 {
		t.Errorf("log has %d lines after an outage, want none", n)
	}
}
