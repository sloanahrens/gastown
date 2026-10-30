package jev

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestLines reads path and decodes its request lines.
func requestLines(t *testing.T, path string) []RequestRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []RequestRecord
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec RequestRecord
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		out = append(out, rec)
	}
	return out
}

// requestFields decodes one line into its raw JSON object, for the fields a
// typed decode would coerce.
func requestFields(t *testing.T, path string, index int) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	require.Greater(t, len(lines), index)
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[index]), &fields))
	return fields
}

// twoChoiceAnswers answers the two questions TestClientRecordsRequestProof
// asks, whatever their ids.
const twoChoiceAnswers = `{"model":"jev-1.13.0","answers":{
	"alpha":{"type":"choice","choice":"returns","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1},
	"beta":{"type":"choice","choice":"billing","probabilities":{"billing":1,"returns":0,"shipping":0},"confidence":1}},
	"usage":{"input_tokens":10,"output_tokens":2}}`

func TestClientRecordsRequestProof(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "jev")
	ledgerPath := filepath.Join(dir, "usage.jsonl")
	rs := &respond{body: twoChoiceAnswers, header: http.Header{"X-Request-Id": {"req-42"}}}
	c := newTestClient(t, rs, WithLedger(ledgerPath), WithCaller("proof caller"))

	qs := map[string]Question{
		"beta":  {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
		"alpha": {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
	}
	_, err := c.Ask(context.Background(), shoeState, qs)
	require.NoError(t, err)

	path := filepath.Join(dir, "requests.jsonl")
	lines := requestLines(t, path)
	require.Len(t, lines, 1)
	rec := lines[0]
	marshaledState, err := json.Marshal(shoeState)
	require.NoError(t, err)
	bodySum := sha256.Sum256(rs.raw)

	assert.Equal(t, "proof caller", rec.Caller)
	assert.Equal(t, []string{"alpha", "beta"}, rec.QuestionIDs, "the question ids are sorted")
	assert.Equal(t, "req-42", rec.RequestID)
	assert.Equal(t, len(marshaledState), rec.StateBytes)
	assert.Equal(t, len(rs.raw), rec.BodyBytes, "body_bytes is the posted body's length")
	assert.Equal(t, hex.EncodeToString(bodySum[:]), rec.BodySHA256, "the hash covers the bytes the server received")
	assert.Equal(t, float64(http.StatusOK), rec.Status)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the request log is private")
	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "the request log directory is private")
}

// cutTestState is a state a battery cut before sending it.
type cutTestState struct {
	Text          string `json:"text"`
	before, after int
	wasCut        bool
}

func (s cutTestState) Cut() (int, int, bool) { return s.before, s.after, s.wasCut }

func TestClientRecordsCutState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.jsonl")
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithRequests(path), WithCaller("cut caller"))

	state := cutTestState{Text: strings.Repeat("x", 40), before: 900, wasCut: true}
	marshaled, err := json.Marshal(state)
	require.NoError(t, err)
	state.after = len(marshaled)

	_, err = c.Choice(context.Background(), state, "Which team?", shoeCriteria)
	require.NoError(t, err)
	_, err = c.Choice(context.Background(), shoeState, "Which team?", shoeCriteria)
	require.NoError(t, err)

	lines := requestLines(t, path)
	require.Len(t, lines, 2)
	require.NotNil(t, lines[0].StateBytesBeforeCut)
	require.NotNil(t, lines[0].Cut)
	assert.Equal(t, len(marshaled), lines[0].StateBytes)
	assert.Equal(t, 900, *lines[0].StateBytesBeforeCut, "the size before the cut comes from the state")
	assert.True(t, *lines[0].Cut)

	plain := requestFields(t, path, 1)
	assert.NotContains(t, plain, "state_bytes_before_cut", "a state without the interface carries no before size")
	assert.NotContains(t, plain, "cut", "a state without the interface carries no cut flag")
}

// timeoutErr is a transport failure the client reads as a timeout, so the
// failed-call test needs no wall clock to reach one.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// failingBody is a response body whose every read fails.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
func (failingBody) Close() error             { return nil }

func TestClientRecordsFailedCalls(t *testing.T) {
	t.Parallel()
	const sentinelState = "SENTINEL-STATE-TEXT"
	const sentinelKey = "sk-sentinel-key-0123456789"
	const sentinelInstructions = "SENTINEL-INSTRUCTIONS"
	timeout := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, timeoutErr{} })

	tests := []struct {
		name   string
		rt     http.RoundTripper
		status any
	}{
		{"timeout", timeout, statusTimeout},
		{"rate limited", &respond{status: http.StatusTooManyRequests, body: `{"error":"slow down"}`}, float64(http.StatusTooManyRequests)},
		{"server error", &respond{status: http.StatusInternalServerError, body: "upstream exploded"}, float64(http.StatusInternalServerError)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "requests.jsonl")
			opts := []Option{WithRequests(path), WithCaller("failed caller"), WithKey(sentinelKey)}
			c := newTestClient(t, tt.rt, opts...)

			_, err := c.Ask(context.Background(), sentinelState, map[string]Question{
				questionID: {Type: typeChoice, Instructions: sentinelInstructions, Options: shoeCriteria},
			})
			require.Error(t, err)

			lines := requestLines(t, path)
			require.Len(t, lines, 1, "a failed call is still one line")
			assert.Equal(t, tt.status, lines[0].Status)
			assert.Equal(t, "failed caller", lines[0].Caller)

			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.NotContains(t, string(data), sentinelState, "the line holds no state text")
			assert.NotContains(t, string(data), sentinelInstructions, "the line holds no question text")
			assert.NotContains(t, string(data), sentinelKey, "the line holds no key")
		})
	}
}

func TestClientRecordsNothingForACallThatNeverLeaves(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithLedger(filepath.Join(dir, "usage.jsonl")))

	_, err := c.Ask(context.Background(), shoeState, nil)

	require.Error(t, err)
	assert.Zero(t, rs.callCnt, "no request left")
	assert.NoFileExists(t, filepath.Join(dir, "requests.jsonl"))
}

func TestClientRecordsNothingForARequestThatCannotBeBuilt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	// ":" is not a URL the client can build a request from, so the call stops
	// before any round trip.
	c := newTestClient(t, rs, WithRequests(path), WithBaseURL(":"))

	_, err := c.Ask(context.Background(), shoeState, map[string]Question{
		questionID: {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
	})

	require.Error(t, err)
	assert.Zero(t, rs.callCnt, "a request the client cannot build never leaves")
	assert.NoFileExists(t, path, "no line for a request that never left")
}

func TestClientRecordsABodyReadFailure(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"X-Request-Id": {"req-read"}},
			Body:       failingBody{},
			Request:    r,
		}, nil
	})
	c := newTestClient(t, rt, WithRequests(path), WithCaller("read caller"))

	_, err := c.Ask(context.Background(), shoeState, map[string]Question{
		questionID: {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
	})

	require.Error(t, err)
	lines := requestLines(t, path)
	require.Len(t, lines, 1)
	assert.Equal(t, statusBodyReadFailed, lines[0].Status, "a body that cannot be read is not a success")
	assert.Equal(t, http.StatusOK, lines[0].HTTPStatus, "the reply's status is kept beside the class")
	assert.Equal(t, "req-read", lines[0].RequestID)
}

func TestClientRecordsNothingForAStateThatWillNotEncode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithRequests(path))

	_, err := c.Ask(context.Background(), make(chan int), map[string]Question{
		questionID: {Type: typeChoice, Instructions: "Which team?", Options: shoeCriteria},
	})

	require.Error(t, err)
	assert.Zero(t, rs.callCnt, "a state that will not encode is not sent")
	assert.NoFileExists(t, path, "no line for a request that never left")
}

func TestClientRequestRecordFailureDoesNotFailTheCall(t *testing.T) {
	t.Parallel()
	// A regular file where the request log's directory should be makes every
	// write fail without touching permissions.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	var warnings strings.Builder
	c := newTestClient(t, &respond{body: readTestdata(t, "choice_response.json")},
		WithRequests(filepath.Join(blocker, "requests.jsonl")), WithWarnings(&warnings))

	got, err := c.Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)

	require.NoError(t, err, "a request log failure never fails the call")
	assert.Equal(t, "returns", got.Choice)
	assert.Contains(t, warnings.String(), "request log")
	assert.NotContains(t, warnings.String(), testKey)
}

func TestRequestsPath(t *testing.T) {
	t.Parallel()
	assert.Equal(t, filepath.Join("/town", ".runtime", "jev", "requests.jsonl"), RequestsPath("/town"))
}

func TestReadRequestsReportsAnUnreadableLog(t *testing.T) {
	t.Parallel()
	t.Run("opening", func(t *testing.T) {
		// A regular file where the log's directory should be fails the open.
		blocker := filepath.Join(t.TempDir(), "blocker")
		require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

		_, _, err := ReadRequests(filepath.Join(blocker, "requests.jsonl"), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "opening the request log")
	})
	t.Run("reading", func(t *testing.T) {
		// Opening a directory succeeds; reading it does not.
		_, _, err := ReadRequests(t.TempDir(), "", 20)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "reading the request log")
	})
}

// writeRequestLog writes lines to a fresh log and returns its path.
func writeRequestLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func TestReadRequestsCountsUnreadableLines(t *testing.T) {
	t.Parallel()
	path := writeRequestLog(t,
		`{"caller":"dispatch","body_sha256":"aaa"}`,
		`{not json`,
		``,
		`{"caller":"dispatch","body_sha256":"bbb"}`,
	)

	records, skipped, err := ReadRequests(path, "", 20)

	require.NoError(t, err)
	assert.Equal(t, 1, skipped, "the unreadable line is counted, not dropped in silence")
	require.Len(t, records, 2)
	assert.Equal(t, "aaa", records[0].BodySHA256)
	assert.Equal(t, "bbb", records[1].BodySHA256)
}

func TestReadRequestsRecentWindow(t *testing.T) {
	t.Parallel()
	lines := []string{
		`{"caller":"dispatch","body_sha256":"aaa"}`,
		`{"caller":"dispatch","body_sha256":"bbb"}`,
	}
	tests := []struct {
		name    string
		lines   []string
		recent  int
		want    []string
		skipped int
	}{
		{"an empty file is no records", []string{}, 20, nil, 0},
		{"a window wider than the log keeps every line", lines, 20, []string{"aaa", "bbb"}, 0},
		{"a window exactly the log's length keeps every line", lines, 2, []string{"aaa", "bbb"}, 0},
		{"a window one short drops the oldest line", lines, 1, []string{"bbb"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if len(tt.lines) == 0 {
				path = filepath.Join(t.TempDir(), "requests.jsonl")
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			} else {
				path = writeRequestLog(t, tt.lines...)
			}

			records, skipped, err := ReadRequests(path, "", tt.recent)

			require.NoError(t, err)
			assert.Equal(t, tt.skipped, skipped)
			var got []string
			for _, rec := range records {
				got = append(got, rec.BodySHA256)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
