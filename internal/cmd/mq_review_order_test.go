package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func ranked(pairs ...interface{}) []rankedMR {
	var out []rankedMR
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, rankedMR{Issue: &beads.Issue{ID: pairs[i].(string)}, Score: pairs[i+1].(float64)})
	}
	return out
}

// gt-tgey7: on 2026-09-23 a P0 (score 1400) waited behind a P2 (score 1203)
// because the refinery gated from a stale queue-scan list. The review gate
// must name the outranking MR instead of reviewing out of order.
func TestOutOfOrderAhead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mrID   string
		score  float64
		queue  []rankedMR
		wantID string // "" = no refusal
	}{
		{"P2 reviewed while a P0 is ready", "gt-p2", 1203.2, ranked("gt-p0", 1400.3, "gt-p2", 1203.2), "gt-p0"},
		{"reviewing the top MR", "gt-p0", 1400.3, ranked("gt-p0", 1400.3, "gt-p2", 1203.2), ""},
		{"tie with the top MR is not out of order", "gt-b", 1300.0, ranked("gt-a", 1300.0, "gt-b", 1300.0), ""},
		{"MR not in the ready list but outranked", "gt-blocked", 1100.0, ranked("gt-p1", 1301.0), "gt-p1"},
		{"MR not in the ready list and outranks everything", "gt-x", 1500.0, ranked("gt-p1", 1301.0), ""},
		{"empty queue", "gt-x", 1200.0, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outOfOrderAhead(tt.mrID, tt.score, tt.queue)
			gotID := ""
			if got != nil {
				gotID = got.Issue.ID
			}
			if gotID != tt.wantID {
				t.Errorf("outOfOrderAhead(%s, %.1f) = %q, want %q", tt.mrID, tt.score, gotID, tt.wantID)
			}
		})
	}
}
