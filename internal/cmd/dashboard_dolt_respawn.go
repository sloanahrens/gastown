package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The respawn section of the Dolt panel: the beads whose respawn counts have
// reached the limit the sling blocks on, then the beads one count under it, so
// the operator sees what the breaker is about to stop before it stops it. It
// lists polecat.RespawnCounts and resets nothing.

const (
	// doltRespawnTitle is the section's heading.
	doltRespawnTitle = "Beads at the respawn limit"
	// doltRespawnShown is how many at-limit beads the section lists before
	// counting the rest as "+N more": enough to name the storm without pushing
	// the one-under group off the panel.
	doltRespawnShown = 10

	doltRespawnAtLimit  = "at limit"
	doltRespawnOneUnder = "one under"
)

func init() {
	registerDoltSection("respawn", func(ctx context.Context, _ time.Time) (dashboard.DoltSection, error) {
		return readDoltRespawnSection(ctx, polecat.RespawnCounts)
	})
}

// readDoltRespawnSection is the section's registered read. The count reader is
// injected so the rows are testable without a respawn state.
func readDoltRespawnSection(ctx context.Context, counts func(townRoot string) ([]polecat.RespawnCount, error)) (dashboard.DoltSection, error) {
	townRoot, ok := doltTownRoot(ctx)
	if !ok {
		return dashboard.DoltSection{}, errors.New("the respawn section got no town root")
	}
	got, err := counts(townRoot)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("reading the respawn counts: %w", err)
	}
	return doltRespawnSection(got), nil
}

// doltRespawnSection draws the counts as two groups: the beads at the limit,
// the first doltRespawnShown with a "+N more" row when there are more, then the
// beads one count under it. A bead over the limit counts as at the limit,
// because AtLimit is what the sling blocks on, not the ordering of the counts.
func doltRespawnSection(counts []polecat.RespawnCount) dashboard.DoltSection {
	section := dashboard.DoltSection{
		Title: doltRespawnTitle,
		Note:  "respawn counts from the town's state, and the limit the sling blocks on",
		Head:  []string{"group", "bead", "count", "limit"},
	}
	var atLimit, oneUnder []polecat.RespawnCount
	for _, c := range counts {
		switch {
		case c.AtLimit:
			atLimit = append(atLimit, c)
		case c.Limit > 0 && c.Count == c.Limit-1:
			oneUnder = append(oneUnder, c)
		}
	}
	shown := atLimit
	if len(shown) > doltRespawnShown {
		shown = shown[:doltRespawnShown]
	}
	for _, c := range shown {
		section.Rows = append(section.Rows, doltRespawnRow(doltRespawnAtLimit, c))
	}
	if more := len(atLimit) - len(shown); more > 0 {
		section.Rows = append(section.Rows, []string{doltRespawnAtLimit, fmt.Sprintf("+%d more", more), "", ""})
	}
	for _, c := range oneUnder {
		section.Rows = append(section.Rows, doltRespawnRow(doltRespawnOneUnder, c))
	}
	return section
}

// doltRespawnRow is one bead's row: its group, bead, count and limit.
func doltRespawnRow(group string, c polecat.RespawnCount) []string {
	return []string{group, c.Bead, strconv.Itoa(c.Count), strconv.Itoa(c.Limit)}
}
