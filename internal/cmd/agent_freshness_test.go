package cmd

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestBuildAgentStateLabels(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	stamp := heartbeatLabelKey + ":" + epoch(now.Unix())

	tests := []struct {
		name      string
		allLabels []string
		mutate    func(map[string]string)
		want      []string
	}{
		{
			name:      "colon labels round-trip and the stamp is appended",
			allLabels: []string{"gt:agent", "idle:3"},
			mutate:    func(m map[string]string) { m["idle"] = "0" },
			want:      []string{"gt:agent", "idle:0", stamp},
		},
		{
			name:      "older stamp replaced",
			allLabels: []string{"idle:3", "heartbeat:1"},
			want:      []string{"idle:3", stamp},
		},
		{
			name:      "stamp beats one supplied as state",
			allLabels: []string{"heartbeat:1"},
			mutate:    func(m map[string]string) { m["backoff"] = "30s" },
			want:      []string{"backoff:30s", stamp},
		},
		{
			name: "stamp alone when the bead carries no labels",
			want: []string{stamp},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateLabels := parseStateLabels(tt.allLabels)
			if tt.mutate != nil {
				tt.mutate(stateLabels)
			}

			got := buildAgentStateLabels(tt.allLabels, stateLabels, now)
			if len(got) != len(tt.want) {
				t.Fatalf("buildAgentStateLabels() = %v, want %v", got, tt.want)
			}
			for _, label := range tt.want {
				if !slices.Contains(got, label) {
					t.Errorf("buildAgentStateLabels() = %v, missing %q", got, label)
				}
			}
		})
	}
}

// epoch renders a Unix epoch the way an agent bead's heartbeat label carries it.
func epoch(n int64) string {
	return strconv.FormatInt(n, 10)
}
