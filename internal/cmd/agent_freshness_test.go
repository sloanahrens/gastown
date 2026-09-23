package cmd

import (
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestHeartbeatEpoch(t *testing.T) {
	t.Parallel()

	newer := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	older := newer.Add(-time.Hour)

	tests := []struct {
		name   string
		labels []string
		want   time.Time
		wantOK bool
	}{
		{
			name:   "newest of several stamps wins",
			labels: []string{"gt:agent", "heartbeat:" + epoch(older.Unix()), "idle:3", "heartbeat:" + epoch(newer.Unix())},
			want:   newer,
			wantOK: true,
		},
		{
			name:   "unparseable stamps are ignored",
			labels: []string{"heartbeat:", "heartbeat:not-a-number", "heartbeat:" + epoch(newer.Unix())},
			want:   newer,
			wantOK: true,
		},
		{
			name:   "no heartbeat label",
			labels: []string{"gt:agent", "idle:3"},
			wantOK: false,
		},
		{
			name:   "only unparseable stamps",
			labels: []string{"heartbeat:abc"},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := heartbeatEpoch(tt.labels)
			if ok != tt.wantOK {
				t.Fatalf("heartbeatEpoch() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !got.Equal(tt.want) {
				t.Errorf("heartbeatEpoch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFreshestBeadWrite(t *testing.T) {
	t.Parallel()

	rowTime := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	updatedAt := rowTime.Format(time.RFC3339)

	tests := []struct {
		name      string
		updatedAt string
		labels    []string
		want      time.Time
		wantErr   bool
	}{
		{
			name:      "heartbeat newer than updated_at wins",
			updatedAt: updatedAt,
			labels:    []string{"heartbeat:" + epoch(rowTime.Add(time.Hour).Unix())},
			want:      rowTime.Add(time.Hour),
		},
		{
			name:      "updated_at newer than heartbeat wins",
			updatedAt: updatedAt,
			labels:    []string{"heartbeat:" + epoch(rowTime.Add(-time.Hour).Unix())},
			want:      rowTime,
		},
		{
			name:      "no heartbeat label falls back to updated_at",
			updatedAt: updatedAt,
			labels:    []string{"gt:agent", "idle:0"},
			want:      rowTime,
		},
		{
			name:      "unparseable heartbeat falls back to updated_at",
			updatedAt: updatedAt,
			labels:    []string{"heartbeat:yesterday"},
			want:      rowTime,
		},
		{
			name:      "unparseable updated_at is an error",
			updatedAt: "not-a-time",
			labels:    []string{"heartbeat:" + epoch(rowTime.Unix())},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := freshestBeadWrite(tt.updatedAt, tt.labels)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("freshestBeadWrite() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("freshestBeadWrite() error = %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("freshestBeadWrite() = %v, want %v", got, tt.want)
			}
		})
	}
}

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

// TestStateWriteAdvancesBeadFreshness is the gt-dq5z acceptance test: the write
// gt agents state performs must advance the time the deacon's HEALTH_CHECK reads
// as its primary signal, or a label-only response can never satisfy it.
//
// updated_at stays frozen across the write — that is what bd does with a
// label-only update, and why the heartbeat stamp has to carry the advance.
func TestStateWriteAdvancesBeadFreshness(t *testing.T) {
	t.Parallel()

	rowTime := time.Date(2026, 9, 16, 23, 18, 13, 0, time.UTC)
	updatedAt := rowTime.Format(time.RFC3339)
	allLabels := []string{"gt:agent", "idle:41", "heartbeat:" + epoch(rowTime.Unix())}

	before, err := freshestBeadWrite(updatedAt, allLabels)
	if err != nil {
		t.Fatalf("freshestBeadWrite() before write: %v", err)
	}

	now := rowTime.Add(72 * time.Hour)
	stateLabels := parseStateLabels(allLabels)
	if err := applyLabelOperations(stateLabels, []string{"probe=1"}, "", nil); err != nil {
		t.Fatalf("applyLabelOperations() = %v", err)
	}
	written := buildAgentStateLabels(allLabels, stateLabels, now)

	after, err := freshestBeadWrite(updatedAt, written)
	if err != nil {
		t.Fatalf("freshestBeadWrite() after write: %v", err)
	}

	if !after.After(before) {
		t.Fatalf("freshness did not advance: before %v, after %v", before, after)
	}
	if !after.Equal(now) {
		t.Errorf("freshness = %v, want the write time %v", after, now)
	}
}

// epoch renders a Unix epoch the way an agent bead's heartbeat label carries it.
func epoch(n int64) string {
	return strconv.FormatInt(n, 10)
}
