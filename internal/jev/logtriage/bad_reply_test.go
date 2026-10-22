package logtriage

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/jev"
)

// TestJevLogtriageBadReplyLogsTheCheck: a reply with one bad answer logs the
// check that failed, the question it failed on, the number the check read and
// the request's size. The line holds no text the reply or the error carried.
func TestJevLogtriageBadReplyLogsTheCheck(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.jev.body = strings.Replace(jevReply, `"score": 1,`, `"score": 7,`, 1)
	fx.logLines(lineWarn)

	fx.run()
	lines := fx.triage()
	require.Len(t, lines, 1)
	line := lines[0]
	assert.Equal(t, ReasonBadResponse, line.Reason)
	assert.Equal(t, jev.CheckScoreRange, line.Detail)
	assert.Equal(t, QSeverity, line.Question)
	require.NotNil(t, line.Value)
	assert.Equal(t, 7.0, *line.Value)
	assert.Equal(t, 1, line.SampleCount)
	assert.Positive(t, line.RequestChars)
	assert.Nil(t, line.Answers)

	raw, err := os.ReadFile(fx.pass.LogPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "outside 0 to", "the error's text stays out of the line")
	assert.NotContains(t, string(raw), "unusable", "the error's text stays out of the line")
	assert.NotContains(t, string(raw), "jev-1.13.0", "the reply's own text stays out of the line")
}

// bareBadReply is an Asker that returns the bare ErrBadResponse sentinel, not
// the typed *jev.BadResponse: the reply failed in a way that names no check.
type bareBadReply struct{}

func (bareBadReply) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	return nil, jev.ErrBadResponse
}

// TestJevLogtriageBadReplyWithoutACheck: a bad response that is not a typed
// *jev.BadResponse still logs bad_response with the request's size and sample
// count, and leaves the check, question and value empty rather than guessing.
func TestJevLogtriageBadReplyWithoutACheck(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)
	fx.pass.Asker = bareBadReply{}

	fx.run()
	lines := fx.triage()
	require.Len(t, lines, 1)
	line := lines[0]
	assert.Equal(t, ReasonBadResponse, line.Reason)
	assert.Empty(t, line.Detail)
	assert.Empty(t, line.Question)
	assert.Nil(t, line.Value)
	assert.Equal(t, 1, line.SampleCount)
	assert.Positive(t, line.RequestChars)
	assert.Nil(t, line.Answers)

	raw, err := os.ReadFile(fx.pass.LogPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "unusable", "the error's text stays out of the line")
}

// TestJevLogtriageGoodLineHasNoFailureFields: a line whose call answered is
// unchanged by the failure fields, which are omitted from it.
func TestJevLogtriageGoodLineHasNoFailureFields(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.logLines(lineWarn)

	fx.run()
	raw, err := os.ReadFile(fx.pass.LogPath)
	require.NoError(t, err)
	for _, field := range []string{`"detail"`, `"question":`, `"value"`, `"request_chars"`, `"sample_count"`} {
		assert.NotContains(t, string(raw), field)
	}
}
