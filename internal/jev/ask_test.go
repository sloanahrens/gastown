package jev

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	askQuestions = map[string]Question{
		"team": {
			Type:         "choice",
			Instructions: "Which team should handle this?",
			Options:      shoeCriteria,
		},
		"refund": {
			Type:         "noul",
			Instructions: "The customer is asking for a refund.",
			True:         "The message asks for money back",
			False:        "The message asks for an exchange or another outcome",
		},
		"urgency": {
			Type:         "score",
			Instructions: "How urgent is this request?",
			Levels: []string{
				"No time pressure",
				"Soon, but not today",
				"This week",
				"Today",
				"Immediately, and the customer is blocked",
			},
		},
	}

	choiceAsk = map[string]Question{
		questionID: {Type: "choice", Instructions: "q?", Options: shoeCriteria},
	}
	noulAsk = map[string]Question{
		questionID: {Type: "noul", Instructions: "q?"},
	}
	scoreAsk = map[string]Question{
		questionID: {Type: "score", Instructions: "q?", Levels: []string{"low", "mid", "high"}},
	}
)

func TestAsk_RequestShape(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "ask_response.json")}
	c := newTestClient(t, rs)

	_, err := c.Ask(context.Background(), shoeState, askQuestions)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, rs.req.Method)
	assert.Equal(t, testURL, rs.req.URL.String())
	assert.Equal(t, "Bearer "+testKey, rs.req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", rs.req.Header.Get("Content-Type"))
	assert.JSONEq(t, readTestdata(t, "ask_request.json"), string(rs.raw))
	assert.Equal(t, 1, rs.callCnt, "one Ask is one request")
}

func TestAsk_ParsesEveryAnswerType(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "ask_response.json")}
	c := newTestClient(t, rs)

	got, err := c.Ask(context.Background(), shoeState, askQuestions)
	require.NoError(t, err)

	assert.Equal(t, Model, got.Model)
	assert.Equal(t, Usage{InputTokens: 412, OutputTokens: 57}, got.Usage)
	assert.Equal(t, ChoiceAnswer{
		Choice:        "returns",
		Probabilities: map[string]float64{"billing": 0.05, "returns": 0.92, "shipping": 0.03},
		Confidence:    0.92,
	}, got.Choice["team"])
	assert.Equal(t, map[string]float64{"refund": 0.25}, got.Noul)
	assert.Equal(t, ScoreAnswer{
		Score:      1,
		Confidence: 0.8,
		Probabilities: map[string]float64{
			"0": 0.05, "1": 0.8, "2": 0.1, "3": 0.04, "4": 0.01,
		},
		Legend: map[string]string{
			"0": "No time pressure",
			"1": "Soon, but not today",
			"2": "This week",
			"3": "Today",
			"4": "Immediately, and the customer is blocked",
		},
	}, got.Score["urgency"])
}

func TestAsk_NoulCriteria(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		q        Question
		want     string
		wantOmit string
	}{
		{
			name:     "undescribed omits criteria",
			q:        Question{Type: "noul", Instructions: "It can be swapped."},
			wantOmit: `"criteria"`,
		},
		{
			name: "true description only",
			q:    Question{Type: "noul", Instructions: "It can be swapped.", True: "the size is in stock"},
			want: `"criteria":{"true":"the size is in stock"}`,
		},
		{
			name: "both descriptions",
			q: Question{
				Type:         "noul",
				Instructions: "It can be swapped.",
				True:         "the size is in stock",
				False:        "the size is gone",
			},
			want: `"criteria":{"false":"the size is gone","true":"the size is in stock"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := &respond{body: `{"answers":{"q":{"type":"noul","noul":0.5}}}`}
			c := newTestClient(t, rs)

			_, err := c.Ask(context.Background(), "s", map[string]Question{"q": tt.q})
			require.NoError(t, err)

			if tt.want != "" {
				assert.Contains(t, string(rs.raw), tt.want)
			}
			if tt.wantOmit != "" {
				assert.NotContains(t, string(rs.raw), tt.wantOmit)
			}
		})
	}
}

func TestAsk_RejectsQuestionsItCannotSend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		qs   map[string]Question
	}{
		{"no questions", nil},
		{"unknown type", map[string]Question{"q": {Type: "ranking", Instructions: "q?"}}},
		{"choice without options", map[string]Question{"q": {Type: "choice", Instructions: "q?"}}},
		{"score with one level", map[string]Question{"q": {Type: "score", Instructions: "q?", Levels: []string{"only"}}}},
		{"score with eleven levels", map[string]Question{"q": {Type: "score", Instructions: "q?", Levels: make([]string, 11)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := &respond{}
			c := newTestClient(t, rs)

			_, err := c.Ask(context.Background(), "s", tt.qs)

			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrBadResponse, "a caller mistake is not a bad response")
			assert.Zero(t, rs.callCnt, "no request leaves for a question the API cannot carry")
		})
	}
}

func TestAsk_BadResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		qs   map[string]Question
		body string
	}{
		{"not json", choiceAsk, `<html>`},
		{"missing answer id", choiceAsk, `{"answers":{}}`},
		{"wrong answer type", choiceAsk, `{"answers":{"q":{"type":"noul","noul":0.9}}}`},
		{"choice not asked", choiceAsk, `{"answers":{"q":{"type":"choice","choice":"refunds","probabilities":{"billing":0,"returns":1,"shipping":0},"confidence":1}}}`},
		{"choice probabilities do not sum to 1", choiceAsk, `{"answers":{"q":{"type":"choice","choice":"returns","probabilities":{"billing":0.5,"returns":0.8,"shipping":0.5},"confidence":1}}}`},
		{"noul above 1", noulAsk, `{"answers":{"q":{"type":"noul","noul":1.2}}}`},
		{"noul below 0", noulAsk, `{"answers":{"q":{"type":"noul","noul":-0.1}}}`},
		{"noul without a value", noulAsk, `{"answers":{"q":{"type":"noul"}}}`},
		{"score above the top level", scoreAsk, `{"answers":{"q":{"type":"score","score":3,"confidence":0.5,"probabilities":{"low":0.5,"mid":0.5,"high":0}}}}`},
		{"score below the bottom level", scoreAsk, `{"answers":{"q":{"type":"score","score":-1,"confidence":0.5,"probabilities":{"low":0.5,"mid":0.5,"high":0}}}}`},
		{"score without a value", scoreAsk, `{"answers":{"q":{"type":"score","confidence":0.5,"probabilities":{"low":0.5,"mid":0.5,"high":0}}}}`},
		{"score probabilities do not sum to 1", scoreAsk, `{"answers":{"q":{"type":"score","score":1,"confidence":0.5,"probabilities":{"low":0.2,"mid":0.3,"high":0.4}}}}`},
		{"score confidence outside 0 to 1", scoreAsk, `{"answers":{"q":{"type":"score","score":1,"confidence":1.5,"probabilities":{"low":0.5,"mid":0.5,"high":0}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, &respond{body: tt.body})

			_, err := c.Ask(context.Background(), "s", tt.qs)

			require.ErrorIs(t, err, ErrBadResponse)
		})
	}
}

func TestAsk_WithLedgerWritesOneLinePerCall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := newTestClient(t, &respond{body: readTestdata(t, "ask_response.json")},
		WithLedger(path), WithCaller("ask test"))

	got, err := c.Ask(context.Background(), shoeState, askQuestions)
	require.NoError(t, err)

	lines := ledgerLines(t, path)
	require.Len(t, lines, 1, "one Ask is one ledger line")
	assert.Equal(t, "ask test", lines[0].Caller)
	assert.Equal(t, got.Usage.InputTokens, lines[0].InputTokens)
	assert.Equal(t, got.Usage.OutputTokens, lines[0].OutputTokens)
}

func TestChoice_MatchesOneQuestionAsk(t *testing.T) {
	t.Parallel()
	viaChoice := &respond{body: readTestdata(t, "choice_response.json")}
	_, err := newTestClient(t, viaChoice).Choice(context.Background(), shoeState, "Which team should handle this?", shoeCriteria)
	require.NoError(t, err)

	viaAsk := &respond{body: readTestdata(t, "choice_response.json")}
	_, err = newTestClient(t, viaAsk).Ask(context.Background(), shoeState, map[string]Question{
		questionID: {Type: "choice", Instructions: "Which team should handle this?", Options: shoeCriteria},
	})
	require.NoError(t, err)

	assert.JSONEq(t, string(viaChoice.raw), string(viaAsk.raw))
}
