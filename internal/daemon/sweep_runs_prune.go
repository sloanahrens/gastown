package daemon

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// The sweep-runs prune planner (gt-yh6bax) decides which tier-sweep output
// directories under .runtime/sweep-runs/<rig> the disk maintenance patrol may
// remove. A directory is kept unless it is both older than minAge and outside
// the newest keepNewest, so the patrol removes only what the planner argued
// for; it re-checks each listed name on disk before removing one.

// sweepRunEntry is one direct child of .runtime/sweep-runs/<rig> as the
// planner sees it.
type sweepRunEntry struct {
	// Name is the directory's base name: the run id tierSweepRunnerRunID
	// builds for a sweep.
	Name    string
	ModTime time.Time
	Bytes   int64
}

// sweepRunsRemoval is one directory the plan lists for removal and why.
type sweepRunsRemoval struct {
	Name   string
	Bytes  int64
	Reason string
}

// sweepRunsPlan is the planner's decision for one listing.
type sweepRunsPlan struct {
	// Remove lists the directories to remove, in the order they were given.
	Remove []sweepRunsRemoval
	// Skipped names the entries whose name is not one the sweep writes. They
	// are never listed in Remove: a directory the sweep did not make is not
	// the planner's to prune.
	Skipped []string
	// Bytes is the sum of the Remove entries' Bytes.
	Bytes int64
}

// planSweepRunsPrune decides which of entries the patrol may remove. It keeps
// an entry whose ModTime is zero (an unknown age never deletes), whose age is
// under minAge, or that is among the keepNewest newest by ModTime; every other
// entry whose name is a run id the sweep writes is listed in Remove with the
// reason, and every other name is listed in Skipped. An age exactly minAge is
// not under the bound, so the entry is eligible for removal. A tie at the
// keepNewest cut is kept, and a zero ModTime never takes one of the keepNewest
// places: keeping one directory too many is the safe direction.
func planSweepRunsPrune(entries []sweepRunEntry, now time.Time, keepNewest int, minAge time.Duration) sweepRunsPlan {
	var plan sweepRunsPlan
	matched := make([]sweepRunEntry, 0, len(entries))
	for _, entry := range entries {
		if !isSweepRunName(entry.Name) {
			plan.Skipped = append(plan.Skipped, entry.Name)
			continue
		}
		matched = append(matched, entry)
	}

	// The newest keepNewest ModTimes among the entries whose age is known.
	// The cut is the keepNewest-th newest time, and an entry at or after it
	// in time is kept, so a tie at the cut keeps every entry in it.
	var newest []time.Time
	for _, entry := range matched {
		if !entry.ModTime.IsZero() {
			newest = append(newest, entry.ModTime)
		}
	}
	slices.SortFunc(newest, func(a, b time.Time) int { return b.Compare(a) })
	var cut time.Time
	keepByCount := keepNewest > 0 && len(newest) > 0
	if keepByCount {
		index := min(keepNewest, len(newest)) - 1
		cut = newest[index]
	}

	reason := fmt.Sprintf("not among the newest %d and at least %s old", keepNewest, minAge)
	for _, entry := range matched {
		if entry.ModTime.IsZero() {
			continue
		}
		if now.Sub(entry.ModTime) < minAge {
			continue
		}
		if keepByCount && !entry.ModTime.Before(cut) {
			continue
		}
		plan.Remove = append(plan.Remove, sweepRunsRemoval{
			Name:   entry.Name,
			Bytes:  entry.Bytes,
			Reason: reason,
		})
		plan.Bytes += entry.Bytes
	}
	return plan
}

// sweepRunTiers are the tier tokens a run id carries. A stage names one or
// two of them, joined by "-" (tierSweepStages).
var sweepRunTiers = map[string]bool{
	tiersweep.TierShell:       true,
	tiersweep.TierIntegration: true,
	tiersweep.TierRace:        true,
}

// isSweepRunName reports whether name is a run id the sweep writes:
// <rig>-<8 hex>-<tier list>, the shape tierSweepRunnerRunID builds. The rig is
// one or more non-empty hyphen-separated segments, the hash is the 8 lowercase
// hex characters shortSHA keeps of a commit sha, and the tier list is one or
// more distinct tier tokens joined by "-". The tiers are read from the right,
// so a rig name holding hyphens still parses. Any other shape is a directory
// the sweep did not make.
func isSweepRunName(name string) bool {
	parts := strings.Split(name, "-")
	tiers := 0
	seen := make(map[string]bool, len(sweepRunTiers))
	i := len(parts) - 1
	for ; i >= 0 && sweepRunTiers[parts[i]]; i-- {
		if seen[parts[i]] {
			return false
		}
		seen[parts[i]] = true
		tiers++
	}
	// i now names the hash segment; at least one rig segment must precede it.
	if tiers == 0 || i < 1 || !isShortSHA(parts[i]) {
		return false
	}
	for _, segment := range parts[:i] {
		if segment == "" {
			return false
		}
	}
	return true
}

// isShortSHA reports whether s is the 8 lowercase hex characters shortSHA
// keeps of a git commit sha.
func isShortSHA(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
