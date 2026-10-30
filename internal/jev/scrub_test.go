package jev

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev/scrub"
)

// The synthetic secrets a scrubbing test's state carries: a vendor key and a
// home path, the two shapes the default rules replace here.
const (
	testVendorKey = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	testHomeDir   = "/home/alice"
)

// scrubLog is a state field holding both test secrets the way a battery's log
// excerpt would.
const scrubLog = "git push with " + testVendorKey + " from " + testHomeDir + "/src/gastown"

// scrubState returns a state holding both test secrets beside a field with
// neither.
func scrubState() map[string]string {
	return map[string]string{"log": scrubLog, "title": "the gate stays red"}
}

// TestClientScrubsState: a state holding a token and a home path reaches the
// server with both replaced, the questions unchanged, and the answer parsed as
// before. Removing the ScrubJSON call from Ask turns this red.
func TestClientScrubsState(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithScrub(scrub.Default()))
	instructions := "Read the log at " + testHomeDir + "/src/gastown and pick a team."

	got, err := c.Choice(context.Background(), scrubState(), instructions, shoeCriteria)
	require.NoError(t, err)

	assert.Equal(t, "returns", got.Choice, "the answer parses as it does without a scrubber")
	assert.Equal(t, Usage{InputTokens: 328, OutputTokens: 34}, got.Usage)

	var posted struct {
		State     map[string]string `json:"state"`
		Questions map[string]struct {
			Instructions string `json:"instructions"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(rs.raw, &posted))

	wantLog, _ := scrub.Default().Scrub(scrubLog)
	assert.Equal(t, wantLog, posted.State["log"])
	assert.NotContains(t, posted.State["log"], testVendorKey)
	assert.NotContains(t, posted.State["log"], testHomeDir)
	assert.Equal(t, "the gate stays red", posted.State["title"])
	assert.Equal(t, instructions, posted.Questions[questionID].Instructions,
		"the questions are the caller's own text and go out unchanged")
}

// TestClientWithoutScrubIsUnchanged: without WithScrub the client posts the
// bytes recorded in ask_request_body.json, which is what it sent before the
// option existed.
func TestClientWithoutScrubIsUnchanged(t *testing.T) {
	t.Parallel()
	rs := &respond{body: readTestdata(t, "ask_response.json")}
	c := newTestClient(t, rs)

	_, err := c.Ask(context.Background(), shoeState, askQuestions)
	require.NoError(t, err)

	assert.Equal(t, readTestdata(t, "ask_request_body.json"), string(rs.raw))
}

// TestClientScrubRecordsCountsNotText: the request line says the state was
// scrubbed, counts each rule that fired, and carries none of the text they
// replaced.
func TestClientScrubRecordsCountsNotText(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "jev")
	rs := &respond{body: readTestdata(t, "choice_response.json")}
	c := newTestClient(t, rs, WithLedger(filepath.Join(dir, "usage.jsonl")), WithScrub(scrub.Default()))

	_, err := c.Choice(context.Background(), scrubState(), "Which team?", shoeCriteria)
	require.NoError(t, err)

	path := filepath.Join(dir, "requests.jsonl")
	fields := requestFields(t, path, 0)
	assert.Equal(t, true, fields["scrubbed"])
	assert.Equal(t, map[string]any{
		scrub.RuleVendorKey: float64(1),
		scrub.RuleHomePath:  float64(1),
	}, fields["replacements"])

	line, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(line), testVendorKey, "the line never carries the token")
	assert.NotContains(t, string(line), testHomeDir, "the line never carries the account name")
}
