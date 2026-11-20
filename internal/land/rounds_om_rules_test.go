package land

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// omAcceptsFindingIDs restates om's id rule for a --rounds-file
// (internal/verdict/rounds.go ParseRounds in the om rig): every finding has an id.
func omAcceptsFindingIDs(rounds rendered) bool {
	for _, r := range rounds {
		for _, f := range r.Findings {
			if f.ID == "" {
				return false
			}
		}
	}
	return true
}

// TestRoundsOMRulesRefusesAnIdlessFinding: Render refuses the ledger om would
// refuse for an id-less finding, naming the round and finding, and an all-ids
// ledger renders as before (gt-2rvuze).
func TestRoundsOMRulesRefusesAnIdlessFinding(t *testing.T) {
	t.Parallel()
	withIDs := []Finding{{ID: "F1", Path: "a.go", Title: "not atomic"}, {ID: "F2", Path: "b.go", Title: "name"}}
	idless := []Finding{{ID: "F1", Path: "a.go", Title: "not atomic"}, {Path: "b.go", Title: "id lost"}}
	for _, tc := range []struct {
		name     string
		findings []Finding
		wantErr  bool
	}{{"all ids", withIDs, false}, {"an empty id", idless, true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := ledgerIn(t, "gt-abc")
			must(t, l.Append(1, "head1", tc.findings))
			_, err := l.Render(0)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Render on an all-ids ledger = %v", err)
				}
				if rounds := renderedOf(t, l, 0); !omAcceptsFindingIDs(rounds) || len(rounds) != 1 || len(rounds[0].Findings) != 2 {
					t.Fatalf("rendered rounds = %+v, want round 1 with both ids", rounds)
				}
				return
			}
			if !errors.Is(err, ErrRoundsCorrupt) || !strings.Contains(err.Error(), "round 1") || !strings.Contains(err.Error(), "finding 1") {
				t.Fatalf("Render on an id-less ledger = %v, want ErrRoundsCorrupt naming round 1, finding 1", err)
			}
		})
	}
}

// TestRoundsOMRulesDegradesTheLanding: a ledger Render refuses costs the
// landing its rounds, not its review: no text, one log line for the bead, and
// the review still runs (gt-2rvuze).
func TestRoundsOMRulesDegradesTheLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.RoundsDir = t.TempDir()
	must(t, (&RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID, Now: l.now}).Append(1, "earlier", []Finding{
		{ID: "F1", Path: "b.txt", Title: "broken"}, {Path: "b.txt", Title: "id lost"}}))
	var out strings.Builder
	l.Out = &out
	for i := 0; i < 2; i++ { // named once for the bead, not once per read
		if got := l.roundsText(f.work.BeadID); got != "" {
			t.Errorf("roundsText = %q, want no rounds", got)
		}
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land over a refused ledger: %v", err)
	}
	if n := strings.Count(out.String(), "could not read the rounds ledger"); n != 1 || len(f.review.calls) != 1 {
		t.Errorf("named %d time(s), review ran %d time(s), want once each:\n%s", n, len(f.review.calls), out.String())
	}
}
