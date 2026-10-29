// Package worktree plans the removal of stale crew worktrees, the overseer-*
// checkouts that pile up under ~/gt/<rig>/crew. BuildPlan only decides;
// target.go discovers the worktrees and reads the git state it decides on.
package worktree

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// MinAge is how long a worktree's last commit must be in the past before the
// worktree may go; a fresher one is someone's live work.
const MinAge = 48 * time.Hour

// minAgeDays is MinAge in whole days, for the rule text a plan carries.
const minAgeDays = int(MinAge / (24 * time.Hour))

// planTarget is the Target name a plan carries.
const planTarget = "worktree"

// crewDir is the parent directory every removable worktree sits in.
const crewDir = "crew"

// removePrefix marks the directories this plan may remove.
const removePrefix = "overseer-"

// cloneDir is the one overseer checkout that is never removed.
const cloneDir = "overseer"

// ruleClone is the rule a Kept entry names for the overseer clone. Every other
// kept worktree gets a report-only rule built from its own facts.
const ruleClone = "clone"

// Worktree is one git worktree as discovery read it.
type Worktree struct {
	Path, Rig, Dir, Branch string // Dir is the base name of Path, e.g. "overseer-red"
	Dirty                  int    // changed plus untracked entries
	LastCommit             time.Time
	Pushed, Landed         bool
	Err                    string // non-empty: this worktree could not be read
}

// BuildPlan decides which worktrees may be removed. A worktree is removed only
// when it is an overseer-* directory under a crew parent, clean, pushed or
// landed, idle for at least MinAge, and read without error. A landed worktree
// also gets its local branch deleted. Everything else is kept with the rule
// that kept it. A non-empty readErrs refuses the whole plan, because a read
// that failed could hide a worktree that must stay. The plan is ordered by
// Path, so the same input always gives the same plan.
func BuildPlan(ws []Worktree, now time.Time, readErrs []string) diskmaint.Plan {
	if len(readErrs) > 0 {
		return diskmaint.Plan{Target: planTarget, Refused: "unreadable worktree state: " + strings.Join(readErrs, "; ")}
	}

	sorted := append([]Worktree(nil), ws...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	plan := diskmaint.Plan{Target: planTarget}
	for _, w := range sorted {
		age := now.Sub(w.LastCommit)
		days := int(age / (24 * time.Hour))

		switch {
		case w.Dir == cloneDir && w.Err == "":
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: w.Path, Rule: ruleClone})
		case !removable(w, age):
			plan.Kept = append(plan.Kept, diskmaint.Kept{Ref: w.Path, Rule: reportRule(w, days)})
		default:
			state := "pushed"
			if w.Landed {
				state = "landed"
			}
			plan.Actions = append(plan.Actions, diskmaint.Action{
				Kind: "remove-worktree",
				Ref:  w.Path,
				Why:  fmt.Sprintf("rule: clean, pushed or landed, idle %dd or more (%s, idle %dd)", minAgeDays, state, days),
			})
			if w.Landed {
				plan.Actions = append(plan.Actions, diskmaint.Action{
					Kind: "delete-branch",
					Ref:  w.Path + "#" + w.Branch,
					Why:  "landed: local branch goes with its worktree",
				})
			}
		}
	}
	return plan
}

// removable reports whether every removal condition holds for w. A worktree
// with no commit time is kept: subtracting the zero time saturates the age to
// the largest duration, which would pass the age gate.
func removable(w Worktree, age time.Duration) bool {
	return strings.HasPrefix(w.Dir, removePrefix) &&
		filepath.Base(filepath.Dir(w.Path)) == crewDir &&
		w.Dirty == 0 &&
		(w.Pushed || w.Landed) &&
		!w.LastCommit.IsZero() &&
		age >= MinAge &&
		w.Err == ""
}

// reportRule is the rule a kept worktree carries: its owner, which is the part
// of Dir before the first dash, and the facts a reader needs to judge it. A
// worktree that could not be read carries its read error in place of the facts,
// which are unknown, so the plan shows why it was kept.
func reportRule(w Worktree, days int) string {
	owner, _, _ := strings.Cut(w.Dir, "-")
	if w.Err != "" {
		return fmt.Sprintf("report-only: owner %s, unreadable: %s", owner, w.Err)
	}
	return fmt.Sprintf("report-only: owner %s, age %dd, dirty %d", owner, days, w.Dirty)
}
