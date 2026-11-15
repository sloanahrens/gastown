package fixrecurrence

import (
	"sort"
	"strings"
	"time"
)

// Bead is one bead the pair extractor reads: the prose that may cite another
// bead, the status and timestamps that decide whether it is the cited bead or
// the newer one, and the dependency edges an agent already set. A citation
// matches an id case-insensitively; the keys of Edges are the store's spelling.
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

// Pairs returns every citation pair the beads form, sorted by New then Old: a
// bead whose title or description cites a closed bead that closed before the
// citing bead was created, where no dependency edge already joins the two.
func Pairs(beads []Bead) []Pair {
	byID := make(map[string]*Bead, len(beads))
	ambiguous := make(map[string]bool)
	for i := range beads {
		key := strings.ToLower(beads[i].ID)
		if _, ok := byID[key]; ok {
			ambiguous[key] = true
			continue
		}
		byID[key] = &beads[i]
	}
	var pairs []Pair
	for i := range beads {
		newBead := &beads[i]
		// An empty id names no bead, and an id the list holds twice names no
		// single record, so neither can be the citing bead.
		newKey := strings.ToLower(newBead.ID)
		if newKey == "" || ambiguous[newKey] {
			continue
		}
		for _, citedKey := range citations(newBead) {
			// A bead is no recurrence of itself, and a citation of an id the
			// list holds twice names no single record.
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
// earlier than the citing bead's creation. A zero close time is unknown, so it
// makes no pair, and a close at the creation instant is not before it.
func closedBefore(older, newer *Bead) bool {
	return older.Status == "closed" && !older.ClosedAt.IsZero() && older.ClosedAt.Before(newer.CreatedAt)
}

// edged reports whether an agent already set a dependency edge between the two
// beads in either direction: an edge the agent set always wins over a citation.
func edged(a, b *Bead) bool {
	return a.Edges[b.ID] || b.Edges[a.ID]
}
