package rehearsal

import (
	"context"
	"fmt"
)

// Collect replaces rec.Gates and rec.Landings with what the rehearsal beads ids
// record: each id's notes and comments give its gates, and its notes give its
// landing, each naming the bead it came from. Neither field is assigned until
// every bead has read, so a failed read stops Collect, returns its error
// wrapped, and leaves both fields unchanged.
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
	return nil
}
