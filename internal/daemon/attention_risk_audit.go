package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// The resolved-FAIL pass (gt-tlzvh). A deep-review FAIL keeps a risk-path item
// in the attention queue until a later PASS, WAIVED or AUDITED for the same
// head, and the FAIL line names the finding beads it covers (findings=, gt-86uzs).
// When every one of those beads is closed, fixed or refuted, the FAIL has
// nothing left to say, and this pass records
//
//	OVERSEER REVIEW <head> AUDITED: findings resolved by <ids>
//
// through land.RecordOverseerReview, the writer gt bead review uses, so the
// item leaves the queue with its reason on the bead.
//
// The queue's collectors never write to beads; this pass is the one write, and
// it runs before the collectors so the item clears on the tick that resolved
// it. It fails closed. A finding bead that cannot be read, that does not exist
// or that is in any state but closed leaves the FAIL alone, and so does a FAIL
// with no findings= (a note written before the suffix existed): the overseer
// audits that backlog by script, and a pass that guessed would clear a defect
// nobody looked at. It only appends; no review line is edited or removed.

// riskAuditCap bounds the AUDITED lines one tick writes, so a backlog of
// resolved FAILs drains over several ticks instead of one burst of bd writes.
const riskAuditCap = 10

// auditResolvedRiskFails writes an AUDITED line on each risk-path bead whose
// latest review of a landed head is a FAIL naming findings that are all closed,
// up to riskAuditCap per call, and returns the lines written. A read that
// fails skips the one landing or bead it concerned; the first such error is
// returned after the pass has done what it can, so the caller logs it.
func (s *attentionSources) auditResolvedRiskFails(ctx context.Context) ([]string, error) {
	if s.findingStatus == nil || s.riskAppend == nil {
		return nil, nil
	}
	var wrote []string
	var firstErr error
	note := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// One read per finding bead per tick: several landings may name the same one.
	status := map[string]string{}
	since := s.now.Add(-attentionRiskPathWindow)
	for _, rig := range s.landingRigs() {
		recs, err := s.riskLandings(rig, since)
		if err != nil {
			note(fmt.Errorf("%s: %w", rig, err))
			continue
		}
		for _, rec := range recs {
			if len(wrote) >= riskAuditCap {
				return wrote, firstErr
			}
			if err := ctx.Err(); err != nil {
				return wrote, err
			}
			if rec.BeadID == "" || rec.Head == "" || len(rec.RiskPaths) == 0 {
				continue
			}
			notes, err := s.riskNotes(ctx, rig, rec.BeadID)
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
				continue
			}
			if verdict, _ := land.OverseerReviewVerdict(notes, rec.Head); verdict != land.OverseerReviewFail {
				continue
			}
			findings := land.OverseerReviewFindings(notes, rec.Head)
			if len(findings) == 0 {
				continue
			}
			resolved, err := s.findingsClosed(ctx, findings, status)
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
			}
			if !resolved {
				continue
			}
			appender := riskAppender(func(id, line string) error { return s.riskAppend(ctx, rig, id, line) })
			line, written, err := land.RecordOverseerReview(appender, rec.BeadID, notes, rec.Head, "AUDITED", nil, "findings resolved by "+strings.Join(findings, ","))
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
				continue
			}
			if written {
				wrote = append(wrote, rec.BeadID+": "+line)
			}
		}
	}
	return wrote, firstErr
}

// findingsClosed reports whether every finding bead is closed. An unreadable
// bead is Unknown, which is not closed: the first read error is returned so the
// pass logs it, and the FAIL stays. status caches the reads of one tick.
func (s *attentionSources) findingsClosed(ctx context.Context, findings []string, status map[string]string) (bool, error) {
	closed := true
	var firstErr error
	for _, f := range findings {
		st, ok := status[f]
		if !ok {
			var err error
			st, err = s.findingStatus(ctx, f)
			if err != nil {
				// A bead that is absent is as unreadable as one bd failed on:
				// neither says the finding was fixed.
				if firstErr == nil && !errors.Is(err, beads.ErrNotFound) {
					firstErr = fmt.Errorf("finding %s: %w", f, err)
				}
				closed = false
				continue
			}
			status[f] = st
		}
		if beads.IssueStatus(strings.TrimSpace(st)) != beads.StatusClosed {
			closed = false
		}
	}
	return closed, firstErr
}

// riskAppender adapts a func to land.NoteAppender.
type riskAppender func(id, note string) error

func (f riskAppender) AppendNotes(id, note string) error { return f(id, note) }

// productionRiskAudit wires the pass's two outside accesses: a read of any
// finding bead's status, routed by its prefix because a finding may live in
// another rig, and the mutating append the review writer needs.
func (d *Daemon) productionRiskAudit(s *attentionSources) {
	s.findingStatus = func(ctx context.Context, id string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		is, err := d.workBeads(bdReadOnlyRoutingEnv(d.config.TownRoot), attentionBeadTimeout).Show(id)
		if err != nil {
			return "", err
		}
		if is == nil {
			return "", beads.ErrNotFound
		}
		return is.Status, nil
	}
	s.riskAppend = func(ctx context.Context, rig, id, note string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot), beads.WithBin(d.bdPathOrDefault())).
			WithTimeout(attentionBeadTimeout).AppendNotes(id, note)
	}
}
