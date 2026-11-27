package rehearsal

import (
	"bytes"
	"context"
	"fmt"
)

// Collect fills rec from the town: Gates and Landings from the rehearsal beads,
// FailedCalls from the daemon log over [rec.Start, rec.End), then Stranded from
// the polecat listing of t.Rig. A step assigns its fields only after every read
// in it succeeds, so a failure wraps its error and leaves later fields unchanged.
func Collect(ctx context.Context, t Town, rec *Record, ids []string) error {
	var gates []Gate
	var landings []Landing
	for _, id := range ids {
		bead, err := t.Bead(ctx, id)
		if err != nil {
			return fmt.Errorf("collect bead %s: %w", id, err)
		}
		for _, g := range GateFacts(bead.Notes, bead.Comments) {
			g.Bead = id
			gates = append(gates, g)
		}
		landings = append(landings, LandingFacts(id, bead.Notes))
	}
	rec.Gates, rec.Landings = gates, landings
	log, err := t.DaemonLog(ctx)
	if err != nil {
		return fmt.Errorf("collect failed calls: %w", err)
	}
	failed, err := FailedCalls(bytes.NewReader(log), rec.Start, rec.End)
	if err != nil {
		return fmt.Errorf("collect failed calls: %w", err)
	}
	rec.FailedCalls = failed
	listing, err := t.Polecats(ctx)
	if err != nil {
		return fmt.Errorf("collect stranded seats: %w", err)
	}
	stranded, err := Stranded(listing, t.Rig)
	if err != nil {
		return fmt.Errorf("collect stranded seats: %w", err)
	}
	rec.Stranded = stranded
	return nil
}
