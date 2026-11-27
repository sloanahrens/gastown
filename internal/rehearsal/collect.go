package rehearsal

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// Collect fills rec from the town: Gates and Landings from the rehearsal beads,
// FailedCalls from the daemon log over [rec.Start, rec.End), then Stranded from
// the polecat listing of t.Rig, then Load from hostLoad over the same window and
// SweepRan from the daemon log already read. A step assigns its fields only after
// every read in it succeeds, so a failure wraps its error, stops Collect, and
// leaves the fields of later steps unchanged.
func Collect(ctx context.Context, t Town, rec *Record, ids []string, hostLoad io.Reader) error {
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
	load, err := LoadSummary(hostLoad, rec.Start, rec.End)
	if err != nil {
		return fmt.Errorf("collect host load: %w", err)
	}
	rec.Load = load
	ran, err := sweepRan(log, t.Rig, rec.Start, rec.End)
	if err != nil {
		return fmt.Errorf("collect sweep run: %w", err)
	}
	rec.SweepRan = ran
	return nil
}
