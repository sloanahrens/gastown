package specdispatch

import "slices"

// Inherited is a blocker's effective priority: the best (lowest-number)
// priority among the beads it blocks, directly or through a chain, and the id
// of the bead that gave it.
type Inherited struct {
	Priority int
	From     string
}

// EffectivePriorities gives each blocker the priority of the most urgent bead
// it holds up. own maps a bead id to its priority; blockedBy maps a bead id to
// the ids that block it. Every id that appears in a blockedBy list is a
// blocker.
//
// A blocker with no entry in own (a ready bead whose priority nobody listed)
// gets an entry whenever something it blocks has a known priority. A blocker
// with an own priority gets an entry only when the inherited priority is
// strictly better. An id that blocks nothing is absent from the result, and a
// cycle terminates because a blocker is never part of its own reach.
func EffectivePriorities(own map[string]int, blockedBy map[string][]string) map[string]Inherited {
	dependents := make(map[string][]string)
	for blocked, blockers := range blockedBy {
		for _, b := range blockers {
			dependents[b] = append(dependents[b], blocked)
		}
	}
	out := make(map[string]Inherited)
	for blocker := range dependents {
		best, ok := bestReachable(blocker, dependents, own)
		if !ok {
			continue
		}
		if p, known := own[blocker]; known && best.Priority >= p {
			continue
		}
		out[blocker] = best
	}
	return out
}

// bestReachable walks the beads that start blocks, transitively, and returns
// the best known own priority among them. A bead with no known priority
// contributes nothing. A tie goes to the smallest id, so the result does not
// depend on map order. ok is false when nothing reachable has a known priority.
func bestReachable(start string, dependents map[string][]string, own map[string]int) (Inherited, bool) {
	seen := map[string]bool{start: true}
	queue := []string{start}
	var reach []string
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range dependents[id] {
			if !seen[next] {
				seen[next] = true
				reach = append(reach, next)
				queue = append(queue, next)
			}
		}
	}
	slices.Sort(reach)
	var best Inherited
	found := false
	for _, id := range reach {
		p, known := own[id]
		if known && (!found || p < best.Priority) {
			best = Inherited{Priority: p, From: id}
			found = true
		}
	}
	return best, found
}
