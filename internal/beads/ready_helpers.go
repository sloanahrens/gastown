package beads

import "fmt"

// The two ready reads below are free functions over Client so a caller can
// name one entry point whatever store it holds. Both still run bd's own
// `ready` query for a *Beads, which is the only way to get bd's blocking
// semantics (blocked_issues_cache, transitive propagation, conditional
// blocks) and the exclude flags ReadyDispatchable travels with (gt-0q80).

// ReadyDispatchable returns the ready issues that are dispatchable work: a
// *Beads asks bd for them with the bookkeeping families excluded server-side,
// any other Client answers from its own Ready set with those families dropped
// client-side.
func ReadyDispatchable(c Client) ([]*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.ReadyDispatchable()
	}
	issues, err := c.Ready()
	if issues == nil {
		// Ready hands back a page WITH the truncated-page sentinel, so only a
		// genuinely empty answer is an error here.
		return nil, err
	}
	return dispatchableOnly(issues), err
}

// dispatchableOnly drops the bookkeeping families (mail, escalations,
// identity, merge queue, event records) from issues.
func dispatchableOnly(issues []*Issue) []*Issue {
	kept := make([]*Issue, 0, len(issues))
	for _, issue := range issues {
		if IsNonDispatchableBead(issue) {
			continue
		}
		kept = append(kept, issue)
	}
	return kept
}

// ReadyForMol returns moleculeID's ready steps, its own issue excluded. A
// Client that is not a *Beads cannot answer: bd's ready --mol query is the
// only source of a molecule's ready set, and no Client method reaches it yet.
func ReadyForMol(c Client, moleculeID string) ([]*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return b.ReadyForMol(moleculeID)
	}
	return nil, fmt.Errorf("ready steps for molecule %s need a bd-backed Client (no Client method covers bd ready --mol)", moleculeID)
}
