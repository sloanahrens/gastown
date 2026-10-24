package progress

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev"
)

// askResponse is what a Jev endpoint returns for the five questions. Its
// probabilities sum to one, as the client checks.
const askResponse = `{
  "model": "jev-1.13.0",
  "answers": {
    "making_progress":       {"type": "noul", "noul": 0.9},
    "looping":               {"type": "noul", "noul": 0.1},
    "blocked_on_environment": {"type": "noul", "noul": 0.2},
    "drifted_from_bead":     {"type": "noul", "noul": 0.05},
    "remaining": {
      "type": "score",
      "score": 2,
      "confidence": 0.8,
      "legend": {"0": "barely started", "1": "halfway", "2": "nearly done", "3": "done"},
      "probabilities": {"0": 0.05, "1": 0.15, "2": 0.7, "3": 0.1}
    }
  },
  "usage": {"input_tokens": 400, "output_tokens": 50}
}`

// cannedJev answers every request with resp and keeps the request body it was
// sent. It stands in for the Jev endpoint, so no test opens a socket.
type cannedJev struct {
	status int
	resp   string
	req    []byte
}

func (c *cannedJev) RoundTrip(r *http.Request) (*http.Response, error) {
	c.req, _ = io.ReadAll(r.Body)
	return &http.Response{
		StatusCode: c.status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.resp)),
		Request:    r,
	}, nil
}

// cannedClient is the production Jev client pointed at a canned endpoint.
func cannedClient(t *testing.T, rt *cannedJev) *jev.Client {
	t.Helper()
	client, err := jev.NewClient(
		jev.WithHTTPClient(&http.Client{Transport: rt}),
		jev.WithBaseURL("https://jev.test/v1/systemone"),
		jev.WithKey("test-key"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The pass over the production client, against a canned endpoint: one nudge is
// one request, and its line holds the five answers.
func TestJevProgressAskOverHTTPLogsFiveAnswers(t *testing.T) {
	t.Parallel()
	rt := &cannedJev{status: http.StatusOK, resp: askResponse}
	client := cannedClient(t, rt)
	town := t.TempDir()
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("working")))
	q := &Queue{}
	q.Push(nudgeFor(path))
	pass := newPass(t, town, client, q)

	if _, err := pass.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{QMakingProgress, QLooping, QBlockedOnEnvironment, QDriftedFromBead, QRemaining, "bead_title"} {
		if !strings.Contains(string(rt.req), id) {
			t.Errorf("request does not carry %q", id)
		}
	}
	lines := readLog(t, LogPath(town))
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}
	if lines[0].Model != "jev-1.13.0" || len(lines[0].Answers) != 5 {
		t.Errorf("line = model %q, %d answers", lines[0].Model, len(lines[0].Answers))
	}
	if a := lines[0].Answers[QRemaining]; a.Score == nil || *a.Score != 2 {
		t.Errorf("remaining = %+v", a)
	}
}

// A Jev call that fails logs its nudge with null answers, and the log holds
// no error text.
func TestJevProgressAskFailureLogsReasonOnly(t *testing.T) {
	t.Parallel()
	client := cannedClient(t, &cannedJev{status: http.StatusInternalServerError, resp: `{"error":"secret detail"}`})
	town := t.TempDir()
	path := writeTranscript(t, town, line("assistant", "2026-10-21T00:25:00Z", textBlock("working")))
	q := &Queue{}
	q.Push(nudgeFor(path))

	sum, err := newPass(t, town, client, q).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Unanswered != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	raw, err := os.ReadFile(LogPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret detail") {
		t.Errorf("the log holds the response body: %s", raw)
	}
	lines := readLog(t, LogPath(town))
	if len(lines) != 1 || lines[0].Reason != ReasonAskFailed || lines[0].Answers != nil {
		t.Errorf("log = %+v", lines)
	}
}

// Every path the pass reads must sit under the town root.
func TestJevProgressTranscriptsStayInTheTown(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "gt")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, ".claude-town", "x.jsonl"), true},
		{filepath.Join(root, "gastown", "x.jsonl"), true},
		{filepath.Join(root, "..", "x.jsonl"), false},
		{filepath.Join(root, "..", "gt-other", "x.jsonl"), false},
		{filepath.Join(t.TempDir(), "x.jsonl"), false},
	}
	for _, c := range cases {
		if got := within(root, c.path); got != c.want {
			t.Errorf("within(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// The log and the Caller name are the ones the scorecard and the usage ledger
// read.
func TestJevProgressLogPathAndCaller(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := filepath.Join(town, ".runtime", "jev", "progress.jsonl")
	if got := LogPath(town); got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
	if Caller != "jev-progress" {
		t.Errorf("Caller = %q", Caller)
	}
}
