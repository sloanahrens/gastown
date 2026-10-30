package jev

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

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
	resp := &http.Response{
		StatusCode: rep.status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(rep.body)),
		Request:    r,
	}
	return resp, nil
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
		first := shadowSelected(sum, ShadowScrubPercent)
		assert.Equal(t, first, shadowSelected(sum, ShadowScrubPercent), "state %d is sampled the same way twice", i)
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
		c.shadows.Wait()

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
		c.shadows.Wait()

		assert.Equal(t, 1, rt.count(), "an unchanged state sends no second call")
		lines := checkLines(t, filepath.Join(dir, scrubCheckFile))
		require.Len(t, lines, 1)
		assert.Equal(t, true, lines[0]["unchanged"])
		assert.NotContains(t, lines[0], "scrubbed_answers")
		assert.NotContains(t, lines[0], "replacements")
	})
}

// TestShadowScrubFailureIsHarmless: a failing shadow call, or a cap that is
// closed, leaves the original answers and ledger line as they were and writes
// no check line.
func TestShadowScrubFailureIsHarmless(t *testing.T) {
	t.Parallel()
	closed := func() (bool, error) { return false, nil }
	failed := func() (bool, error) { return false, fmt.Errorf("the ledger is unreadable") }
	tests := []struct {
		name    string
		replies []scriptedReply
		allows  func() (bool, error)
		calls   int
	}{
		{name: "server error", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {status: http.StatusInternalServerError, body: "boom"}}, allows: allowCap, calls: 2},
		{name: "transport error", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {err: fmt.Errorf("connection reset")}}, allows: allowCap, calls: 2},
		{name: "timeout", replies: []scriptedReply{{body: replyChoice("returns", 328)}, {err: context.DeadlineExceeded}}, allows: allowCap, calls: 2},
		{name: "cap closed", replies: []scriptedReply{{body: replyChoice("returns", 328)}}, allows: closed, calls: 1},
		{name: "cap unreadable", replies: []scriptedReply{{body: replyChoice("returns", 328)}}, allows: failed, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt := &scriptedRT{replies: tt.replies}
			c, dir := shadowFixture(t, rt, 100, tt.allows)

			got, err := c.Choice(context.Background(), scrubState(), "Which team?", shoeCriteria)
			require.NoError(t, err, "the original call succeeds whatever the shadow does")
			c.shadows.Wait()

			assert.Equal(t, "returns", got.Choice)
			assert.Equal(t, tt.calls, rt.count())
			assert.NoFileExists(t, filepath.Join(dir, scrubCheckFile), "a failed or capped check writes no line")
			ledger := ledgerLines(t, filepath.Join(dir, "usage.jsonl"))
			require.Len(t, ledger, 1, "only the original call is billed")
			assert.Equal(t, "testcaller", ledger[0].Caller)
		})
	}
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
