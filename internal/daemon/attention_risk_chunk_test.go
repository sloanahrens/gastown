package daemon

import (
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestRiskReadChunks pins how a rig's risk-path records are cut into bead
// reads: consecutive chunks of at most riskReadChunkSize records, every record
// once and in order, and no chunk for an empty input (gt-4vuv24).
func TestRiskReadChunks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		records int
		chunks  int
	}{
		{"no records", 0, 0},
		{"exactly one chunk", 50, 1},
		{"one past a chunk", 51, 2},
		{"two chunks and a remainder", 120, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			need := make([]land.LandingRecord, tc.records)
			for i := range need {
				need[i] = land.LandingRecord{BeadID: "gt-" + strconv.Itoa(i)}
			}
			chunks := riskReadChunks(need)
			if len(chunks) != tc.chunks {
				t.Fatalf("riskReadChunks(%d records) gave %d chunks, want %d", tc.records, len(chunks), tc.chunks)
			}
			var got []string
			for _, chunk := range chunks {
				if len(chunk) > riskReadChunkSize {
					t.Errorf("a chunk holds %d records, want at most %d", len(chunk), riskReadChunkSize)
				}
				for _, rec := range chunk {
					got = append(got, rec.BeadID)
				}
			}
			if len(got) != tc.records {
				t.Fatalf("chunks hold %d records, want %d", len(got), tc.records)
			}
			for i, id := range got {
				if want := "gt-" + strconv.Itoa(i); id != want {
					t.Fatalf("record %d is %q, want %q", i, id, want)
				}
			}
		})
	}
}
