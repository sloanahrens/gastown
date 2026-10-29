// Package civol trims the CI cache volumes: the Go build cache, the Go module
// cache and the npm cache. Plan decides which paths may be removed; Target.Apply
// removes them inside the docker-in-docker daemon and, with no job running,
// prunes the build cache and the dangling images.
package civol

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// planTarget is the Target name a plan carries.
const planTarget = "civol"

// deleteKind is the Action kind for a path the plan removes.
const deleteKind = "delete-path"

// Volume names the CI runner mounts.
const (
	gocacheVolume = "ci-gocache"
	gomodVolume   = "ci-gomod"
	npmVolume     = "ci-npm"
)

// kbPerGB converts the GB limits to the KB that sizes are measured in.
const kbPerGB = 1024 * 1024

// jobGuard is how far before the oldest running job's start an entry must sit
// to be trimmed while a job runs, so the cache a running build may still be
// writing to is left alone.
const jobGuard = 65 * time.Minute

// Rules a kept row carries, so a dry run explains itself.
const (
	ruleMissing     = "volume missing"
	ruleNoLimit     = "no limit configured"
	ruleJobWaits    = "a job is running; waits for a quiet run"
	ruleJobUnknown  = "job running and oldest job start unknown; waits"
	ruleJobNewer    = "job running; newer than the job guard before oldest job start"
	ruleModuleCache = "go download cache; re-extracted from, never removed"
	ruleNotModule   = "not a whole module directory; never removed"
	ruleTargetMet   = "not needed; trim target reached"
)

// Entry is one file or directory a volume holds.
type Entry struct {
	Path  string // relative to the volume root, no ".." segment and no newline
	MTime time.Time
	Bytes int64
}

// VolumeInput is one CI cache volume as discovery measured it.
type VolumeInput struct {
	Name    string // ci-gocache, ci-gomod, ci-npm
	Exists  bool
	SizeKB  int64
	Entries []Entry // files for ci-gocache and ci-npm; whole "<module>@<version>" directories under pkg/mod for ci-gomod
}

// Limit is the cap a volume may not exceed and the size a trim brings it to, in GB.
type Limit struct {
	Name            string
	CapGB, TargetGB int64
}

// DefaultLimits are the live caps and trim targets of the CI cache script.
var DefaultLimits = []Limit{
	{gocacheVolume, 8, 5},
	{gomodVolume, 5, 3},
	{npmVolume, 1, 1},
}

// Input is everything a plan decides from.
type Input struct {
	Volumes        []VolumeInput
	Limits         []Limit
	RunningJobs    int
	OldestJobStart time.Time // zero: unknown
	ReadErrs       []string
}

// BuildPlan decides which CI cache entries may be removed. A volume over its
// cap loses its oldest entries until the bytes removed reach what brings it to
// its target. A missing volume, one under its cap, and one a running job holds
// lose nothing. A non-empty ReadErrs, or any path that is empty, absolute, has
// a ".." segment, or spans lines, refuses the whole plan, because the plan
// cannot be trusted to name only paths inside its volumes.
func BuildPlan(in Input) diskmaint.Plan {
	if len(in.ReadErrs) > 0 {
		return refused("unreadable CI volume state: " + strings.Join(in.ReadErrs, "; "))
	}
	for _, v := range in.Volumes {
		for _, e := range v.Entries {
			if unsafePath(e.Path) {
				return refused(fmt.Sprintf("%s entry %q is not a relative path inside the volume", v.Name, e.Path))
			}
		}
	}

	limits := make(map[string]Limit, len(in.Limits))
	for _, l := range in.Limits {
		limits[l.Name] = l
	}
	volumes := slices.Clone(in.Volumes)
	slices.SortFunc(volumes, func(a, b VolumeInput) int { return strings.Compare(a.Name, b.Name) })

	plan := diskmaint.Plan{Target: planTarget}
	for _, v := range volumes {
		planVolume(&plan, v, limits, in)
	}
	return plan
}

// planVolume decides one volume in MTime order, so its rows read oldest first.
func planVolume(plan *diskmaint.Plan, v VolumeInput, limits map[string]Limit, in Input) {
	lim, ok := limits[v.Name]
	switch {
	case !v.Exists:
		keep(plan, v.Name, ruleMissing)
		return
	case !ok:
		keep(plan, v.Name, ruleNoLimit)
		return
	case v.SizeKB <= lim.CapGB*kbPerGB:
		keep(plan, v.Name, fmt.Sprintf("under %d GB cap at %s", lim.CapGB, sizeText(v.SizeKB)))
		return
	}

	gated := in.RunningJobs > 0
	var cutoff time.Time
	if gated {
		switch {
		case v.Name != gocacheVolume:
			keep(plan, v.Name, ruleJobWaits)
			return
		case in.OldestJobStart.IsZero():
			keep(plan, v.Name, ruleJobUnknown)
			return
		}
		cutoff = in.OldestJobStart.Add(-jobGuard)
	}

	need := (v.SizeKB - lim.TargetGB*kbPerGB) * 1024
	why := fmt.Sprintf("oldest first; %s over %d GB cap, trim to %d GB", sizeText(v.SizeKB), lim.CapGB, lim.TargetGB)
	entries := slices.Clone(v.Entries)
	slices.SortFunc(entries, byAge)

	var removed int64
	for _, e := range entries {
		ref := v.Name + ":" + e.Path
		switch {
		case gated && !e.MTime.Before(cutoff):
			keep(plan, ref, ruleJobNewer)
		case v.Name == gomodVolume && isModuleCache(e.Path):
			keep(plan, ref, ruleModuleCache)
		case v.Name == gomodVolume && !isModuleDir(e.Path):
			keep(plan, ref, ruleNotModule)
		case removed >= need:
			keep(plan, ref, ruleTargetMet)
		default:
			removed += e.Bytes
			remove(plan, ref, e.Bytes, why)
		}
	}
	if removed < need {
		keep(plan, v.Name, fmt.Sprintf("over %d GB cap at %s; the removable entries do not reach the %d GB target", lim.CapGB, sizeText(v.SizeKB), lim.TargetGB))
	}
}

// refused is the empty plan that says why nothing may be removed.
func refused(why string) diskmaint.Plan {
	return diskmaint.Plan{Target: planTarget, Refused: why}
}

// unsafePath reports a path that could name something outside its volume. Only
// a ".." segment escapes, so a name that merely contains two dots is fine.
func unsafePath(p string) bool {
	return p == "" || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") || strings.ContainsAny(p, "\r\n")
}

// isModuleCache reports a path in the module cache's download area, which Go
// re-extracts from, so removing it would only force a download.
func isModuleCache(p string) bool {
	rest := strings.TrimPrefix(p, "pkg/mod/")
	return rest == "cache" || strings.HasPrefix(rest, "cache/")
}

// isModuleDir reports a whole "<module>@<version>" directory under pkg/mod. The
// version segment must be last, so a file inside a module is not a module.
func isModuleDir(p string) bool {
	rest, ok := strings.CutPrefix(p, "pkg/mod/")
	if !ok {
		return false
	}
	at := strings.LastIndex(rest, "@")
	return at > 0 && !strings.Contains(rest[at:], "/")
}

// byAge orders entries oldest first, breaking ties by Path so the plan is stable.
func byAge(a, b Entry) int {
	if c := a.MTime.Compare(b.MTime); c != 0 {
		return c
	}
	return strings.Compare(a.Path, b.Path)
}

// sizeText renders a size in KB as GB for a rule or a reason.
func sizeText(sizeKB int64) string {
	return fmt.Sprintf("%.1f GB", float64(sizeKB)/kbPerGB)
}

// keep records a path or volume the plan leaves alone, with the rule that did so.
func keep(plan *diskmaint.Plan, ref, rule string) {
	plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: ref, Rule: rule})
}

// remove records a path the plan deletes.
func remove(plan *diskmaint.Plan, ref string, bytes int64, why string) {
	plan.Actions = append(plan.Actions, diskmaint.Action{Kind: deleteKind, Ref: ref, Bytes: bytes, Why: why})
}
