package land

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
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

// TestRoundsOMRulesRefusesAnIdlessFindingOverTheCap: Render checks finding ids
// at its cap-exceeded return too. A lane holding one round has nothing to drop,
// so an id-less finding in a ledger too big for the cap would otherwise reach om
// unchecked; it is refused instead, and the landing degrades to no rounds
// (gt-2rvuze, gt-vd31vi).
func TestRoundsOMRulesRefusesAnIdlessFindingOverTheCap(t *testing.T) {
	t.Parallel()
	findings := []Finding{
		{ID: "F1", Path: "a.go", Title: "not atomic"},
		{Path: "b.go", Title: strings.Repeat("x", DefaultRoundsCap)},
	}
	l := ledgerIn(t, "gt-abc")
	must(t, l.Append(1, "head1", findings))
	if _, err := l.Render(1); !errors.Is(err, ErrRoundsCorrupt) || !strings.Contains(err.Error(), "round 1") || !strings.Contains(err.Error(), "finding 1") {
		t.Fatalf("Render over a cap no round can meet = %v, want ErrRoundsCorrupt naming round 1, finding 1", err)
	}

	// Through roundsText the same over-the-cap ledger hands om nothing, and
	// the landing still completes.
	f := newLandFixture(t)
	lander := f.lander()
	lander.RoundsDir = l.OutDir
	if got := lander.roundsText(f.work.BeadID); got != "" {
		t.Errorf("roundsText over a corrupt ledger = %q, want no rounds", got)
	}
	if _, err := lander.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land over a refused ledger: %v", err)
	}
}

// TestRoundsOMRulesDropsAnIdlessFindingOverTheCap: the id check runs on the
// rounds Render emits, so a round the byte cap drops takes its id-less finding
// with it. The same ledger is refused while both rounds fit; at a cap only
// round 2 meets it renders round 2 alone, every finding with an id
// (gt-2rvuze).
func TestRoundsOMRulesDropsAnIdlessFindingOverTheCap(t *testing.T) {
	t.Parallel()
	kept := []Finding{{ID: "F1", Path: "b.go", Title: "kept"}}
	only := ledgerIn(t, "gt-only")
	must(t, only.Append(2, "head2", kept))
	round2, err := only.Render(0)
	must(t, err)

	l := ledgerIn(t, "gt-abc")
	must(t, l.Append(1, "head1", []Finding{{Path: "a.go", Title: "id lost"}}))
	must(t, l.Append(2, "head2", kept))

	if _, err := l.Render(DefaultRoundsCap); !errors.Is(err, ErrRoundsCorrupt) || !strings.Contains(err.Error(), "round 1") {
		t.Fatalf("Render with both rounds kept = %v, want ErrRoundsCorrupt naming round 1", err)
	}

	rounds := renderedOf(t, l, len(round2))
	if len(rounds) != 1 || rounds[0].Round != 2 || !omAcceptsFindingIDs(rounds) {
		t.Fatalf("rendered rounds at a cap that drops round 1 = %+v, want only round 2 with its id", rounds)
	}
}

// TestRoundsOMRulesDegradesTheLanding: a ledger Render refuses costs the
// landing its rounds, not its review: no text, one log line for the bead, and
// the review still runs (gt-2rvuze).
func TestRoundsOMRulesDegradesTheLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.RoundsDir = testutil.PrivateTempDir(t)
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
