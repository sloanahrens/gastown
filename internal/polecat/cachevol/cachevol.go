// Package cachevol plans the shared polecat cache: the seed steps the seed Job runs for each pinned
// rig, and the trim that keeps the shared claim under its cap. Both are pure: the seed pins, the file
// listing and the sandbox state are inputs, and nothing here touches a filesystem, a cluster or a
// clock (design: docs/plans/2026-10-26-k8s-shared-cache-design.md, "The seed pass" and "Cap and trim").
package cachevol

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Pin is one rig's seed source: the commit the seed Job clones and seeds from.
type Pin struct {
	Rig    string
	Commit string
}

// Step is one seed command run in a module directory. Required steps fail the seed when they fail;
// the others are logged and skipped (the Dockerfile's seed RUN, minus its darwin step).
type Step struct {
	Argv     []string
	Required bool
}

// RigSeed is the seed plan for one pinned rig.
type RigSeed struct {
	Rig    string
	Commit string
	Steps  []Step
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PlanSeed returns the linux seed steps for each pin, in the order the pins were given. A pin with no
// rig name, a commit that is not a full sha, or a rig named twice is refused, so the seed never runs
// from a pins file it cannot read whole.
func PlanSeed(pins []Pin) ([]RigSeed, error) {
	seen := make(map[string]bool, len(pins))
	out := make([]RigSeed, 0, len(pins))
	for _, p := range pins {
		switch {
		case p.Rig == "":
			return nil, errors.New("a seed pin names no rig")
		case !commitRE.MatchString(p.Commit):
			return nil, fmt.Errorf("the seed pin for %s is not a full 40-hex commit", p.Rig)
		case seen[p.Rig]:
			return nil, fmt.Errorf("the seed pins name %s twice", p.Rig)
		}
		seen[p.Rig] = true
		out = append(out, RigSeed{Rig: p.Rig, Commit: p.Commit, Steps: seedSteps()})
	}
	return out, nil
}

// seedSteps are the Dockerfile seed RUN's commands for one module, in its order. go mod download
// runs first, because a linux build fetches only the modules linux imports.
func seedSteps() []Step {
	return []Step{
		{Argv: []string{"go", "mod", "download"}, Required: true},
		{Argv: []string{"go", "build", "./..."}, Required: true},
		{Argv: []string{"go", "vet", "./..."}, Required: false},
		{Argv: []string{"go", "test", "-count=1", "-run", "^$", "./..."}, Required: false},
	}
}

// Cache says which part of the shared claim an entry belongs to.
type Cache int

const (
	// BuildCache is GOCACHE: a removed entry costs a recompile, which Go handles as a miss.
	BuildCache Cache = iota
	// ModuleCache is GOMODCACHE: a removed file can break a build that is using it.
	ModuleCache
)

// Entry is one file in the claim's listing.
type Entry struct {
	Path  string
	Cache Cache
	Size  int64
	Atime time.Time
	Mtime time.Time
}

// lastUsed is the later of atime and mtime: Go touches the mtime of a reused entry.
func (e Entry) lastUsed() time.Time {
	if e.Mtime.After(e.Atime) {
		return e.Mtime
	}
	return e.Atime
}

// Removal is one file the trim deletes.
type Removal struct {
	Path string
	Size int64
}

// TrimPlan is what the trim removes and whether the claim is under its cap afterwards.
type TrimPlan struct {
	Removals []Removal
	// Total is the listing's size before any removal.
	Total int64
	// Overflow is true when the claim is still over the cap after every removal the rules allow.
	// The caller raises an alarm; no sandbox is stopped by it.
	Overflow bool
	// OverBytes is how many bytes the claim is still over the cap, when Overflow is true.
	OverBytes int64
}

// PlanTrim returns the removals that bring the listing under capBytes. Build-cache files go first,
// least recently used first (by the later of atime and mtime, ties by path). Module-cache files are
// removed only when no sandbox is running, and only while the claim is still over the cap, so a trim
// never touches the module cache under a running build (the overseer's default decision on the
// design's open question). If the claim cannot reach the cap, Overflow reports how far over it is.
func PlanTrim(listing []Entry, capBytes int64, sandboxesRunning bool) TrimPlan {
	var plan TrimPlan
	for _, e := range listing {
		plan.Total += e.Size
	}
	remaining := plan.Total

	build := sortedLRU(listing, BuildCache)
	for _, e := range build {
		if remaining <= capBytes {
			break
		}
		plan.Removals = append(plan.Removals, Removal{Path: e.Path, Size: e.Size})
		remaining -= e.Size
	}

	if remaining > capBytes && !sandboxesRunning {
		for _, e := range sortedLRU(listing, ModuleCache) {
			if remaining <= capBytes {
				break
			}
			plan.Removals = append(plan.Removals, Removal{Path: e.Path, Size: e.Size})
			remaining -= e.Size
		}
	}

	if remaining > capBytes {
		plan.Overflow = true
		plan.OverBytes = remaining - capBytes
	}
	return plan
}

// sortedLRU is the entries of one cache, least recently used first, ties broken by path.
func sortedLRU(listing []Entry, c Cache) []Entry {
	var out []Entry
	for _, e := range listing {
		if e.Cache == c {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].lastUsed().Equal(out[j].lastUsed()) {
			return out[i].lastUsed().Before(out[j].lastUsed())
		}
		return out[i].Path < out[j].Path
	})
	return out
}
