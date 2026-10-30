// Package baks plans the removal of stale backups: config .bak files and old
// binary backups. It only decides. Listing directories, asking lsof which files
// are open and deleting anything belong to the target that uses this plan.
package baks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// planTarget is the Target name a plan carries.
const planTarget = "baks"

// configMarker splits a config backup's group, the text before it, from its suffix.
const configMarker = ".bak"

// binaryMarker splits a binary backup's group, the binary name, from its suffix.
const binaryMarker = ".bak-"

// configKeep is how many of the newest copies of one config file are kept whatever their age.
const configKeep = 30

// configYoung is the age under which every config copy is kept, so a burst of recent copies is never pruned.
const configYoung = 7 * 24 * time.Hour

// binaryKeep is how many of the newest copies of one binary are kept.
const binaryKeep = 2

// installedBin is the installed binary that decides whether a stand-alone bd.<sha> is stale.
const installedBin = "bd"

// deleteKind is the Action kind for a file the plan removes.
const deleteKind = "delete-file"

// ruleLive is the rule a kept live config file carries; only backups are ever removed.
const ruleLive = "live file"

// ruleInstalled is the rule an installed binary carries; it is never removed.
const ruleInstalled = "installed binary"

// ruleUnreadableBin is the rule a bd.<sha> carries when the installed bd cannot be read to compare against.
const ruleUnreadableBin = "installed binary unreadable"

// ruleUnknownBinary is the rule a binary that is neither a backup nor the installed binary carries.
const ruleUnknownBinary = "not a backup"

// configYoungDays is configYoung in whole days, for the rule text a plan carries.
var configYoungDays = int(configYoung / (24 * time.Hour))

// shaName matches a stand-alone bd build named by its git sha, which the installed bd replaces.
var shaName = regexp.MustCompile(`^bd\.[0-9a-f]{7,40}$`)

// Entry is one file as discovery read it.
type Entry struct {
	Path, Name string
	ModTime    time.Time
	Size       int64
	OpenBy     string // non-empty: a process has the file open (from lsof)
}

// Input is everything a plan decides from.
type Input struct {
	Config       []Entry              // files from config directories
	Binaries     []Entry              // files from binary directories
	InstalledMod map[string]time.Time // installed binary name ("bd") -> its mtime
	Now          time.Time
	ReadErrs     []string
}

// BuildPlan decides which backups may be removed. A config copy is removed
// only when it is outside the newest configKeep copies of its file and older
// than configYoung; its file is the name in its own directory, so the same
// name under two directories is two files with two keep counts. A binary
// copy is removed only when it is outside the newest binaryKeep copies of
// its binary. A bd.<sha> is removed only when the installed bd is newer
// than it. An entry that is open is never removed, and still counts toward
// its group's keep count. Everything not removed is kept with the rule that
// kept it. A non-empty ReadErrs refuses the whole plan, because a read that
// failed could hide a backup that must stay. The plan is ordered by Path, so
// the same input always gives the same plan.
func BuildPlan(in Input) diskmaint.Plan {
	if len(in.ReadErrs) > 0 {
		return diskmaint.Plan{Target: planTarget, Refused: "unreadable backup state: " + strings.Join(in.ReadErrs, "; ")}
	}

	plan := diskmaint.Plan{Target: planTarget}
	planConfig(&plan, in.Config, in.Now)
	planBinaries(&plan, in.Binaries, in.InstalledMod)

	sort.SliceStable(plan.Actions, func(i, j int) bool { return plan.Actions[i].Ref < plan.Actions[j].Ref })
	sort.SliceStable(plan.Kept, func(i, j int) bool { return plan.Kept[i].Ref < plan.Kept[j].Ref })
	return plan
}

// groupKey names one file's backups: the directory they live in and the name
// they share. Two files with the same name in different directories are
// different files, each with its own keep count.
type groupKey struct {
	Dir  string
	Stem string
}

// keyOf is the group e belongs to, its stem being the text before marker.
func keyOf(e Entry, marker string) groupKey {
	stem, _, _ := strings.Cut(e.Name, marker)
	return groupKey{Dir: filepath.Dir(e.Path), Stem: stem}
}

// planConfig decides config copies, grouped by their directory and the text
// before their first .bak.
func planConfig(plan *diskmaint.Plan, entries []Entry, now time.Time) {
	groups := map[groupKey][]Entry{}
	for _, e := range entries {
		if !strings.Contains(e.Name, configMarker) {
			keep(plan, e, ruleLive)
			continue
		}
		key := keyOf(e, configMarker)
		groups[key] = append(groups[key], e)
	}

	for key, group := range groups {
		sortNewest(group)
		for i, e := range group {
			switch {
			case e.OpenBy != "":
				keep(plan, e, openRule(e))
			case now.Sub(e.ModTime) < configYoung:
				keep(plan, e, fmt.Sprintf("younger than %dd", configYoungDays))
			case i < configKeep:
				keep(plan, e, fmt.Sprintf("newest %d of %s", configKeep, key.Stem))
			default:
				remove(plan, e, fmt.Sprintf("rule: older than %dd and outside the newest %d of %s", configYoungDays, configKeep, key.Stem))
			}
		}
	}
}

// planBinaries decides binary copies: the .bak-* backups per directory and
// binary, the stand-alone bd.<sha> builds, and the installed binary and anything
// else, which are kept.
func planBinaries(plan *diskmaint.Plan, entries []Entry, installed map[string]time.Time) {
	groups := map[groupKey][]Entry{}
	var shas, others []Entry
	for _, e := range entries {
		switch {
		case strings.Contains(e.Name, binaryMarker):
			key := keyOf(e, binaryMarker)
			groups[key] = append(groups[key], e)
		case shaName.MatchString(e.Name):
			shas = append(shas, e)
		default:
			others = append(others, e)
		}
	}

	for key, group := range groups {
		sortNewest(group)
		for i, e := range group {
			switch {
			case e.OpenBy != "":
				keep(plan, e, openRule(e))
			case i < binaryKeep:
				keep(plan, e, fmt.Sprintf("newest %d of %s", binaryKeep, key.Stem))
			default:
				remove(plan, e, fmt.Sprintf("rule: outside the newest %d of %s", binaryKeep, key.Stem))
			}
		}
	}

	bdMod, haveBd := installed[installedBin]
	for _, e := range shas {
		switch {
		case e.OpenBy != "":
			keep(plan, e, openRule(e))
		case !haveBd:
			keep(plan, e, ruleUnreadableBin)
		case bdMod.After(e.ModTime):
			remove(plan, e, fmt.Sprintf("rule: older than the installed %s", installedBin))
		default:
			keep(plan, e, fmt.Sprintf("not older than the installed %s", installedBin))
		}
	}

	for _, e := range others {
		switch {
		case e.OpenBy != "":
			keep(plan, e, openRule(e))
		default:
			if _, ok := installed[e.Name]; ok {
				keep(plan, e, ruleInstalled)
			} else {
				keep(plan, e, ruleUnknownBinary)
			}
		}
	}
}

// sortNewest orders a group newest first, breaking ties by Path so the plan is stable.
func sortNewest(group []Entry) {
	sort.SliceStable(group, func(i, j int) bool {
		if !group[i].ModTime.Equal(group[j].ModTime) {
			return group[i].ModTime.After(group[j].ModTime)
		}
		return group[i].Path < group[j].Path
	})
}

// openRule is the rule an open entry carries, naming the process that holds it.
func openRule(e Entry) string {
	return "open by " + e.OpenBy
}

// keep records e as kept under rule.
func keep(plan *diskmaint.Plan, e Entry, rule string) {
	plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: e.Path, Rule: rule})
}

// remove records e as a file the plan deletes, with why.
func remove(plan *diskmaint.Plan, e Entry, why string) {
	plan.Actions = append(plan.Actions, diskmaint.Action{Kind: deleteKind, Ref: e.Path, Bytes: e.Size, Why: why})
}
