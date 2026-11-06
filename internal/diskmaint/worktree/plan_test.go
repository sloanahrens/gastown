package worktree

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// cleanWorktree is a worktree that meets every removal condition; each table
// row breaks exactly one of them.
func cleanWorktree() Worktree {
	return Worktree{
		Path:       "/town/repo-a/crew/overseer-a",
		Rig:        "repo-a",
		Dir:        "overseer-a",
		Branch:     "polecat/overseer-a/feature",
		Dirty:      0,
		LastCommit: testNow.Add(-72 * time.Hour),
		Pushed:     true,
	}
}

func TestBuildPlanKeepsWhenOneConditionFails(t *testing.T) {
	t.Parallel()
	for name, breakIt := range map[string]func(*Worktree){
		"dirty":                 func(w *Worktree) { w.Dirty = 1 },
		"not pushed or landed":  func(w *Worktree) { w.Pushed = false },
		"under 48 hours":        func(w *Worktree) { w.LastCommit = testNow.Add(-47 * time.Hour) },
		"no commit time":        func(w *Worktree) { w.LastCommit = time.Time{} },
		"a non-overseer dir":    func(w *Worktree) { w.Dir = "other-b"; w.Path = "/town/repo-a/crew/other-b" },
		"not under a crew dir":  func(w *Worktree) { w.Path = "/town/repo-a/overseer-a" },
		"the clone named exact": func(w *Worktree) { w.Dir = "overseer"; w.Path = "/town/repo-a/crew/overseer" },
		"a worktree error":      func(w *Worktree) { w.Err = "git status failed" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := cleanWorktree()
			breakIt(&w)
			p := BuildPlan([]Worktree{w}, testNow, nil)
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none", p.Actions)
			}
			if len(p.Kept) != 1 || p.Kept[0].Ref != w.Path {
				t.Fatalf("kept = %+v, want the one worktree %s", p.Kept, w.Path)
			}
		})
	}
}

func TestBuildPlanRemovesAtExactlyMinAge(t *testing.T) {
	t.Parallel()
	w := cleanWorktree()
	w.LastCommit = testNow.Add(-MinAge)
	p := BuildPlan([]Worktree{w}, testNow, nil)
	if len(p.Actions) != 1 || p.Actions[0].Kind != "remove-worktree" {
		t.Fatalf("actions = %+v, want one remove-worktree at exactly %v", p.Actions, MinAge)
	}
	if want := "idle 2d or more"; !strings.Contains(p.Actions[0].Why, want) {
		t.Errorf("Why = %q, want the threshold %q derived from MinAge", p.Actions[0].Why, want)
	}
}

func TestBuildPlanLandedWorktreeAlsoDeletesBranch(t *testing.T) {
	t.Parallel()
	w := cleanWorktree()
	w.Pushed = false
	w.Landed = true
	p := BuildPlan([]Worktree{w}, testNow, nil)
	if len(p.Actions) != 2 {
		t.Fatalf("actions = %+v, want remove-worktree then delete-branch", p.Actions)
	}
	if p.Actions[0].Kind != "remove-worktree" || p.Actions[0].Ref != w.Path {
		t.Errorf("first action = %+v, want remove-worktree on %s", p.Actions[0], w.Path)
	}
	if want := w.Path + "#" + w.Branch; p.Actions[1].Kind != "delete-branch" || p.Actions[1].Ref != want {
		t.Errorf("second action = %+v, want delete-branch on %s", p.Actions[1], want)
	}
}

func TestBuildPlanPushedOnlyWorktreeKeepsBranch(t *testing.T) {
	t.Parallel()
	p := BuildPlan([]Worktree{cleanWorktree()}, testNow, nil)
	if len(p.Actions) != 1 || p.Actions[0].Kind != "remove-worktree" {
		t.Fatalf("actions = %+v, want remove-worktree alone", p.Actions)
	}
}

// Another owner's finished worktree goes once its branch is landed, like an
// overseer one, and its local branch goes with it.
func TestBuildPlanRemovesOtherOwnersLandedCleanIdleWorktrees(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"operator-x", "dipper-y"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			w := cleanWorktree()
			w.Dir = dir
			w.Path = "/town/repo-a/crew/" + dir
			w.Branch = "crew/" + dir + "/feature"
			w.Pushed = false
			w.Landed = true

			p := BuildPlan([]Worktree{w}, testNow, nil)
			if len(p.Actions) != 2 {
				t.Fatalf("actions = %+v, want remove-worktree then delete-branch", p.Actions)
			}
			if p.Actions[0].Kind != "remove-worktree" || p.Actions[0].Ref != w.Path {
				t.Errorf("first action = %+v, want remove-worktree on %s", p.Actions[0], w.Path)
			}
			if want := w.Path + "#" + w.Branch; p.Actions[1].Kind != "delete-branch" || p.Actions[1].Ref != want {
				t.Errorf("second action = %+v, want delete-branch on %s", p.Actions[1], want)
			}
			if want := "clean, landed"; !strings.Contains(p.Actions[0].Why, want) {
				t.Errorf("Why = %q, want the landed rule %q", p.Actions[0].Why, want)
			}
		})
	}
}

// Another owner's worktree needs the landing, not merely a push, and is never
// its owner's clone: the clone, a dirty one, a pushed-but-unlanded one and a
// fresh one are kept with the rule that kept each.
func TestBuildPlanKeepsCloneDirtyUnpushedAndFreshOthers(t *testing.T) {
	t.Parallel()
	type row struct {
		dir, path string
		dirty     int
		pushed    bool
		landed    bool
		last      time.Time
		want      string
	}
	old := testNow.Add(-72 * time.Hour)
	for name, c := range map[string]row{
		// The clone is landed and old, so only being a clone keeps it.
		"the clone": {dir: "operator", path: "/town/repo-a/crew/operator", landed: true, last: old, want: "clone"},
		"a dirty worktree": {dir: "operator-dirty", path: "/town/repo-a/crew/operator-dirty",
			dirty: 1, landed: true, last: old, want: "report-only: owner operator, age 3d, dirty 1"},
		"a pushed worktree": {dir: "operator-pushed", path: "/town/repo-a/crew/operator-pushed",
			pushed: true, last: old, want: "report-only: owner operator, age 3d, dirty 0"},
		"a fresh worktree": {dir: "operator-fresh", path: "/town/repo-a/crew/operator-fresh",
			landed: true, last: testNow.Add(-47 * time.Hour), want: "report-only: owner operator, age 1d, dirty 0"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := cleanWorktree()
			w.Dir, w.Path = c.dir, c.path
			w.Dirty, w.Pushed, w.Landed = c.dirty, c.pushed, c.landed
			w.LastCommit = c.last

			p := BuildPlan([]Worktree{w}, testNow, nil)
			if len(p.Actions) != 0 {
				t.Fatalf("actions = %+v, want none", p.Actions)
			}
			if len(p.Kept) != 1 || p.Kept[0].Ref != c.path || p.Kept[0].Rule != c.want {
				t.Fatalf("kept = %+v, want %s kept with rule %q", p.Kept, c.path, c.want)
			}
		})
	}
}

// A landed worktree gets delete-branch whatever its Branch field holds, as the
// spec states; the apply step owns what to do with an empty branch name.
func TestBuildPlanLandedWorktreeGetsBranchActionEvenWithoutBranchName(t *testing.T) {
	t.Parallel()
	w := cleanWorktree()
	w.Landed = true
	w.Branch = ""
	p := BuildPlan([]Worktree{w}, testNow, nil)
	if len(p.Actions) != 2 || p.Actions[1].Kind != "delete-branch" {
		t.Fatalf("actions = %+v, want remove-worktree then delete-branch", p.Actions)
	}
}

func TestBuildPlanKeptRulesReadReportOnly(t *testing.T) {
	t.Parallel()
	w := Worktree{
		Path:       "/town/repo-a/crew/other-b",
		Dir:        "other-b",
		Dirty:      2,
		LastCommit: testNow.Add(-3*24*time.Hour - time.Hour),
	}
	p := BuildPlan([]Worktree{w}, testNow, nil)
	if len(p.Kept) != 1 || p.Kept[0].Rule != "report-only: owner other, age 3d, dirty 2" {
		t.Fatalf("kept = %+v, want the report-only owner rule", p.Kept)
	}
}

// A kept worktree whose state could not be read says so in its rule, clone or
// not, so the plan shows why it was kept.
func TestBuildPlanShowsTheReadErrorOfAKeptWorktree(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		dir, path, want string
	}{
		"a worktree": {"overseer-a", "/town/repo-a/crew/overseer-a", "report-only: owner overseer, unreadable: git status failed"},
		"the clone":  {"overseer", "/town/repo-a/crew/overseer", "report-only: owner overseer, unreadable: git status failed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := cleanWorktree()
			w.Dir, w.Path = c.dir, c.path
			w.Err = "git status failed"
			p := BuildPlan([]Worktree{w}, testNow, nil)
			if len(p.Actions) != 0 || len(p.Kept) != 1 || p.Kept[0].Rule != c.want {
				t.Fatalf("plan = %+v, want it kept with rule %q", p, c.want)
			}
		})
	}
}

func TestBuildPlanNamesCloneRule(t *testing.T) {
	t.Parallel()
	w := cleanWorktree()
	w.Dir = "overseer"
	w.Path = "/town/repo-a/crew/overseer"
	p := BuildPlan([]Worktree{w}, testNow, nil)
	if len(p.Kept) != 1 || p.Kept[0].Rule != "clone" {
		t.Fatalf("kept = %+v, want the clone rule", p.Kept)
	}
}

func TestBuildPlanRefusesOnReadErrors(t *testing.T) {
	t.Parallel()
	p := BuildPlan([]Worktree{cleanWorktree()}, testNow, []string{"listing worktrees failed"})
	if p.Refused == "" {
		t.Fatal("plan not refused, want Refused set for a read error")
	}
	if !strings.Contains(p.Refused, "listing worktrees failed") {
		t.Errorf("Refused = %q, want the read error named", p.Refused)
	}
	if len(p.Actions) != 0 || len(p.Kept) != 0 {
		t.Errorf("refused plan has actions %+v and kept %+v, want neither", p.Actions, p.Kept)
	}
}

// The plan is ordered by Path, and the same input always gives the same plan,
// whatever order the caller listed the worktrees in.
func TestBuildPlanIsDeterministicAndOrderedByPath(t *testing.T) {
	t.Parallel()
	a := cleanWorktree()
	b := cleanWorktree()
	b.Path = "/town/repo-a/crew/overseer-b"
	b.Dir = "overseer-b"
	c := Worktree{Path: "/town/repo-a/crew/other-c", Dir: "other-c", LastCommit: testNow}
	d := Worktree{Path: "/town/repo-a/crew/aaa-d", Dir: "aaa-d", LastCommit: testNow}

	forward := []Worktree{a, b, c, d}
	reversed := []Worktree{d, c, b, a}

	first := BuildPlan(forward, testNow, nil)
	if again := BuildPlan(forward, testNow, nil); !reflect.DeepEqual(first, again) {
		t.Fatalf("same input gave different plans:\n%+v\n%+v", first, again)
	}
	if fromReversed := BuildPlan(reversed, testNow, nil); !reflect.DeepEqual(first, fromReversed) {
		t.Fatalf("reordered input gave a different plan:\n%+v\n%+v", first, fromReversed)
	}

	var refs []string
	for _, act := range first.Actions {
		refs = append(refs, act.Ref)
	}
	if want := []string{a.Path, b.Path}; !reflect.DeepEqual(refs, want) {
		t.Errorf("action refs = %v, want %v", refs, want)
	}

	var kept []string
	for _, k := range first.Kept {
		kept = append(kept, k.Ref)
	}
	if want := []string{d.Path, c.Path}; !reflect.DeepEqual(kept, want) {
		t.Errorf("kept refs = %v, want %v", kept, want)
	}
}
