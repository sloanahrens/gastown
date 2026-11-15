package fixrecurrence

import (
	"sort"
	"strings"
	"time"
)

// Bead is one bead the pair extractor reads: the prose that may cite another
// bead, the status and timestamps that decide whether it is the cited bead or
// the newer one, and the dependency edges an agent already set. ID and the keys
// of Edges are the store's own spelling, which is lowercase.
type Bead struct {
	ID          string
	Title       string
	Description string
	Status      string
	CreatedAt   time.Time
	ClosedAt    time.Time
	// Edges holds the ids this bead has a dependency edge to or from. A pair
	// carrying an edge in either direction is skipped: the edge an agent set
	// already says how the beads relate.
	Edges map[string]bool
}

// Pair is a bead citing a closed bead that closed before the citing bead was
// created: the candidate the relationship classifiers then read.
type Pair struct {
	// New is the bead whose title or description cites Old.
	New string
	// Old is the closed bead it cites.
	Old string
}

// Pairs returns every citation pair the beads form, sorted by New then Old.
// A bead's title and description are scanned for bead ids; a citation is a pair
// when the cited id names a closed bead in the list, that bead's close time is
// earlier than the citing bead's creation, and no dependency edge already joins
// the two beads in either direction. The comparison reads the beads' own
// timestamps, never the wall clock.
//
// No pair is made by a bead citing itself, by a citation of an id the list does
// not hold, or by a citation of an id the list holds twice — which record is the
// bead, and when it closed, are both unknown. A closed bead whose close time is
// zero makes no pair either: that time is unknown, not the beginning of time.
//
// The citation is not classified here. A pair says the newer bead named the
// older one, not how it relates to it.
func Pairs(beads []Bead) []Pair {
	byID := make(map[string]*Bead, len(beads))
	ambiguous := make(map[string]bool)
	for i := range beads {
		if _, ok := byID[beads[i].ID]; ok {
			ambiguous[beads[i].ID] = true
			continue
		}
		byID[beads[i].ID] = &beads[i]
	}
	var pairs []Pair
	for i := range beads {
		newBead := &beads[i]
		newKey := newBead.ID
		if newKey == "" || ambiguous[newKey] {
			continue
		}
		for _, citedKey := range citations(newBead) {
			if citedKey == newKey || ambiguous[citedKey] {
				continue
			}
			oldBead, ok := byID[citedKey]
			if !ok || !closedBefore(oldBead, newBead) || edged(newBead, oldBead) {
				continue
			}
			pairs = append(pairs, Pair{New: newBead.ID, Old: oldBead.ID})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].New != pairs[j].New {
			return pairs[i].New < pairs[j].New
		}
		return pairs[i].Old < pairs[j].Old
	})
	return pairs
}

// citations is the lowercased ids bead's title and description cite, in the
// order they first appear, each once. An id has the shape the title classifier
// counts, and a candidate touching a longer token is part of that token rather
// than a citation of its own: the gt-wisp of gt-wisp-l8d8bv names no bead.
func citations(bead *Bead) []string {
	var cited []string
	seen := make(map[string]bool)
	for _, text := range []string{bead.Title, bead.Description} {
		for _, at := range beadIDPattern.FindAllStringIndex(text, -1) {
			if continuesIDBefore(text, at[0]) || continuesIDAfter(text, at[1]) {
				continue
			}
			id := strings.ToLower(text[at[0]:at[1]])
			if seen[id] {
				continue
			}
			seen[id] = true
			cited = append(cited, id)
		}
	}
	return cited
}

// closedBefore reports whether old is a closed bead whose close is known and
// earlier than new's creation. A zero close time is unknown, so it makes no
// pair, and a close at the same instant as the creation is not before it.
func closedBefore(old, new *Bead) bool {
	return old.Status == "closed" && !old.ClosedAt.IsZero() && old.ClosedAt.Before(new.CreatedAt)
}

// edged reports whether an agent already set a dependency edge between the two
// beads in either direction: an edge the agent set always wins over a citation.
func edged(a, b *Bead) bool {
	return a.Edges[b.ID] || b.Edges[a.ID]
}
