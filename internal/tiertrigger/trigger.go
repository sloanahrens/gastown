// Package tiertrigger decides which container-backed packages run in a tier
// sweep: a package runs when the change touched one of its trigger paths, or
// when the daily run is due (gt-e69b2).
package tiertrigger

import "strings"

// Runs reports whether a package whose tests read the trigger paths runs in a
// sweep that saw changed: true when dailyRun, or when some changed path is at
// or under a trigger path. An empty changed list runs nothing that is not due
// daily.
func Runs(triggers, changed []string, dailyRun bool) bool {
	if dailyRun {
		return true
	}
	for _, trigger := range triggers {
		trigger = clean(trigger)
		if trigger == "" {
			continue
		}
		for _, path := range changed {
			path = clean(path)
			if path == "" {
				continue
			}
			if trigger == "." || path == trigger || strings.HasPrefix(path, trigger+"/") {
				return true
			}
		}
	}
	return false
}

// clean puts a repo-relative path in the single spelling the prefix match
// reads: no "./" prefix, no trailing "/". The repo root, in any spelling,
// becomes "." and matches every path.
func clean(p string) string {
	if p == "" {
		return ""
	}
	p = strings.TrimRight(p, "/")
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		return "."
	}
	return p
}
