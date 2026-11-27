package specdispatch

import (
	"fmt"
	"regexp"
)

// creatorField is the Refusal.Field every creator cite refusal names.
const creatorField = "creator cite"

// CreatorState is what the caller read for a bead a spec cites as the creator
// of a file (gt-pt1fb4, creator cite 1/4).
type CreatorState string

const (
	// CreatorBlocker is an open bead that an edge makes the spec wait on.
	CreatorBlocker CreatorState = "blocker"
	// CreatorLanded is a closed bead whose work has landed.
	CreatorLanded CreatorState = "landed"
	// CreatorUnlinked is an open bead that no edge makes the spec wait on.
	CreatorUnlinked CreatorState = "unlinked"
	// CreatorRetired is a bead closed without creating the file; the caller
	// records its close reason on the CreatorCite.
	CreatorRetired CreatorState = "retired"
	// CreatorUnreadable is a bead bd could not read.
	CreatorUnreadable CreatorState = "unreadable"
)

// CreatorCite is one creator bead a spec cites, with the state the caller
// recorded for it. Reason is the close reason of a retired cite.
type CreatorCite struct {
	ID     string
	State  CreatorState
	Reason string
}

// creatorCiteRe reads the bead id after "created by bead". The id is a prefix,
// a dash and a dotted tail, so a sentence's final period is not part of it.
var creatorCiteRe = regexp.MustCompile(`(?i)\bcreated\s+by\s+bead\s+([a-z][a-z0-9]*-[a-z0-9]+(?:\.[a-z0-9]+)*)`)

// CreatorCites returns the bead ids that follow "created by bead" in the Goal
// and Constraints sections, each once, in order of appearance. A bead id named
// any other way is not a creator cite.
func CreatorCites(description string) []string {
	sections := Sections(description)
	var ids []string
	seen := map[string]bool{}
	for _, key := range []string{"goal", "constraints"} {
		for _, m := range creatorCiteRe.FindAllStringSubmatch(sections[key], -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				ids = append(ids, m[1])
			}
		}
	}
	return ids
}

// creatorCiteRefusals is the refusal each recorded cite earns. A cite the
// description names with no state recorded is refused too: once the caller
// records states, a cite it left unjudged would pass as clean. A spec with no
// recorded states is judged as before.
func creatorCiteRefusals(s Spec) []Refusal {
	if len(s.CreatorCites) == 0 {
		return nil
	}
	var out []Refusal
	recorded := make(map[string]bool, len(s.CreatorCites))
	for _, c := range s.CreatorCites {
		recorded[c.ID] = true
		if r, refused := c.judge(s.ID); refused {
			out = append(out, r)
		}
	}
	for _, id := range CreatorCites(s.Description) {
		if !recorded[id] {
			out = append(out, Refusal{Field: creatorField, Reason: fmt.Sprintf("creator cite %s has no recorded state, so it cannot be judged", id)})
		}
	}
	return out
}

// judge is the refusal a cite earns from its recorded state, and whether it
// earns one. A blocker and a landed cite pass. Any other state, including one
// this function does not know, is refused.
func (c CreatorCite) judge(self string) (Refusal, bool) {
	switch c.State {
	case CreatorBlocker, CreatorLanded:
		return Refusal{}, false
	case CreatorUnlinked:
		return Refusal{Field: creatorField, Reason: fmt.Sprintf("creator cite %s is open and not a blocker, so this bead does not wait on the work that creates its file; add the edge with gt bead dep add %s %s", c.ID, self, c.ID)}, true
	case CreatorRetired:
		return Refusal{Field: creatorField, Reason: fmt.Sprintf("creator cite %s is retired (closed %q), so the file it cites will not be created; name a bead that will create it", c.ID, c.Reason)}, true
	case CreatorUnreadable:
		return Refusal{Field: creatorField, Reason: fmt.Sprintf("creator cite %s could not be read, so its state is unknown", c.ID)}, true
	}
	return Refusal{Field: creatorField, Reason: fmt.Sprintf("creator cite %s has unrecognised state %q", c.ID, c.State)}, true
}
