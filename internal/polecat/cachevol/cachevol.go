// Package cachevol plans the shared polecat cache: the seed Job that fills it, the seed steps that
// Job runs for each pinned rig, and the trim that keeps the claim under its cap. All of it is pure:
// the seed pins, the file listing and the sandbox state are inputs, and nothing here touches a
// filesystem, a cluster or a clock.
package cachevol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
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
	// Output is the claim-relative path of the -d file this entry's -a header names, empty for any
	// other entry: Go's build cache records an action's output id in the index file beside it, and
	// the lister reads it there. A path the listing does not hold as a build-cache entry is not
	// paired with anything.
	Output string
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
	// Protected is the size of the build-cache units the age floor kept in the claim: the unit the
	// build pass stopped at and every unit after it, sorted as the plan sorts them. It is 0 when the
	// build pass reached the end of the build cache or the claim was under the cap.
	Protected int64
}

// goCacheTouchInterval is Go's cache mtimeInterval: a reused build-cache entry has its mtime reset to
// now at most once this often, so its mtime is older than its last use by up to this much
// (cmd/go/internal/cache, cache.go: "The mtimes therefore roughly reflect 'time of last use' but may
// in fact be older by at most an hour").
const goCacheTouchInterval = time.Hour

// gateMargin is how long after a gate started the trim still keeps every file the gate uses.
const gateMargin = 15 * time.Minute

// buildCacheMinAge is the youngest a build-cache file may be and still be planned for removal: a
// file whose later of atime and mtime is inside this age is left in the claim, so no file used
// within gateMargin of the trim is planned. Go resets a reused entry's mtime at most once a
// goCacheTouchInterval, so the floor is that interval plus the margin: a file used a minute before
// the trim can carry an mtime almost goCacheTouchInterval plus that minute, and a floor of the
// interval alone keeps nothing a gate used. A file under the floor stays even when that leaves the
// claim over its cap; Overflow reports it.
const buildCacheMinAge = goCacheTouchInterval + gateMargin

// PlanTrim returns the removals that bring the listing under capBytes, judged as of now. Build-cache
// files go first, least recently used first (by the later of atime and mtime, ties by path), and never
// while younger than buildCacheMinAge. An -a file goes with the -d file its header names, and a -d
// file with every -a file that names it, as one unit: the unit sits where its youngest file would, is
// kept whole while any file in it is inside the minimum age, and its bytes leave the claim once
// (gt-udrvfw). Module-cache files are removed only when no sandbox is running, only while the claim
// is still over the cap, and only when the build pass reached the end of the build cache: a build
// pass the age floor stopped is not the build cache saying it cannot reach the cap, so the floor
// never moves a trim on to the module cache (the overseer's default decision on the design's open
// question). If the claim cannot reach the cap, Overflow reports how far over it is.
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
	for _, u := range sortedUnits(listing) {
		if remaining <= capBytes {
			break
		}
		if u.withinMinAge(now) {
			// Every unit from here on is at least as young, so the pass keeps all of them.
			buildPassStopped = true
			for _, e := range u.entries {
				plan.Protected += e.Size
			}
			continue
		}
		for _, e := range u.entries {
			plan.Removals = append(plan.Removals, Removal{Path: e.Path, Size: e.Size})
			remaining -= e.Size
		}
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
	return withinAgeFloor(e.lastUsed(), now)
}

// withinAgeFloor is the one age comparison the floor makes: an unset used, or one inside buildCacheMinAge
// of now (future included), is too young to remove. Entry and BuildCacheInUse both judge by it.
func withinAgeFloor(used, now time.Time) bool {
	return used.IsZero() || now.Sub(used) < buildCacheMinAge
}

// BuildCacheInUse reports whether the age floor keeps the build-cache file rel, last modified at mtime,
// as of now. A module-cache or other path, or one not claim-relative, is never in use.
func BuildCacheInUse(rel string, mtime, now time.Time) bool {
	if !claimRelative(rel) || !strings.HasPrefix(rel, SeedBuildCacheDir+"/") {
		return false
	}
	return withinAgeFloor(mtime, now)
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

// trimUnit is the build-cache files one removal takes together. Go reads an -a file's output path
// out of its header, so removing one half of a pair leaves an index naming a file that is gone, which
// a later build reports as a missing export data file (gt-udrvfw). An -a file and the -d file it
// names are one unit, and a -d file joins every -a file that names it: a build reads any of them.
type trimUnit struct {
	entries []Entry
}

// lastUsed is the unit's youngest file's last use: the unit cannot go before the last of its files
// may.
func (u trimUnit) lastUsed() time.Time {
	latest := u.entries[0].lastUsed()
	for _, e := range u.entries[1:] {
		if used := e.lastUsed(); used.After(latest) {
			latest = used
		}
	}
	return latest
}

// withinMinAge reports whether any of the unit's files is too young to remove: the unit goes whole,
// so one file inside the floor keeps all of it.
func (u trimUnit) withinMinAge(now time.Time) bool {
	for _, e := range u.entries {
		if e.withinMinAge(now) {
			return true
		}
	}
	return false
}

// sortedUnits is the build cache's entries grouped into the units a removal takes together, least
// recently used first by the unit's youngest file and ties by its first path. An entry whose Output
// the listing does not hold as a build-cache entry is a unit of one: a path the listing does not name
// is not a trim's to remove with it, and a module-cache file is never pulled into a build-cache
// removal.
func sortedUnits(listing []Entry) []trimUnit {
	listed := make(map[string]bool, len(listing))
	for _, e := range listing {
		if e.Cache == BuildCache {
			listed[e.Path] = true
		}
	}
	parent := make(map[string]string, len(listed))
	for p := range listed {
		parent[p] = p
	}
	// find returns the unit a path belongs to, halving the walk on the way.
	var find func(p string) string
	find = func(p string) string {
		for parent[p] != p {
			parent[p] = parent[parent[p]]
			p = parent[p]
		}
		return p
	}
	for _, e := range listing {
		if e.Cache != BuildCache || !listed[e.Output] {
			continue
		}
		a, b := find(e.Path), find(e.Output)
		if a == b {
			continue
		}
		// The smaller path is the root, so which of two joined paths ends up there does not depend
		// on the listing's order.
		if b < a {
			a, b = b, a
		}
		parent[b] = a
	}

	byRoot := make(map[string]*trimUnit, len(listed))
	for _, e := range listing {
		if e.Cache != BuildCache {
			continue
		}
		root := find(e.Path)
		u := byRoot[root]
		if u == nil {
			u = &trimUnit{}
			byRoot[root] = u
		}
		u.entries = append(u.entries, e)
	}
	units := make([]trimUnit, 0, len(byRoot))
	for _, u := range byRoot {
		sort.SliceStable(u.entries, func(i, j int) bool { return u.entries[i].Path < u.entries[j].Path })
		units = append(units, *u)
	}
	sort.SliceStable(units, func(i, j int) bool {
		if !units[i].lastUsed().Equal(units[j].lastUsed()) {
			return units[i].lastUsed().Before(units[j].lastUsed())
		}
		return units[i].entries[0].Path < units[j].entries[0].Path
	})
	return units
}

// validDNS1123Label reports whether name is a DNS-1123 label, the form a rig takes as a directory
// name in the seed Job and as a Kubernetes object name: lowercase letters, digits and hyphens, at
// most 63 characters, starting and ending with a letter or digit.
func validDNS1123Label(name string) bool {
	return len(name) <= 63 && dns1123LabelRE.MatchString(name)
}

// buildActionHeaderLen is how many bytes of a build-cache -a file hold the ids BuildActionOutput
// reads: the version, the action id and the output id, each followed by its space.
const buildActionHeaderLen = 3 + 64 + 1 + 64 + 1

var buildActionPathRE = regexp.MustCompile(`^` + regexp.QuoteMeta(SeedBuildCacheDir) + `/[0-9a-f]{2}/[0-9a-f]{64}-a$`)

// isBuildActionFile reports whether p is a Go build-cache action file, the index Go writes beside an
// action's output: gocache/<two hex digits>/<64 hex digits>-a.
func isBuildActionFile(p string) bool { return buildActionPathRE.MatchString(p) }

// BuildActionOutput returns the claim-relative path of the -d file the build-cache action file at p
// names in header, and false for a p that is not an action file or bytes Go itself would read as a
// cache miss.
func buildActionOutput(p string, header []byte) (string, bool) {
	if !isBuildActionFile(p) {
		return "", false
	}
	rest, ok := bytes.CutPrefix(header, []byte("v1 "))
	if !ok {
		return "", false
	}
	action, rest, ok := cutCacheID(rest)
	if !ok || action+"-a" != path.Base(p) {
		return "", false
	}
	output, _, ok := cutCacheID(rest)
	if !ok {
		return "", false
	}
	return SeedBuildCacheDir + "/" + output[:2] + "/" + output + "-d", true
}

// readBuildActionHeader reads the start of a build-cache -a file however short, and returns only a
// read error, since Go reads a short file as a cache miss.
func readBuildActionHeader(r io.Reader) ([]byte, error) {
	buf := make([]byte, buildActionHeaderLen)
	n, err := io.ReadFull(r, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// BuildActionOutputFrom returns the claim-relative path of the -d file the build-cache action file at
// p names, reading its header through open. A file that is gone, is not an action file, or holds a
// header Go reads as a cache miss names no live output and is paired with nothing; a file that
// cannot be read is an error, since the pair is then unknown and a trim would remove the -d file
// without it (gt-udrvfw).
func BuildActionOutputFrom(p string, open func() (io.ReadCloser, error)) (string, error) {
	if !isBuildActionFile(p) {
		return "", nil
	}
	f, err := open()
	if errors.Is(err, fs.ErrNotExist) {
		// Another Job removed the file after the walk read it, so no listing names it.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// A close on a file read once cannot change what the header said.
	defer func() { _ = f.Close() }()
	header, err := readBuildActionHeader(f)
	if err != nil {
		return "", err
	}
	output, _ := buildActionOutput(p, header)
	return output, nil
}

// cutCacheID cuts 64 hex digits and the space after them from the front of b and returns the id
// lower-case, the form Go writes into a file name. Go decodes either case, so both are read.
func cutCacheID(b []byte) (id string, rest []byte, ok bool) {
	if len(b) < 65 || b[64] != ' ' {
		return "", nil, false
	}
	for _, c := range b[:64] {
		switch {
		case '0' <= c && c <= '9':
		case 'a' <= c && c <= 'f':
		case 'A' <= c && c <= 'F':
		default:
			return "", nil, false
		}
	}
	return strings.ToLower(string(b[:64])), b[65:], true
}

var dns1123LabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
