// Package cachevol plans the shared polecat cache: the seed Job that fills it, the seed steps that
// Job runs for each pinned rig, and the trim that keeps the claim under its cap. All of it is pure:
// the seed pins, the file listing and the sandbox state are inputs, and nothing here touches a
// filesystem, a cluster or a clock.
package cachevol

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Pin is one rig's seed source: the rig, the commit the seed Job checks out, and the URL the Job's
// init container clones it from.
type Pin struct {
	Rig    string `json:"rig"`
	Commit string `json:"commit"`
	URL    string `json:"url"`
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

// PlanSeed returns the linux seed steps for each pin, in the order the pins were given. A rig name
// that is not a DNS-1123 label (it becomes a directory name in the seed Job), a commit that is not a
// full sha, or a rig named twice is refused, so the seed never runs from a pins file it cannot read
// whole.
func PlanSeed(pins []Pin) ([]RigSeed, error) {
	seen := make(map[string]bool, len(pins))
	out := make([]RigSeed, 0, len(pins))
	for _, p := range pins {
		switch {
		case !validDNS1123Label(p.Rig):
			return nil, fmt.Errorf("the seed pin rig %q is not a DNS-1123 label: lowercase letters, digits and hyphens, at most 63 characters", p.Rig)
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

// buildCacheMinAge is the youngest a build-cache file may be and still be planned for removal: a
// file whose later of atime and mtime is inside this age is left in the claim. Go sets both times
// when it uses a cache entry, but at most once an hour (cmd/go/internal/cache mtimeInterval), so a
// file a seat read within the hour can still carry times older than an hour. A file under the floor
// stays even when that leaves the claim over its cap; Overflow reports it.
const buildCacheMinAge = time.Hour

// PlanTrim returns the removals that bring the listing under capBytes, judged as of now. Build-cache
// files go first, least recently used first (by the later of atime and mtime, ties by path), and never
// while younger than buildCacheMinAge. Module-cache files are removed only when no sandbox is running,
// only while the claim is still over the cap, and only when the build pass reached the end of the
// build cache: a build pass the age floor stopped is not the build cache saying it cannot reach the
// cap, so the floor never moves a trim on to the module cache (the overseer's default decision on the
// design's open question). If the claim cannot reach the cap, Overflow reports how far over it is.
//
// The age is the same later-of-atime-and-mtime figure the order uses: a stale atime can only make a
// file look older than its mtime says, and the later of the two never takes it, so a file Go touched
// is young whatever the mount does to its access time. A file whose times are both unset has no age
// to judge, so it is kept and ends the build pass, as a file under the floor does. A zero now is
// refused: it is a caller's mistake, and by it no file reads as old enough to remove.
func PlanTrim(listing []Entry, capBytes int64, sandboxesRunning bool, now time.Time) (TrimPlan, error) {
	if now.IsZero() {
		return TrimPlan{}, fmt.Errorf("the trim plan needs the time its file ages are judged by: the zero time leaves every file younger than the minimum age, so the plan could remove nothing")
	}
	var plan TrimPlan
	for _, e := range listing {
		plan.Total += e.Size
	}
	remaining := plan.Total

	// buildPassStopped is set when the age floor, rather than the end of the build cache, ended the
	// pass: its remaining files are too young to remove, so the module cache is not the next resort.
	buildPassStopped := false
	for _, e := range sortedLRU(listing, BuildCache) {
		if remaining <= capBytes {
			break
		}
		if e.withinMinAge(now) {
			buildPassStopped = true
			break
		}
		plan.Removals = append(plan.Removals, Removal{Path: e.Path, Size: e.Size})
		remaining -= e.Size
	}

	if remaining > capBytes && !sandboxesRunning && !buildPassStopped {
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
	return plan, nil
}

// withinMinAge reports whether e is too young to remove: its last use is inside buildCacheMinAge of
// now, in the future (a clock that disagrees with the filesystem), or unset.
func (e Entry) withinMinAge(now time.Time) bool {
	used := e.lastUsed()
	if used.IsZero() {
		return true
	}
	return now.Sub(used) < buildCacheMinAge
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

// validDNS1123Label reports whether name is a DNS-1123 label, the form a rig takes as a directory
// name in the seed Job and as a Kubernetes object name: lowercase letters, digits and hyphens, at
// most 63 characters, starting and ending with a letter or digit.
func validDNS1123Label(name string) bool {
	return len(name) <= 63 && dns1123LabelRE.MatchString(name)
}

var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
