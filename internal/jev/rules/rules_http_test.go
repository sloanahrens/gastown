package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev"
)

// httpJev is an in-process Jev. It calls a session that pushed to main a main
// push, and any other session none. It is a RoundTripper, so no socket opens.
type httpJev struct{}

func (httpJev) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		State struct {
			Excerpt string `json:"excerpt"`
		} `json:"state"`
	}
	_ = json.Unmarshal(body, &req)

	noul, choice := 0.05, KindNone
	if strings.Contains(req.State.Excerpt, "git push origin main") {
		noul, choice = 0.95, KindMainPush
	}
	probs := map[string]float64{}
	for k := range Questions()[QRuleKind].Options {
		probs[k] = 0.02
	}
	probs[choice] = 1 - 0.02*5
	out, _ := json.Marshal(map[string]any{
		"model": "jev-test",
		"answers": map[string]any{
			QBrokeStandingRule: map[string]any{"type": "noul", "noul": noul},
			QRuleKind: map[string]any{
				"type": "choice", "choice": choice, "probabilities": probs, "confidence": 0.8,
			},
		},
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 2},
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(out)),
		Request:    r,
	}, nil
}

// TestRulesPassAgainstJevHTTP runs a pass through the real Jev client: a session
// that pushed to main gets a line naming main_push, and a session that did not
// gets a line naming none. Each line's choice is read by name.
func TestRulesPassAgainstJevHTTP(t *testing.T) {
	t.Parallel()
	client, err := jev.NewClient(
		jev.WithBaseURL("http://jev.test"), jev.WithKey("test-key"),
		jev.WithHTTPClient(&http.Client{Transport: httpJev{}}),
		jev.WithLedger(filepath.Join(t.TempDir(), "usage.jsonl")), jev.WithCaller(Caller),
	)
	if err != nil {
		t.Fatal(err)
	}
	town := t.TempDir()
	push := writeTranscript(t, town, "push.jsonl", pushTranscript)
	clean := writeTranscript(t, town, "clean.jsonl", cleanTranscript)

	p := newPass(t, town, client, push)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-push", fixedNow.Add(-2*time.Minute))}}
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Latest = func(string) (string, error) { return clean, nil }
	p.Lister = fixedLister{dones: []Done{submitted("gt-clean", fixedNow.Add(-time.Minute))}}
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	lines := parseLines(t, logLines(t, p.LogPath))
	if len(lines) != 2 || lines[0].Bead != "gt-push" || lines[1].Bead != "gt-clean" {
		t.Fatalf("log = %+v, want gt-push then gt-clean", lines)
	}
	if got := lines[0].Answers[QRuleKind].Choice; got != KindMainPush {
		t.Errorf("push choice = %q, want %q", got, KindMainPush)
	}
	if got := lines[1].Answers[QRuleKind].Choice; got != KindNone {
		t.Errorf("clean choice = %q, want %q", got, KindNone)
	}
	if lines[0].Model != "jev-test" || *lines[0].Answers[QBrokeStandingRule].Noul != 0.95 {
		t.Errorf("push line = %+v, want model jev-test and broke 0.95", lines[0])
	}
}

// TestRulesJevOutOfReachStopsWithCursorBefore checks an outage costs no
// submission: the pass stops, and the cursor stays before the one it could not
// ask, so the next pass asks it.
func TestRulesJevOutOfReachStopsWithCursorBefore(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &fakeAsker{}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{
		submitted("gt-a", fixedNow.Add(-3*time.Minute)),
		submitted("gt-b", fixedNow.Add(-2*time.Minute)),
	}}

	asker.err = errors.New("connection refused")
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("pass with Jev out of reach returned no error")
	}
	if n := len(logLines(t, p.LogPath)); n != 0 {
		t.Fatalf("log has %d lines after an outage, want none", n)
	}

	asker.err = nil
	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Asked != 2 {
		t.Errorf("after the outage asked %d, want both submissions", sum.Asked)
	}
}

// TestRulesBadResponseGetsReasonLine checks a Jev answer that does not answer
// as asked is logged with a reason and null answers, and the pass moves on.
func TestRulesBadResponseGetsReasonLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &fakeAsker{err: jev.ErrBadResponse}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-bad", fixedNow.Add(-time.Minute))}}

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unanswered != 1 || sum.Asked != 0 {
		t.Errorf("unanswered %d, asked %d; want 1 and 0", sum.Unanswered, sum.Asked)
	}
	l := parseLines(t, logLines(t, p.LogPath))[0]
	if l.Answers != nil || l.Reason != ReasonBadResponse {
		t.Errorf("line = %+v, want null answers and reason %q", l, ReasonBadResponse)
	}
}

// TestRulesNoWorktreeGetsReasonLine checks a submission whose polecat has no
// worktree is logged with the no_transcript reason, and Jev is not asked.
func TestRulesNoWorktreeGetsReasonLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &fakeAsker{}
	p := newPass(t, town, asker, transcript)
	p.Locate = func(string, string) string { return "" }
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-gone", fixedNow.Add(-time.Minute))}}

	if _, err := p.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	l := parseLines(t, logLines(t, p.LogPath))[0]
	if l.Reason != ReasonNoTranscript {
		t.Errorf("reason = %q, want %q", l.Reason, ReasonNoTranscript)
	}
	if asker.calls != 0 {
		t.Errorf("Jev was asked %d time(s) for a session with no worktree", asker.calls)
	}
}

// TestRulesCapStopsWithCursorBefore checks a spent cap stops the pass before
// the next submission, and leaves that submission for the next pass.
func TestRulesCapStopsWithCursorBefore(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	transcript := writeTranscript(t, town, "session.jsonl", pushTranscript)
	asker := &fakeAsker{}
	p := newPass(t, town, asker, transcript)
	startPass(t, p)
	p.Lister = fixedLister{dones: []Done{submitted("gt-a", fixedNow.Add(-2*time.Minute)), submitted("gt-b", fixedNow.Add(-time.Minute))}}
	calls := 0
	p.Allows = func() (bool, error) { calls++; return calls == 1, nil }

	sum, err := p.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sum.CapReached || sum.Asked != 1 {
		t.Errorf("cap reached %v, asked %d; want true and 1", sum.CapReached, sum.Asked)
	}
	if asker.calls != 1 {
		t.Errorf("Jev asked %d time(s), want 1", asker.calls)
	}
}
