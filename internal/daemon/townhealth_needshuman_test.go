package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// needsHumanTown is healthTown with eight landing rigs and a bead set that has
// one bead under two of the labels, a deferred bead (not actionable), and one
// bead per label of its own.
func needsHumanTown(t *testing.T, now time.Time) (*Daemon, *labelBeads, []string) {
	t.Helper()
	d, bd := healthTown(t, now)
	rigs := map[string]any{}
	var names []string
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("rig%d", i)
		rigs[name] = map[string]any{}
		names = append(names, name)
	}
	writeJSONFile(t, d.config.TownRoot+"/mayor/rigs.json", map[string]any{"version": 1, "rigs": rigs})
	d.invalidateKnownRigsCache()
	at := func(h int) string { return now.Add(-time.Duration(h) * time.Hour).Format(time.RFC3339) }
	bd.issuesByLabel = map[string][]*beads.Issue{
		land.LabelNeedsOverseer: {
			{ID: "o1", Status: "open", CreatedAt: at(5)},
			{ID: "both", Status: "open", CreatedAt: at(9)},
		},
		land.LabelNeedsOperator: {
			{ID: "both", Status: "open", CreatedAt: at(9)},
			{ID: "p1", Status: "open", CreatedAt: at(3)},
			{ID: "parked", Status: "deferred", CreatedAt: at(40)},
		},
		land.LabelNeedsHuman: {
			{ID: "h1", Status: "open", CreatedAt: at(12)},
		},
	}
	return d, bd, names
}

func waitByLabel(t *testing.T, got []townhealth.PersonWait, label string) townhealth.PersonWait {
	t.Helper()
	for _, w := range got {
		if w.Label == label {
			return w
		}
	}
	t.Fatalf("%s: no wait for %s in %+v", t.Name(), label, got)
	return townhealth.PersonWait{}
}

// NeedsHuman reads each landing rig's three labels with one bd list, not one
// per label, and counts what the three-call version counted: a bead under two
// labels once (under the first in the order), a deferred bead not at all
// (gt-5nq22).
func TestNeedsHumanReadsOnceForEachRig(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	d, bd, rigs := needsHumanTown(t, now)

	got, err := (&healthSources{d: d}).NeedsHuman(context.Background())
	if err != nil {
		t.Fatalf("%s: NeedsHuman: %v", t.Name(), err)
	}

	if len(bd.reads) != len(rigs) {
		t.Fatalf("%s: %d List calls, want %d (one per landing rig; it made %d)", t.Name(), len(bd.reads), len(rigs), 3*len(rigs))
	}
	for _, r := range bd.reads {
		if len(r.opts.LabelsAny) != 3 || r.opts.Label != "" {
			t.Errorf("%s: read %+v, want the three labels in LabelsAny and no Label", t.Name(), r.opts)
		}
	}
	// Per rig: overseer {o1, both}, operator {p1} (both already counted, parked
	// is deferred), human {h1}; eight rigs.
	o := waitByLabel(t, got, townhealth.FieldNeedsOverseer)
	p := waitByLabel(t, got, townhealth.FieldNeedsOperator)
	h := waitByLabel(t, got, townhealth.FieldNeedsHuman)
	if o.Count != 16 || p.Count != 8 || h.Count != 8 {
		t.Errorf("%s: counts overseer %d operator %d human %d, want 16, 8, 8", t.Name(), o.Count, p.Count, h.Count)
	}
	if want := now.Add(-9 * time.Hour); !o.Oldest.Equal(want) {
		t.Errorf("%s: overseer oldest %v, want %v", t.Name(), o.Oldest, want)
	}
	if want := now.Add(-3 * time.Hour); !p.Oldest.Equal(want) {
		t.Errorf("%s: operator oldest %v, want %v (the bead also under overseer counts there, the deferred one nowhere)", t.Name(), p.Oldest, want)
	}
	if want := now.Add(-12 * time.Hour); !h.Oldest.Equal(want) {
		t.Errorf("%s: human oldest %v, want %v", t.Name(), h.Oldest, want)
	}
}

// One rig whose read fails fails the field, with the rig named.
func TestNeedsHumanFailsTheFieldWithTheRigNamed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	d, bd, _ := needsHumanTown(t, now)
	bd.failFor = land.LabelNeedsHuman

	_, err := (&healthSources{d: d}).NeedsHuman(context.Background())

	if err == nil || !strings.Contains(err.Error(), "rig") || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("%s: error = %v, want one naming the rig and the failure", t.Name(), err)
	}
}
