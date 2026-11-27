package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// creatorRetiringPrefixes are the close-reason starts that retire a cite: the
// creator was split, superseded or folded into another bead, so the file it
// cites will not come from it.
var creatorRetiringPrefixes = []string{"split into", "superseded", "folded into"}

// specCreatorStates returns the state of each creator bead spec cites, in
// specdispatch.CreatorCites order (creator cite 3/4). A cite in the open
// blocker set of id is a blocker, answered from the walk with no read of its
// own. Any other cite is read once: a closed one is retired when its close
// reason starts with a retiring prefix, ignoring case and surrounding space,
// and landed otherwise; an open one the walk does not reach is unlinked. A
// cite whose read fails, or answers no bead, is unreadable. A spec with no cite
// reads nothing, and a failed walk of id is its error with no states.
func specCreatorStates(spec specdispatch.Spec, read func(id string) (*beads.Issue, error), id string) ([]specdispatch.CreatorCite, error) {
	cites := specdispatch.CreatorCites(spec.Description)
	if len(cites) == 0 {
		return nil, nil
	}
	blockers, err := openBlockerSet(specBlockerRead(nil, read), id)
	if err != nil {
		return nil, err
	}
	out := make([]specdispatch.CreatorCite, 0, len(cites))
	for _, cite := range cites {
		if blockers[cite] {
			out = append(out, specdispatch.CreatorCite{ID: cite, State: specdispatch.CreatorBlocker})
			continue
		}
		out = append(out, creatorCiteState(read, cite))
	}
	return out, nil
}

// creatorCiteState reads one cite the walk did not reach and judges it.
func creatorCiteState(read func(id string) (*beads.Issue, error), cite string) specdispatch.CreatorCite {
	issue, err := read(cite)
	if err != nil || issue == nil {
		return specdispatch.CreatorCite{ID: cite, State: specdispatch.CreatorUnreadable}
	}
	if strings.ToLower(strings.TrimSpace(issue.Status)) != "closed" {
		return specdispatch.CreatorCite{ID: cite, State: specdispatch.CreatorUnlinked}
	}
	if creatorRetired(issue.CloseReason) {
		return specdispatch.CreatorCite{ID: cite, State: specdispatch.CreatorRetired, Reason: issue.CloseReason}
	}
	return specdispatch.CreatorCite{ID: cite, State: specdispatch.CreatorLanded}
}

// creatorRetired reports whether a close reason starts with a retiring prefix.
func creatorRetired(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	for _, prefix := range creatorRetiringPrefixes {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}
