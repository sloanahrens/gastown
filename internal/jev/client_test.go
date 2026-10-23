package jev

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testURL = "http://jev.test/v1/systemone"
	testKey = "sk-test-0123456789"
)

// roundTripFunc serves each request in-process, so the tests exercise the
// client's real request building and response handling without a socket.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// respond answers every request with status, headers and body, and keeps the
// request and its raw body for assertions.
type respond struct {
	status  int
	header  http.Header
	body    string
	req     *http.Request
	raw     []byte
	callCnt int
}

func (rs *respond) RoundTrip(r *http.Request) (*http.Response, error) {
	rs.callCnt++
	rs.req = r
	rs.raw, _ = io.ReadAll(r.Body)
	rec := httptest.NewRecorder()
	for k, v := range rs.header {
		rec.Header()[k] = v
	}
	if rs.status == 0 {
		rs.status = http.StatusOK
	}
	rec.WriteHeader(rs.status)
	_, _ = rec.WriteString(rs.body)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

func newTestClient(t *testing.T, rt http.RoundTripper, opts ...Option) *Client {
	t.Helper()
	opts = append([]Option{
		WithKey(testKey),
		WithBaseURL(testURL),
		WithHTTPClient(&http.Client{Transport: rt}),
	}, opts...)
	c, err := NewClient(opts...)
	require.NoError(t, err)
	return c
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}

var shoeCriteria = map[string]string{
	"billing":  "Charges, invoices, payment problems",
	"returns":  "Exchanges, wrong or damaged items",
	"shipping": "Delivery status, delays, lost packages",
}

const shoeState = "My running shoes arrived in the wrong size. Can I swap them for a size 10?"

func TestChoice_ParsesFixture(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs)

	got, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)
	require.NoError(t, err)

	assert.Equal(t, "returns", got.Choice)
	assert.Equal(t, 1.0, got.Confidence)
	assert.Equal(t, map[string]float64{"shipping": 0, "returns": 1, "billing": 0}, got.Probabilities)
	assert.Equal(t, Model, got.Model)
	assert.Equal(t, Usage{InputTokens: 328, OutputTokens: 34}, got.Usage)
}

func TestChoice_RequestShape(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs)

	_, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, rs.req.Method)
	assert.Equal(t, testURL, rs.req.URL.String())
	assert.Equal(t, "Bearer "+testKey, rs.req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", rs.req.Header.Get("Content-Type"))
	assert.JSONEq(t, readTestdata(t, "choice_request.json"), string(rs.raw))
}

func TestChoice_StructuredState(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs)

	state := map[string]any{"title": "wrong size", "n": 2}
	_, err := c.Choice(context.Background(), state, "q?", shoeCriteria)
	require.NoError(t, err)
	assert.Contains(t, string(rs.raw), `"state":{"n":2,"title":"wrong size"}`)
}

func TestChoice_NoOptions(t *testing.T) {
	t.Parallel()
	rs := &respond{}
	c := newTestClient(t, rs)

	_, err := c.Choice(context.Background(), "s", "q?", nil)
	require.Error(t, err)
	assert.Zero(t, rs.callCnt, "no request leaves for an empty option set")
}

func TestChoice_Timeout(t *testing.T) {
	t.Parallel()
	calls := 0
	block := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c := newTestClient(t, block, WithTimeout(20*time.Millisecond))

	start := time.Now()
	_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

	require.ErrorIs(t, err, ErrTimeout)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, calls, "a timeout is not retried")
}

func TestChoice_CallerDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	block := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c := newTestClient(t, block)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Choice(ctx, "s", "q?", shoeCriteria)

	require.ErrorIs(t, err, ErrTimeout)
}

func TestChoice_CallerCancelIsNotTimeout(t *testing.T) {
	t.Parallel()
	block := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c := newTestClient(t, block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Choice(ctx, "s", "q?", shoeCriteria)

	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrTimeout)
}

func TestChoice_RateLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
	}{
		{"no header", nil, 0},
		{"retry-after seconds", http.Header{"Retry-After": {"3"}}, 3 * time.Second},
		{"retry-after-ms wins", http.Header{"Retry-After": {"3"}, "Retry-After-Ms": {"250"}}, 250 * time.Millisecond},
		{"unparseable date", http.Header{"Retry-After": {"Wed, 21 Oct 2026 07:28:00 GMT"}}, 0},
		{"negative seconds", http.Header{"Retry-After": {"-3"}}, 0},
		{"infinite seconds", http.Header{"Retry-After": {"+Inf"}}, 0},
		{"out-of-range seconds", http.Header{"Retry-After": {"1e300"}}, 0},
		{"infinite ms", http.Header{"Retry-After-Ms": {"Inf"}}, 0},
		{"out-of-range ms falls back", http.Header{"Retry-After-Ms": {"1e300"}, "Retry-After": {"2"}}, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := &respond{status: http.StatusTooManyRequests, header: tt.header, body: `{"error":"slow down"}`}
			c := newTestClient(t, rs)

			_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

			require.ErrorIs(t, err, ErrRateLimited)
			var rl *RateLimitError
			require.ErrorAs(t, err, &rl)
			assert.Equal(t, tt.want, rl.RetryAfter)
			assert.Equal(t, 1, rs.callCnt, "a 429 is not retried")
		})
	}
}

func TestChoice_HTTPErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		body    string
		wantIs  error
		wantMsg string
	}{
		{"unauthorized", 401, `{"error":"bad key"}`, ErrUnauthorized, ""},
		{"overloaded", 529, `{"message":"overloaded"}`, nil, "overloaded"},
		{"validation", 422, `{"detail":"questions.q.criteria required"}`, nil, "questions.q.criteria required"},
		{"plain text", 500, "upstream exploded", nil, "upstream exploded"},
		{"echoed key", 500, `{"error":"bad header Bearer ` + testKey + `"}`, nil, "bad header Bearer [redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := &respond{status: tt.status, body: tt.body}
			c := newTestClient(t, rs)

			_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

			require.Error(t, err)
			assert.NotContains(t, err.Error(), testKey)
			if tt.wantIs != nil {
				require.ErrorIs(t, err, tt.wantIs)
				return
			}
			var se *StatusError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, tt.status, se.Status)
			assert.Equal(t, tt.wantMsg, se.Message)
			assert.Equal(t, 1, rs.callCnt)
		})
	}
}

func TestChoice_LongErrorBodyIsCut(t *testing.T) {
	t.Parallel()
	rs := &respond{status: 500, body: strings.Repeat("é", 500)}
	c := newTestClient(t, rs)

	_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

	var se *StatusError
	require.ErrorAs(t, err, &se)
	assert.LessOrEqual(t, len(se.Message), maxMessage)
	assert.True(t, strings.HasPrefix(se.Message, "éé"))
}

func TestChoice_BadResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"not json", `<html>`},
		{"no answers", `{"model":"jev-1.13.0","answers":{},"usage":{}}`},
		{"wrong answer type", `{"answers":{"q":{"type":"noul","noul":0.9}}}`},
		{"choice not asked", `{"answers":{"q":{"type":"choice","choice":"refunds","probabilities":{},"confidence":1}}}`},
		{"empty choice", `{"answers":{"q":{"type":"choice","choice":"","probabilities":{},"confidence":1}}}`},
		{"confidence above 1", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1.5}}}`},
		{"confidence below 0", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":-0.5}}}`},
		{"option without probability", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"returns":1,"shipping":0},"confidence":1}}}`},
		{"probability for option not asked", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0,"refunds":0},"confidence":1}}}`},
		{"probabilities do not sum to 1", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.5,"returns":0.5,"shipping":0.5},"confidence":1}}}`},
		{"probability outside 0 to 1", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":1.5,"returns":-0.5,"shipping":0},"confidence":1}}}`},
		{"choice is not most probable", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.1,"returns":0.8,"shipping":0.1},"confidence":1}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, &respond{body: tt.body})

			_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

			require.ErrorIs(t, err, ErrBadResponse)
		})
	}
}

// TestChoice_RoundedProbabilitiesParse gets answers whose probabilities sum to
// within Jev's two-decimal rounding, and keeps the probabilities as Jev sent
// them.
func TestChoice_RoundedProbabilitiesParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		body          string
		choice        string
		probabilities map[string]float64
	}{
		{"each of three rounded to a third sums to 0.99", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.33,"returns":0.33,"shipping":0.33},"confidence":1}}}`,
			"returns", map[string]float64{"billing": 0.33, "returns": 0.33, "shipping": 0.33}},
		{"chosen option 0.01 below the highest is a tie", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.49,"returns":0.5,"shipping":0.01},"confidence":1}}}`,
			"billing", map[string]float64{"billing": 0.49, "returns": 0.5, "shipping": 0.01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, &respond{body: tt.body})

			got, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

			require.NoError(t, err)
			assert.Equal(t, tt.choice, got.Choice)
			assert.Equal(t, tt.probabilities, got.Probabilities)
		})
	}
}

// TestChoice_RoundingEdges checks the rounding tolerance at its edges: a sum
// one step inside the bound parses, a step outside it does not, and the bound
// is 0.005 per option, so two options allow 0.01 where three allow 0.015.
func TestChoice_RoundingEdges(t *testing.T) {
	t.Parallel()
	pair := map[string]string{"billing": "Charges, invoices, payment problems", "returns": "Exchanges, wrong or damaged items"}
	tests := []struct {
		name     string
		body     string
		criteria map[string]string
		wantErr  bool
	}{
		{"three options summing to 0.985 is inside the 0.015 bound", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.33,"returns":0.33,"shipping":0.325},"confidence":1}}}`,
			shoeCriteria, false},
		{"three options summing to 1.015 is inside the 0.015 bound", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.34,"returns":0.34,"shipping":0.335},"confidence":1}}}`,
			shoeCriteria, false},
		{"three options summing to 0.98 is outside the 0.015 bound", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.33,"returns":0.33,"shipping":0.32},"confidence":1}}}`,
			shoeCriteria, true},
		{"three options summing to 1.02 is outside the 0.015 bound", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.34,"returns":0.34,"shipping":0.34},"confidence":1}}}`,
			shoeCriteria, true},
		{"two options summing to 0.99 is inside the 0.01 bound", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.49,"returns":0.5},"confidence":1}}}`,
			pair, false},
		{"two options summing to 0.985 is outside the 0.01 bound", `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.49,"returns":0.495},"confidence":1}}}`,
			pair, true},
		{"chosen option 0.011 below the highest is not a tie", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.489,"returns":0.5,"shipping":0.011},"confidence":1}}}`,
			shoeCriteria, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, &respond{body: tt.body})

			_, err := c.Choice(context.Background(), "s", "q?", tt.criteria)

			if tt.wantErr {
				require.ErrorIs(t, err, ErrBadResponse)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestAsk_RoundedScoreProbabilitiesParse gets a score answer whose level
// probabilities sum to within Jev's two-decimal rounding of one per level.
func TestAsk_RoundedScoreProbabilitiesParse(t *testing.T) {
	t.Parallel()
	score := Question{Type: typeScore, Levels: []string{"low", "mid", "high"}}
	c := newTestClient(t, &respond{body: `{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":0.33,"mid":0.33,"high":0.33},"legend":{"low":"a","mid":"b","high":"c"}}}}`})

	got, err := c.Ask(context.Background(), shoeState, map[string]Question{questionID: score})

	require.NoError(t, err)
	assert.Equal(t, map[string]float64{"low": 0.33, "mid": 0.33, "high": 0.33}, got.Score[questionID].Probabilities)
}

func TestAskRecordsUsageOnBadAnswer(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	body := `{"answers":{"q":{"type":"choice","choice":"refunds","probabilities":{},"confidence":1}},"usage":{"input_tokens":120,"output_tokens":7}}`
	c := newTestClient(t, &respond{body: body}, WithLedger(path), WithCaller("ask test"))

	_, err := c.Ask(context.Background(), shoeState, map[string]Question{
		questionID: {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
	})

	require.ErrorIs(t, err, ErrBadResponse)
	lines := ledgerLines(t, path)
	require.Len(t, lines, 1, "a billed call whose answer is bad is still one ledger line")
	assert.Equal(t, "ask test", lines[0].Caller)
	assert.Equal(t, 120, lines[0].InputTokens)
	assert.Equal(t, 7, lines[0].OutputTokens)
}

func TestChoice_BadResponseMessagesAreUnchanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"no answers", `{"answers":{}}`, "no choice answer"},
		{"wrong answer type", `{"answers":{"q":{"type":"noul","noul":0.9}}}`, "no choice answer"},
		{"choice not asked", `{"answers":{"q":{"type":"choice","choice":"refunds","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1}}}`, `choice "refunds" is not an option asked`},
		{"not most probable", `{"answers":{"q":{"type":"choice","choice":"billing","probabilities":{"billing":0.1,"returns":0.8,"shipping":0.1},"confidence":1}}}`, `choice "billing" is not the most probable option`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, &respond{body: tt.body})

			_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

			require.EqualError(t, err, ErrBadResponse.Error()+": "+tt.want)
		})
	}
}

func TestChoice_TransportErrorScrubsKey(t *testing.T) {
	t.Parallel()
	boom := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed for header Authorization: Bearer " + testKey)
	})
	c := newTestClient(t, boom)

	_, err := c.Choice(context.Background(), "s", "q?", shoeCriteria)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	assert.Contains(t, err.Error(), "[redacted]")
	assert.NotErrorIs(t, err, ErrTimeout)
}

func TestNewClient_NilHTTPClientKeepsDefault(t *testing.T) {
	t.Parallel()
	c, err := NewClient(WithKey(testKey), WithHTTPClient(nil))

	require.NoError(t, err)
	assert.Equal(t, http.DefaultClient, c.httpClient)
}

func TestWithTimeout_NonPositiveKeepsDefault(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, &respond{}, WithTimeout(0))
	assert.Equal(t, DefaultTimeout, c.timeout)
}

func TestNewClient_ReadsKeyFile(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/gt/jev.env"
	writeKeyFile(t, path, "TYPESAFE_API_KEY="+testKey+"\n", 0o600)

	c, err := NewClient(WithKeyPaths(path))

	require.NoError(t, err)
	assert.Equal(t, testKey, c.key)
}

func TestNewClient_MissingKeyNamesFilesNotKey(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/gt/jev.env"

	_, err := NewClient(WithKeyPaths(path))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "jev.env")
}
