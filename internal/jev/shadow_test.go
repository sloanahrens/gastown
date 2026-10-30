package jev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// shadowWaitGuard bounds waitShadows. A shadow call still running after it is a
// bug, and the test fails with a message rather than hanging.
const shadowWaitGuard = 10 * time.Second

// waitShadows waits for the background shadow calls the client started.
func waitShadows(t *testing.T, c *Client) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		c.shadows.Wait()
		close(done)
	}()
	select {
	case <-done:
	//testpolicy:allow no-sleep — the guard only bounds a shadow call that never returns; no test sleeps on it.
	case <-time.After(shadowWaitGuard):
		t.Fatalf("a shadow call did not finish within %v", shadowWaitGuard)
	}
}

// scriptedReply is one answer the scripted transport gives: a status and body,
// or a transport error.
type scriptedReply struct {
	status int
	body   string
	err    error
}

// scriptedRT answers the nth request with the nth reply and keeps every body.
// The shadow call runs in the background, so the transport locks.
type scriptedRT struct {
	mu      sync.Mutex
	replies []scriptedReply
	bodies  [][]byte
}

func (s *scriptedRT) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	s.bodies = append(s.bodies, raw)
	n := len(s.bodies) - 1
	if n >= len(s.replies) {
		return nil, fmt.Errorf("scriptedRT: request %d was not scripted", n+1)
	}
	rep := s.replies[n]
	if rep.err != nil {
		return nil, rep.err
	}
	if rep.status == 0 {
		rep.status = http.StatusOK
	}
	return httpReply(r, rep.status, rep.body), nil
}

// count returns how many requests the transport has seen.
func (s *scriptedRT) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// body returns the nth request body.
func (s *scriptedRT) body(n int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[n]
}

// alwaysAnswers answers every request with the same usable choice.
type alwaysAnswers struct{}

func (alwaysAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	return httpReply(r, http.StatusOK, replyChoice("returns", 1)), nil
}

// failSecondCall answers the original call and fails the shadow call, whose
// state has had the token scrubbed out of it.
type failSecondCall struct{}

func (failSecondCall) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(raw), testVendorKey) {
		return nil, fmt.Errorf("connection reset")
	}
	return httpReply(r, http.StatusOK, replyChoice("returns", 1)), nil
}

// httpReply builds the response a fake transport returns for r.
func httpReply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// replyChoice answers the one question q with choice, probabilities 1 and 0.
func replyChoice(choice string, inTokens int) string {
	probs := map[string]float64{"billing": 0, "returns": 0, "shipping": 0}
	probs[choice] = 1
	b, _ := json.Marshal(map[string]any{
		"model": Model,
		"answers": map[string]any{"q": map[string]any{
			"type": "choice", "choice": choice, "confidence": 1.0, "probabilities": probs,
		}},
		"usage": map[string]int{"input_tokens": inTokens, "output_tokens": 30},
	})
	return string(b)
}

// shadowFixture is a client with the shadow check on, a ledger and a check
// log in a temporary directory, and the transport the test scripts.
func shadowFixture(t *testing.T, rt http.RoundTripper, percent int, allows func() (bool, error)) (*Client, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "jev")
	c := newTestClient(t, rt,
		WithLedger(filepath.Join(dir, "usage.jsonl")),
		WithCaller("testcaller"),
		WithWarnings(io.Discard),
		WithShadowScrub(percent, allows),
	)
	return c, dir
}

// leakyState is a state that carries the token, with i making each one differ.
func leakyState(i int) map[string]string {
	return map[string]string{"log": fmt.Sprintf("git push with %s from %s/src/%d", testVendorKey, testHomeDir, i)}
}

func allowCap() (bool, error) { return true, nil }

// TestShadowScrubSamplesDeterministically: over fixture states the sampled share
// is within 1.2 to 2.8 percent, and a state is always or never sampled.
func TestShadowScrubSamplesDeterministically(t *testing.T) {
	t.Parallel()
	const states = 5000
	selected := 0
	for i := range states {
		raw, err := json.Marshal(map[string]string{"log": fmt.Sprintf("build %d went red", i)})
		require.NoError(t, err)
		sum := sha256.Sum256(raw)
		first := shadowSelected(sum, SamplePercent)
		assert.Equal(t, first, shadowSelected(sum, SamplePercent), "state %d is sampled the same way twice", i)
		if first {
			selected++
		}
	}
	share := 100 * float64(selected) / states
	assert.GreaterOrEqual(t, share, 1.2, "share %.2f%% of %d states", share, states)
	assert.LessOrEqual(t, share, 2.8, "share %.2f%% of %d states", share, states)
}

// TestShadowScrubLogsBothAnswers: a sampled state with a token gets a second
// call carrying the scrubbed state and the same questions, and one check line
// holds both answer sets and the rule counts, with no state text. A sampled
// state with nothing to scrub writes an unchanged line and sends no second call.
func TestShadowScrubLogsBothAnswers(t *testing.T) {
	t.Parallel()

	t.Run("token scrubbed", func(t *testing.T) {
		t.Parallel()
		rt := &scriptedRT{replies: []scriptedReply{
			{body: replyChoice("returns", 328)},
			{body: replyChoice("billing", 300)},
		}}
		c, dir := shadowFixture(t, rt, 100, allowCap)

		got, err := c.Choice(context.Background(), scrubState(), "Which team?", shoeCriteria)
		require.NoError(t, err)
		waitShadows(t, c)

		require.Equal(t, 2, rt.count(), "one call for the original, one shadow call")
		assert.Equal(t, "returns", got.Choice, "the original answer is returned as before")

		var first, second struct {
			State     map[string]string `json:"state"`
			Questions json.RawMessage   `json:"questions"`
		}
		require.NoError(t, json.Unmarshal(rt.body(0), &first))
		require.NoError(t, json.Unmarshal(rt.body(1), &second))
		assert.Equal(t, scrubLog, first.State["log"], "the original call sends the state as it is")
		assert.NotContains(t, second.State["log"], testVendorKey, "the shadow call sends the scrubbed state")
		assert.NotContains(t, second.State["log"], testHomeDir)
		assert.Equal(t, string(first.Questions), string(second.Questions), "the questions are the same")

		lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
		require.Len(t, lines, 1)
		line := lines[0]
		assert.Equal(t, outcomeChecked, line["outcome"])
		assert.Equal(t, "testcaller", line["caller"])
		assert.Equal(t, []any{"q"}, line["question_ids"])
		assert.Len(t, line["state_sha256"], 64)
		assert.Equal(t, map[string]any{
			"choice": map[string]any{"q": map[string]any{
				"choice":        "returns",
				"probabilities": map[string]any{"billing": float64(0), "returns": float64(1), "shipping": float64(0)},
				"confidence":    float64(1),
			}},
		}, line["original_answers"])
		assert.Equal(t, "billing", answerChoice(t, line["scrubbed_answers"]))
		assert.Equal(t, map[string]any{
			scrub.RuleVendorKey: float64(1),
			scrub.RuleHomePath:  float64(1),
		}, line["replacements"])
		assert.NotContains(t, line, "unchanged")

		text, err := os.ReadFile(filepath.Join(dir, scrubCheckFile))
		require.NoError(t, err)
		assert.NotContains(t, string(text), testVendorKey, "the check line never carries the token")
		assert.NotContains(t, string(text), testHomeDir, "the check line never carries the account name")
		assert.NotContains(t, string(text), "git push", "the check line never carries state text")

		ledger := ledgerLines(t, filepath.Join(dir, "usage.jsonl"))
		require.Len(t, ledger, 2)
		assert.Equal(t, "testcaller", ledger[0].Caller)
		assert.Equal(t, "testcaller-scrubcheck", ledger[1].Caller, "the shadow call is billed under its own caller")
		assert.Equal(t, 300, ledger[1].InputTokens)
	})

	t.Run("nothing to scrub", func(t *testing.T) {
		t.Parallel()
		rt := &scriptedRT{replies: []scriptedReply{{body: replyChoice("returns", 328)}}}
		c, dir := shadowFixture(t, rt, 100, allowCap)

		_, err := c.Choice(context.Background(), map[string]string{"title": "the gate stays red"}, "Which team?", shoeCriteria)
		require.NoError(t, err)
		waitShadows(t, c)

		assert.Equal(t, 1, rt.count(), "an unchanged state sends no second call")
		lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
		require.Len(t, lines, 1)
		assert.Equal(t, outcomeChecked, lines[0]["outcome"])
		assert.Equal(t, true, lines[0]["unchanged"])
		assert.NotContains(t, lines[0], "scrubbed_answers")
		assert.NotContains(t, lines[0], "replacements")
	})
}

// TestShadowScrubEachRequestLogsOneOutcome: every request writes exactly one
// line, a state the sample did not choose is unsampled, and the sampled ones
// add up: sampled = checked + failed + capped. Each outcome has its own value,
// so a failed check and a capped one never look like an unsampled state.
func TestShadowScrubEachRequestLogsOneOutcome(t *testing.T) {
	t.Parallel()
	const states, percent = 300, 10
	closed := func() (bool, error) { return false, nil }
	tests := []struct {
		name   string
		rt     http.RoundTripper
		allows func() (bool, error)
		want   string
	}{
		{name: "every second call answers", rt: alwaysAnswers{}, allows: allowCap, want: outcomeChecked},
		{name: "a spent cap sends no second call", rt: alwaysAnswers{}, allows: closed, want: outcomeCapped},
		{name: "a second call that fails", rt: failSecondCall{}, allows: allowCap, want: outcomeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, dir := shadowFixture(t, tt.rt, percent, tt.allows)
			for i := range states {
				_, err := c.Choice(context.Background(), leakyState(i), "Which team?", shoeCriteria)
				require.NoError(t, err, "the original call succeeds whatever the shadow does")
			}
			waitShadows(t, c)

			lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
			require.Len(t, lines, states, "one line per request")
			outcomes := map[string]string{}
			for _, line := range lines {
				sha, _ := line["state_sha256"].(string)
				outcome, _ := line["outcome"].(string)
				_, dup := outcomes[sha]
				require.False(t, dup, "a state has one line, not two")
				outcomes[sha] = outcome
			}

			sampled := 0
			for i := range states {
				raw, err := json.Marshal(leakyState(i))
				require.NoError(t, err)
				sum := sha256.Sum256(raw)
				outcome := outcomes[hex.EncodeToString(sum[:])]
				if shadowSelected(sum, percent) {
					sampled++
					assert.Equal(t, tt.want, outcome, "state %d", i)
				} else {
					assert.Equal(t, outcomeUnsampled, outcome, "state %d", i)
				}
			}
			require.Positive(t, sampled, "the fixture samples some states")

			counts := map[string]int{}
			for _, outcome := range outcomes {
				counts[outcome]++
			}
			assert.Equal(t, sampled, counts[outcomeChecked]+counts[outcomeFailed]+counts[outcomeCapped],
				"sampled = checked + failed + capped")
			assert.Equal(t, states-sampled, counts[outcomeUnsampled])
		})
	}
}

// TestShadowScrubFailureIsHarmless: a failing shadow call, or a cap that is
// closed, leaves the original answers and ledger line as they were. The sampled
// request writes one line that says what happened, with the class of the error
// and never its text.
func TestShadowScrubFailureIsHarmless(t *testing.T) {
	t.Parallel()
	closed := func() (bool, error) { return false, nil }
	failed := func() (bool, error) { return false, fmt.Errorf("the ledger is unreadable") }
	tests := []struct {
		name    string
		replies []scriptedReply
		allows  func() (bool, error)
		calls   int
		outcome string
		class   string
		leak    string
	}{
		{name: "server error", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {status: http.StatusInternalServerError, body: "boom"}}, allows: allowCap, calls: 2, outcome: outcomeFailed, class: "status", leak: "boom"},
		{name: "transport error", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {err: fmt.Errorf("connection reset")}}, allows: allowCap, calls: 2, outcome: outcomeFailed, class: "transport", leak: "connection reset"},
		{name: "timeout", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {err: context.DeadlineExceeded}}, allows: allowCap, calls: 2, outcome: outcomeFailed, class: "timeout"},
		{name: "cap closed", replies: []scriptedReply{{body: replyChoice("returns", 328)}}, allows: closed, calls: 1, outcome: outcomeCapped},
		{name: "cap unreadable", replies: []scriptedReply{{body: replyChoice("returns", 328)}}, allows: failed, calls: 1, outcome: outcomeFailed, class: "cap", leak: "the ledger is unreadable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := &scriptedRT{replies: tt.replies}
			c, dir := shadowFixture(t, rt, 100, tt.allows)

			got, err := c.Choice(context.Background(), scrubState(), "Which team?", shoeCriteria)
			require.NoError(t, err, "the original call succeeds whatever the shadow does")
			waitShadows(t, c)

			assert.Equal(t, "returns", got.Choice)
			assert.Equal(t, tt.calls, rt.count())

			lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
			require.Len(t, lines, 1, "the sampled request writes its one line")
			assert.Equal(t, tt.outcome, lines[0]["outcome"])
			if tt.class != "" {
				assert.Equal(t, tt.class, lines[0]["error_class"])
			} else {
				assert.NotContains(t, lines[0], "error_class")
			}
			assert.NotContains(t, lines[0], "scrubbed_answers")
			if tt.leak != "" {
				text, err := os.ReadFile(filepath.Join(dir, scrubCheckFile))
				require.NoError(t, err)
				assert.NotContains(t, string(text), tt.leak, "the line carries the error's class, not its text")
			}

			ledger := ledgerLines(t, filepath.Join(dir, "usage.jsonl"))
			require.Len(t, ledger, 1, "only the original call is billed")
			assert.Equal(t, "testcaller", ledger[0].Caller)
		})
	}
}

// TestShadowScrubLogsNoSecretInAnswers: a model that echoes a secret back in an
// answer does not put it in the check log, and the caller still gets the answer
// the model gave.
func TestShadowScrubLogsNoSecretInAnswers(t *testing.T) {
	t.Parallel()
	criteria := map[string]string{
		"billing":     "Charges, invoices, payment problems",
		testVendorKey: "An option the model echoes a token into",
	}
	b, err := json.Marshal(map[string]any{
		"model": Model,
		"answers": map[string]any{"q": map[string]any{
			"type": "choice", "choice": testVendorKey, "confidence": 1.0,
			"probabilities": map[string]float64{"billing": 0, testVendorKey: 1},
		}},
		"usage": map[string]int{"input_tokens": 100, "output_tokens": 10},
	})
	require.NoError(t, err)
	rt := &scriptedRT{replies: []scriptedReply{{body: string(b)}, {body: string(b)}}}
	c, dir := shadowFixture(t, rt, 100, allowCap)

	got, err := c.Choice(context.Background(), scrubState(), "Which team?", criteria)
	require.NoError(t, err)
	waitShadows(t, c)
	assert.Equal(t, testVendorKey, got.Choice, "the caller gets the answer the model gave")

	text, err := os.ReadFile(filepath.Join(dir, scrubCheckFile))
	require.NoError(t, err)
	assert.NotContains(t, string(text), testVendorKey, "no answer in the log holds the raw secret")
	lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
	require.Len(t, lines, 1)
	assert.Equal(t, outcomeChecked, lines[0]["outcome"])
}

// TestShadowScrubCapSaysOncePerWindow: the cap's refusals of one UTC day are
// said once, with their count, when the next day's first refusal arrives.
func TestShadowScrubCapSaysOncePerWindow(t *testing.T) {
	t.Parallel()
	var warnings bytes.Buffer
	c := &Client{warnings: &warnings}
	day := time.Date(2026, time.October, 30, 9, 0, 0, 0, time.UTC)
	for range 3 {
		c.noteCapped(day)
	}
	c.noteCapped(day.Add(time.Hour))
	assert.Empty(t, warnings.String(), "a day that is still going says nothing yet")

	c.noteCapped(day.Add(24 * time.Hour))
	want := "jev: scrub check capped 4 times on 2026-10-30: the daily cap was spent\n"
	assert.Equal(t, want, warnings.String())

	c.noteCapped(day.Add(24*time.Hour + time.Minute))
	assert.Equal(t, want, warnings.String(), "the day is said once")
}

// TestNewClientRefusesAShadowCheckItCannotHonor: a check that would sample a
// share that is not a whole part of 100, spend without a cap check, or log
// nowhere is a configuration error, not a client that quietly runs anyway.
func TestNewClientRefusesAShadowCheckItCannotHonor(t *testing.T) {
	t.Parallel()
	ledger := WithLedger(filepath.Join(t.TempDir(), "jev", "usage.jsonl"))
	tests := []struct {
		name string
		opts []Option
		want string
	}{
		{name: "a share that is not a whole part of 100", opts: []Option{ledger, WithShadowScrub(3, allowCap)}, want: "divides 100"},
		{name: "a zero share", opts: []Option{ledger, WithShadowScrub(0, allowCap)}, want: "divides 100"},
		{name: "no cap check", opts: []Option{ledger, WithShadowScrub(2, nil)}, want: "cap check"},
		{name: "no ledger beside which to log", opts: []Option{WithShadowScrub(2, allowCap)}, want: "WithLedger"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewClient(append([]Option{WithKey(testKey)}, tt.opts...)...)
			require.Error(t, err)
			assert.Nil(t, c)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// checkLines decodes the check log at path, one object per line.
func checkLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

// answerChoice returns the choice question q's answer in a logged answer set.
func answerChoice(t *testing.T, set any) string {
	t.Helper()
	m, ok := set.(map[string]any)
	require.True(t, ok, "an answer set, got %T", set)
	choices, ok := m["choice"].(map[string]any)
	require.True(t, ok, "a choice map in %v", m)
	q, ok := choices["q"].(map[string]any)
	require.True(t, ok, "question q in %v", choices)
	choice, _ := q["choice"].(string)
	return choice
}
