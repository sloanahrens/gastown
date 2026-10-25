// Package tiertrigger decides which container-backed packages run in a tier
// sweep: a package runs when the change touched one of its trigger paths, or
// when the daily run is due (gt-e69b2).
package tiertrigger

import (
	"path"
	"strings"
)

// Runs reports whether a package whose tests read the trigger paths runs in a
// sweep that saw changed: true when dailyRun, or when some changed path is at
// or under a trigger path. An empty changed list runs nothing that is not due
// daily. A trigger that names no repo path runs the package, since dropping it
// would skip the tests it was meant to cover.
func Runs(triggers, changed []string, dailyRun bool) bool {
	if dailyRun {
		return true
	}
	for _, trigger := range triggers {
		trigger, ok := clean(trigger)
		if !ok {
			return true
		}
		for _, p := range changed {
			p, ok := clean(p)
			if !ok {
				continue
			}
			if trigger == "." || p == trigger || strings.HasPrefix(p, trigger+"/") {
				return true
			}
		}
	}
	return false
}

// clean puts a repo path in the single spelling the prefix match reads, with
// "." and ".." resolved and no "./" prefix or trailing "/". The repo root, in
// any spelling, becomes "." and matches every path. ok is false for a path that
// names nothing under the root: blank, absolute, or climbing above it, none of
// which matches how git spells a changed path.
func clean(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	p = path.Clean(p)
	if p == "." {
		return ".", true
	}
	if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}
