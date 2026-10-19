package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A state whose seats were never read has no summary time. time.Time ignores
// omitempty, so a zero SummaryAt used to go out as 0001-01-01 and the page drew
// "seats read" an age that no read made (gt-bfh09).
func TestStateSummaryAtZeroOmitted(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(State{})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.NotContains(t, got, "summary_at")

	at := time.Date(2026, 10, 19, 5, 0, 0, 0, time.UTC)
	b, err = json.Marshal(State{SummaryAt: at})
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "2026-10-19T05:00:00Z", got["summary_at"], "a read time keeps its shape")
}
