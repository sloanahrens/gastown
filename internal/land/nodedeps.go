package land

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/nodedeps"
)

// nodeDepsShown bounds the packages one warning names before it counts the
// rest.
const nodeDepsShown = 3

// nodeDepsPreflight returns the warnings for a tree whose installed Node
// packages disagree with its package-lock.json, or nil when they agree or
// there is no npm install to compare (nodedeps, gt-wd12s). The gate prints
// each as a "gate: WARNING" line, and none of them fails it: the gate's own
// steps remain the verdict on the tree.
func nodeDepsPreflight(dir string) []string {
	res, err := nodedeps.Check(dir)
	if err != nil {
		// The comparison did not happen, so this says so rather than
		// reporting the tree in sync.
		return []string{fmt.Sprintf("could not compare node_modules with package-lock.json (%v): the landing gate installs from the lockfile, so this worktree's lint may not be the gate's (gt-wd12s)", err)}
	}
	var warns []string
	if len(res.Mismatches) > 0 {
		warns = append(warns,
			"node_modules disagrees with package-lock.json: "+nodeDepsDetail(res.Mismatches),
			"the landing gate installs from the lockfile, so its lint can fail where this worktree's passes; run npm ci in this worktree to reproduce it before pushing (gt-wd12s)")
	}
	if len(res.NotCompared) > 0 {
		warns = append(warns, fmt.Sprintf("%d package(s) in node_modules could not be compared with package-lock.json: %s (gt-wd12s)",
			len(res.NotCompared), nodeDepsList(res.NotCompared)))
	}
	return warns
}

// nodeDepsDetail names up to nodeDepsShown mismatches and counts the rest.
func nodeDepsDetail(mismatches []nodedeps.Mismatch) string {
	shown := mismatches
	if len(shown) > nodeDepsShown {
		shown = shown[:nodeDepsShown]
	}
	parts := make([]string, 0, len(shown))
	for _, m := range shown {
		if m.Missing {
			parts = append(parts, fmt.Sprintf("%s not installed, lockfile pins %s", m.Path, m.Locked))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s installed %s, lockfile pins %s", m.Path, m.Installed, m.Locked))
	}
	return strings.Join(parts, "; ") + nodeDepsRest(len(mismatches)-len(shown))
}

// nodeDepsList joins at most nodeDepsShown entries and counts the rest.
func nodeDepsList(entries []string) string {
	shown := entries
	if len(shown) > nodeDepsShown {
		shown = shown[:nodeDepsShown]
	}
	return strings.Join(shown, "; ") + nodeDepsRest(len(entries)-len(shown))
}

// nodeDepsRest names the entries a listing left out.
func nodeDepsRest(rest int) string {
	if rest <= 0 {
		return ""
	}
	return fmt.Sprintf("; %d more", rest)
}
