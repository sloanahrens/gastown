package fixrecurrence

import (
	"cmp"
	"slices"
)

// The values Proposal.Source takes: the field of the citing bead that supplied
// the relationship.
const (
	// SourceTitle marks a relationship the title's classifier decided.
	SourceTitle = "title"
	// SourceDescription is the citing bead's description, read when the title
	// names no relationship.
	SourceDescription = "description"
	// SourceAmbiguous is a title that cites the closed bead with no rule
	// matched: the pair claims a relationship a reader still has to classify.
	SourceAmbiguous = "ambiguous"
)

// Proposal is one suggested dependency edge from a bead to a closed bead it
// cites.
type Proposal struct {
	// New is the citing bead.
	New string
	// Old is the closed bead its prose is about.
	Old string
	// Rel is the relationship the prose asserts, and Reference for an
	// ambiguous proposal, which asserts none.
	Rel Relationship
	// Confidence is the classifier's confidence in Rel, and 0 for an
	// ambiguous proposal.
	Confidence float64
	// Source is the field that supplied Rel: SourceTitle, SourceDescription,
	// or SourceAmbiguous.
	Source string
	// Ambiguous reports that the title cites Old but no rule matched, so Rel
	// and Confidence carry no claim.
	Ambiguous bool
}

// Propose returns one Proposal for each citation pair Pairs finds that a
// classifier can name, sorted by New then Old: the title's relationship first,
// then the description's, and an ambiguous reference when the title cites the
// closed bead and no rule matched.
func Propose(beads []Bead) []Proposal {
	byID := make(map[string]*Bead, len(beads))
	for i := range beads {
		byID[beads[i].ID] = &beads[i]
	}
	var proposals []Proposal
	for _, pair := range Pairs(beads) {
		// Pairs names a citing bead by the spelling the slice gives it, so its
		// record is here; a pair whose record is missing has no prose to read,
		// so it earns no proposal.
		citing, ok := byID[pair.New]
		if !ok {
			continue
		}
		if p, ok := proposalFor(pair, citing); ok {
			proposals = append(proposals, p)
		}
	}
	slices.SortFunc(proposals, func(a, b Proposal) int {
		if c := cmp.Compare(a.New, b.New); c != 0 {
			return c
		}
		return cmp.Compare(a.Old, b.Old)
	})
	return proposals
}

// proposalFor is the one proposal a pair earns: the title's relationship, else
// the description's, else an ambiguous reference when the title cites the
// closed bead, else none at all.
func proposalFor(pair Pair, citing *Bead) (Proposal, bool) {
	proposal := Proposal{New: pair.New, Old: pair.Old}
	if rel, confidence := ClassifyTitle(citing.Title, pair.Old); rel != Reference {
		proposal.Rel, proposal.Confidence, proposal.Source = rel, confidence, SourceTitle
		return proposal, true
	}
	if rel, confidence := ClassifyDescription(citing.Description, pair.Old); rel != Reference {
		proposal.Rel, proposal.Confidence, proposal.Source = rel, confidence, SourceDescription
		return proposal, true
	}
	// A whole-token citation of the closed bead, not an id the title uses as
	// part of a longer token, is the pair the classifiers could not read.
	if len(idOccurrences(citing.Title, pair.Old)) > 0 {
		proposal.Rel, proposal.Source, proposal.Ambiguous = Reference, SourceAmbiguous, true
		return proposal, true
	}
	return Proposal{}, false
}
