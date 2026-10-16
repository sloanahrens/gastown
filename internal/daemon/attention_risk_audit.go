package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// The resolved-FAIL pass (gt-tlzvh). A deep-review FAIL keeps a risk-path item
// in the attention queue until a later PASS, WAIVED or AUDITED for the same
// head, and the FAIL line names the finding beads it covers (findings=, gt-86uzs).
// When every one of those beads is closed as resolved, the FAIL has nothing
// left to say, and this pass records
//
//	OVERSEER REVIEW <head> AUDITED: findings resolved by <ids>
//
// through land.RecordOverseerReview, the writer gt bead review uses, so the
// item leaves the queue with its reason on the bead.
//
// The queue's collectors never write to beads; this pass is the one write, and
// it runs before the collectors so the item clears on the tick that resolved
// it. It fails closed, and every reason to doubt a finding leaves the FAIL
// alone: a bead that cannot be read, that does not exist, that is in any state
// but closed, or that closed for a reason which abandons it rather than
// resolving it; a findings= id that is not shaped like a bead id, so bd cannot
// be asked for it; and a FAIL with no findings= at all (a note written before
// the suffix existed), which the overseer audits by script. It only appends; no
// review line is edited or removed.

// riskAuditCap bounds the AUDITED lines one tick writes, so a backlog of
// resolved FAILs drains over several ticks instead of one burst of bd writes.
const riskAuditCap = 10

// auditResolvedRiskFails writes an AUDITED line on each risk-path bead whose
// latest review of a landed head is a FAIL naming findings that are all
// resolved, up to riskAuditCap per call, and returns the lines written. A read
// that fails skips the one landing or bead it concerned; the first such error
// is returned after the pass has done what it can, so the caller logs it.
func (s *attentionSources) auditResolvedRiskFails(ctx context.Context) ([]string, error) {
	if s.findingIssue == nil || s.riskAppend == nil {
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
	read := map[string]*beads.Issue{}
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
			issue, err := s.riskIssue(ctx, rig, rec.BeadID)
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
				continue
			}
			if issue == nil {
				continue
			}
			notes := issue.Notes
			if verdict, _ := land.OverseerReviewVerdict(notes, rec.Head); verdict != land.OverseerReviewFail {
				continue
			}
			findings := land.OverseerReviewFindings(notes, rec.Head)
			if len(findings) == 0 {
				continue
			}
			if bad := unshapedFinding(findings); bad != "" {
				note(fmt.Errorf("%s: the review of %s names %q, which is not a bead id", rec.BeadID, rec.Head, bad))
				continue
			}
			resolved, err := s.findingsResolved(ctx, findings, read)
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
			}
			if !resolved {
				continue
			}
			// The finding reads above are bd calls, so the FAIL may have moved
			// under them. bd has no compare-and-swap to append under: the line
			// is judged against the notes as they are now, and a FAIL that
			// changed instead of standing takes the next tick.
			fresh, err := s.riskIssue(ctx, rig, rec.BeadID)
			if err != nil {
				note(fmt.Errorf("%s: %w", rec.BeadID, err))
				continue
			}
			if fresh == nil {
				continue
			}
			if verdict, _ := land.OverseerReviewVerdict(fresh.Notes, rec.Head); verdict != land.OverseerReviewFail {
				continue
			}
			if !slices.Equal(land.OverseerReviewFindings(fresh.Notes, rec.Head), findings) {
				continue
			}
			appender := riskAppender(func(id, line string) error { return s.riskAppend(ctx, rig, id, line) })
			line, written, err := land.RecordOverseerReview(appender, rec.BeadID, fresh.Notes, rec.Head, "AUDITED", nil, "findings resolved by "+strings.Join(findings, ","))
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

// unshapedFinding returns the first finding id that is not shaped like a bead
// id, and "" when every id is. The ids are text out of a bead's notes, and bd
// reads a leading dash as a flag, so an unshaped id is one this pass cannot ask
// bd for.
func unshapedFinding(findings []string) string {
	for _, f := range findings {
		if !beads.IsBeadIDToken(f) {
			return f
		}
	}
	return ""
}

// findingsResolved reports whether every finding bead was closed as resolved.
// The first read error is returned so the pass logs it, and read caches the
// beads of one tick.
func (s *attentionSources) findingsResolved(ctx context.Context, findings []string, read map[string]*beads.Issue) (bool, error) {
	resolved := true
	var firstErr error
	for _, f := range findings {
		is, ok := read[f]
		if !ok {
			var err error
			is, err = s.findingIssue(ctx, f)
			if err != nil {
				// A bead that is absent is as unreadable as one bd failed on:
				// neither says the finding was fixed.
				if firstErr == nil && !errors.Is(err, beads.ErrNotFound) {
					firstErr = fmt.Errorf("finding %s: %w", f, err)
				}
				resolved = false
				continue
			}
			read[f] = is
		}
		if !findingResolved(is) {
			resolved = false
		}
	}
	return resolved, firstErr
}

// findingResolved reports whether a finding bead's read says the finding was
// dealt with: closed, and closed for a reason that resolves it. The two
// prefixes the town records for abandoning a bead — "supersede:" and
// "cancel:" (gt-6hmz) — say the defect was dropped or moved elsewhere, so a
// FAIL this pass would clear is still live.
func findingResolved(is *beads.Issue) bool {
	if is == nil || beads.IssueStatus(strings.TrimSpace(is.Status)) != beads.StatusClosed {
		return false
	}
	reason := strings.ToLower(strings.TrimSpace(is.CloseReason))
	return !strings.HasPrefix(reason, "supersede:") && !strings.HasPrefix(reason, "cancel:")
}

// riskAppender adapts a func to land.NoteAppender.
type riskAppender func(id, note string) error

func (f riskAppender) AppendNotes(id, note string) error { return f(id, note) }

// productionRiskAudit wires the pass's two outside accesses: a read of any
// finding bead, routed by its prefix because a finding may live in another rig,
// and the mutating append the review writer needs.
func (d *Daemon) productionRiskAudit(s *attentionSources) {
	s.findingIssue = func(ctx context.Context, id string) (*beads.Issue, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		is, err := d.workBeads(bdReadOnlyRoutingEnv(d.config.TownRoot), attentionBeadTimeout).Show(id)
		if err != nil {
			return nil, err
		}
		if is == nil {
			return nil, beads.ErrNotFound
		}
		return is, nil
	}
	s.riskAppend = func(ctx context.Context, rig, id, note string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return beads.NewPlain(d.config.TownRoot, bdMutationRoutingEnv(d.config.TownRoot), beads.WithBin(d.bdPathOrDefault())).
			WithTimeout(attentionBeadTimeout).AppendNotes(id, note)
	}
}
