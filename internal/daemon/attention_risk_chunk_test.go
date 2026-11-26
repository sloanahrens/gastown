package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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

// TestRiskReadChunkFails pins that riskReads asks for a rig's beads chunk by
// chunk and that a failed chunk keeps its error to its own records: 120 records
// are read in three batched calls, and when the second fails only its records
// carry the error while the other chunks' records carry their issues
// (gt-h3vez0).
func TestRiskReadChunkFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	head := strings.Repeat("a", 40)
	recs := make([]land.LandingRecord, 120)
	for i := range recs {
		recs[i] = land.LandingRecord{BeadID: "gt-r" + strconv.Itoa(i), Head: head, RiskPaths: []string{"internal/daemon/attention.go"}}
	}
	var calls []int
	f.src.riskIssues = func(_ context.Context, _ string, ids []string) (map[string]*beads.Issue, error) {
		calls = append(calls, len(ids))
		if len(calls) == 2 {
			return nil, errors.New("bd show: boom")
		}
		out := make(map[string]*beads.Issue, len(ids))
		for _, id := range ids {
			out[id] = &beads.Issue{ID: id}
		}
		return out, nil
	}

	got := f.src.riskReads(context.Background(), "alpha", recs)

	if want := []int{50, 50, 20}; fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("%s: batched reads %v, want %v", t.Name(), calls, want)
	}
	for i, rec := range recs {
		read, ok := got[rec.BeadID]
		if !ok {
			t.Fatalf("%s: %s was not read", t.Name(), rec.BeadID)
		}
		if i >= 50 && i < 100 {
			if read.err == nil {
				t.Errorf("%s: %s is in the failed chunk, want its read to carry the error", t.Name(), rec.BeadID)
			}
			continue
		}
		if read.err != nil {
			t.Errorf("%s: %s is outside the failed chunk, want no error, got %v", t.Name(), rec.BeadID, read.err)
		}
		if read.issue == nil || read.issue.ID != rec.BeadID {
			t.Errorf("%s: %s carries issue %+v, want its own", t.Name(), rec.BeadID, read.issue)
		}
	}
}
