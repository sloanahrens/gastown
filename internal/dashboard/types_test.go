package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A seat's clock is gate time, working time and the phase it is in, filled by
// the hub from the overrun guard. A seat with no clock must marshal exactly as
// before the fields existed, and a filled one must name them gate_sec,
// working_sec and phase (gt-h9qvuc).
func TestPolecatJSONSeatClock(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "starting", PhaseStarting, "a phase's wire value is its constant")
	assert.Equal(t, "working", PhaseWorking, "a phase's wire value is its constant")

	cases := []struct {
		name    string
		polecat Polecat
		absent  []string
		want    map[string]any
	}{
		{
			name:    "zero seat carries no clock keys",
			polecat: Polecat{},
			absent:  []string{"gate_sec", "working_sec", "phase"},
		},
		{
			name:    "filled seat names its clock",
			polecat: Polecat{GateSec: 937, WorkingSec: 720, Phase: PhaseWorking},
			want: map[string]any{
				"gate_sec":    float64(937),
				"working_sec": float64(720),
				"phase":       PhaseWorking,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.polecat)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(b, &got))
			for _, key := range tc.absent {
				assert.NotContains(t, got, key)
			}
			for key, want := range tc.want {
				assert.Equal(t, want, got[key], key)
			}
		})
	}
}

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
