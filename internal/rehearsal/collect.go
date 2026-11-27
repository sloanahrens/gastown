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
// leaves the fields of later steps unchanged. A failure also appends an
// intervention naming the step, so Judge fails the record and it cannot pass
// incomplete.
func Collect(ctx context.Context, t Town, rec *Record, ids []string, hostLoad io.Reader) error {
	var gates []Gate
	var landings []Landing
	for _, id := range ids {
		bead, err := t.Bead(ctx, id)
		if err != nil {
			return failStep(rec, "bead "+id, fmt.Errorf("collect bead %s: %w", id, err))
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
		return failStep(rec, "failed calls", fmt.Errorf("collect failed calls: %w", err))
	}
	failed, err := FailedCalls(bytes.NewReader(log), rec.Start, rec.End)
	if err != nil {
		return failStep(rec, "failed calls", fmt.Errorf("collect failed calls: %w", err))
	}
	rec.FailedCalls = failed
	listing, err := t.Polecats(ctx)
	if err != nil {
		return failStep(rec, "stranded seats", fmt.Errorf("collect stranded seats: %w", err))
	}
	stranded, err := Stranded(listing, t.Rig)
	if err != nil {
		return failStep(rec, "stranded seats", fmt.Errorf("collect stranded seats: %w", err))
	}
	rec.Stranded = stranded
	load, err := LoadSummary(hostLoad, rec.Start, rec.End)
	if err != nil {
		return failStep(rec, "host load", fmt.Errorf("collect host load: %w", err))
	}
	rec.Load = load
	ran, err := sweepRan(log, t.Rig, rec.Start, rec.End)
	if err != nil {
		return failStep(rec, "sweep run", fmt.Errorf("collect sweep run: %w", err))
	}
	rec.SweepRan = ran
	return nil
}

// failStep records the failed step on rec as the intervention "collect failed: <step>"
// and returns err. The entry names the step only, never the output of the read.
func failStep(rec *Record, step string, err error) error {
	rec.Interventions = append(rec.Interventions, "collect failed: "+step)
	return err
}
