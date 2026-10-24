package cmd

import (
	"sort"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// isBlockingDepType reports whether a dependency type gates molecule step
// progress. It is beads' definition, so gt mol and bd answer the same question
// the same way (gt-03mac); parent-child falls out as non-blocking because it is
// the molecule→step hierarchy, not a gate.
func isBlockingDepType(depType string) bool {
	return beads.IsBlockingDependencyType(depType)
}

// sortStepsBySequence sorts step issues by their sequence number suffix (.1, .2, etc.)
func sortStepsBySequence(steps []*beads.Issue) {
	sort.Slice(steps, func(i, j int) bool {
		return extractStepSequence(steps[i].ID) < extractStepSequence(steps[j].ID)
	})
}

// sortStepIDsBySequence sorts step ID strings by their sequence number suffix.
func sortStepIDsBySequence(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		return extractStepSequence(ids[i]) < extractStepSequence(ids[j])
	})
}

// extractStepSequence extracts the numeric sequence suffix from a step ID.
// E.g., "gt-mol.3" -> 3, "gt-mol.12" -> 12
func extractStepSequence(id string) int {
	if idx := strings.LastIndex(id, "."); idx >= 0 {
		if n, err := strconv.Atoi(id[idx+1:]); err == nil {
			return n
		}
	}
	return 999999 // Unknown sequence goes last
}
