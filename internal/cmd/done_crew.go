package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// A crew session submits its branch with the same gt done a polecat uses
// (gt-3e7tk). Crew push their own branches and have no seat, hook, Witness
// or session to retire, so the crew path is only the landing request: check
// the pushed branch, gate it, and mark the work bead ready to land in the
// shape the landing worker reads (land.ParseReadyNote and the worker's
// submission-comment pattern).

// doneIsCrewRun reports whether this gt done is a crew submission: no
// polecat identity anywhere in the environment, and BD_ACTOR and GT_ROLE,
// when set, name a crew member. Every other identity takes the polecat path
// unchanged, including its refusals.
func doneIsCrewRun(getenv func(string) string) bool {
	actor := strings.TrimSpace(getenv("BD_ACTOR"))
	if isPolecatActor(actor) || strings.TrimSpace(getenv("GT_POLECAT")) != "" {
		return false
	}
	if actor != "" && !isCrewActor(actor) {
		return false
	}
	if role := strings.TrimSpace(getenv("GT_ROLE")); role != "" {
		parsed, _, _ := parseRoleString(role)
		return parsed == RoleCrew
	}
	return true
}

// isCrewActor reports whether actor has the crew shape <rig>/crew/<name>.
func isCrewActor(actor string) bool {
	parts := strings.Split(strings.TrimSpace(actor), "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] == "crew" && parts[2] != ""
}

// crewBeadFromBranch is the bead id a crew branch name carries, or "". A
// crew branch is crew/<user>/<slug>, and a slug such as "crew-done-submit"
// looks like a bead id to the polecat parser, so only an id whose prefix
// the town routes counts.
func crewBeadFromBranch(branch string, routed func(prefix string) bool) string {
	id := parseBranchName(branch).Issue
	if id == "" {
		return ""
	}
	if prefix := beads.ExtractPrefix(id); prefix == "" || !routed(prefix) {
		return ""
	}
	return id
}

// runDoneCrew wires a crew submission from the process environment and the
// worktree, then submits it.
func runDoneCrew(exitType string, getenv func(string) string) error {
	if exitType != ExitCompleted {
		return fmt.Errorf("gt done --status %s is for polecats; a crew session has no Witness to signal (submit with plain gt done)", exitType)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("gt done: current directory unavailable: %w", err)
	}
	if err := doneRejectGitEnvOverrides(getenv); err != nil {
		return err
	}
	gitRoot, err := doneGitTopLevel(cwd)
	if err != nil {
		return fmt.Errorf("gt done must be run from a git worktree: %w", err)
	}

	// A crew worktree may live outside the town (a scratch worktree of a
	// crew clone); the session's town root still routes its beads.
	townRoot, _ := workspace.Find(cwd)
	if townRoot == "" {
		townRoot = firstNonEmpty(getenv("GT_TOWN_ROOT"), getenv("GT_ROOT"))
	}
	if townRoot == "" {
		return fmt.Errorf("gt done: not in a Gas Town workspace and GT_TOWN_ROOT is unset")
	}

	g := git.NewGit(gitRoot)
	actor := strings.TrimSpace(getenv("BD_ACTOR"))
	if actor == "" {
		// Crew have no seat identity; the bead records the committer.
		name, _ := g.ConfigGet("user.name")
		actor = strings.TrimSpace(name)
		if actor == "" {
			return fmt.Errorf("gt done needs BD_ACTOR or git user.name to attribute the submission")
		}
		if err := os.Setenv("BD_ACTOR", actor); err != nil {
			return fmt.Errorf("setting BD_ACTOR for bd: %w", err)
		}
	}

	branch, err := g.CurrentBranch()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}
	if err := requireRealCurrentBranch(branch, "gt done"); err != nil {
		return err
	}

	issueID := firstNonEmpty(doneBead, doneIssue)
	if issueID == "" {
		issueID = crewBeadFromBranch(branch, func(prefix string) bool {
			return beads.GetRigNameForPrefix(townRoot, prefix) != ""
		})
	}
	if issueID == "" {
		return fmt.Errorf("gt done: crew branch %s names no bead; pass --bead <id>", branch)
	}

	rigName := strings.TrimSpace(getenv("GT_RIG"))
	if rigName == "" {
		rigName = beads.GetRigNameForPrefix(townRoot, beads.ExtractPrefix(issueID))
	}

	r := &doneRun{
		g:             g,
		cwd:           gitRoot,
		townRoot:      townRoot,
		rigName:       rigName,
		sender:        actor,
		branch:        branch,
		issueID:       issueID,
		defaultBranch: "main",
	}
	if rigName != "" {
		if rigCfg, err := rig.LoadRigConfig(filepath.Join(townRoot, rigName)); err == nil && rigCfg.DefaultBranch != "" {
			r.defaultBranch = rigCfg.DefaultBranch
		}
	}
	r.opts = doneOptions{target: doneTarget, preVerified: donePreVerified}
	r.deps = doneSubmitDeps{
		repo: g,
		source: func(id string) (*beads.Issue, beads.Client, error) {
			// Route from the town, not the worktree: a crew worktree's own
			// .beads may be the source repo's, not the town's.
			info, err := resolveSubmitSourceIssue(townRoot, id)
			if err != nil {
				return nil, nil, err
			}
			return info.Issue, info.BD, nil
		},
		localGate: doneLocalGate,
		sleep:     time.Sleep,
	}
	return submitCrewForLanding(r)
}

// submitCrewForLanding marks a crew branch ready to land. The branch must
// already be on origin at HEAD: crew push their own work, so gt done reads
// the tip back rather than pushing. It writes, in order, the submission
// comment, the READY TO LAND block and the gt:ready-to-land label, so a
// labeled bead always says what to land. It never lands anything.
func submitCrewForLanding(r *doneRun) error {
	if r.branch == r.defaultBranch || r.branch == "master" {
		return fmt.Errorf("cannot submit the %s/master branch for landing; commit on a crew branch and push it", r.defaultBranch)
	}
	if r.issueID == "" {
		return fmt.Errorf("crew branch %s names no bead; pass --bead <id>", r.branch)
	}
	repo := r.deps.repo
	workStatus, err := repo.CheckUncommittedWork()
	if err != nil {
		return fmt.Errorf("checking git status: %w", err)
	}
	if workStatus.HasUncommittedChanges && !workStatus.CleanExcludingRuntime() {
		return fmt.Errorf("cannot submit: uncommitted changes are not on the pushed branch\nCommit and push them first\nUncommitted: %s", workStatus.String())
	}

	issue, bd, err := r.deps.source(r.issueID)
	if err != nil {
		return fmt.Errorf("source issue validation failed: %w", err)
	}
	if bd == nil {
		return fmt.Errorf("no beads client for %s", r.issueID)
	}
	target, err := resolveDoneTarget(r, issue)
	if err != nil {
		return err
	}
	baseRef := repo.CleanBaseRef("origin", r.defaultBranch, target)
	fetchRemote := git.RemoteForRef(baseRef)
	if fetchRemote == "" {
		fetchRemote = "origin"
	}
	if err := repo.Fetch(fetchRemote); err != nil {
		return fmt.Errorf("fetching %s: %w", fetchRemote, err)
	}
	ahead, err := repo.CommitsAhead(baseRef, "HEAD")
	if err != nil {
		return fmt.Errorf("counting commits ahead of %s: %w", baseRef, err)
	}
	if ahead == 0 {
		return fmt.Errorf("nothing to land: %s has no commits ahead of %s", r.branch, baseRef)
	}

	head, err := repo.Rev("HEAD")
	if err != nil {
		return fmt.Errorf("resolving HEAD: %w", err)
	}
	if err := repo.VerifyPushedCommit("origin", r.branch, head); err != nil {
		return doneExit(doneExitPushUnverified,
			fmt.Sprintf("origin/%s is not at HEAD %s; push it first (git push origin HEAD:%s), then re-run gt done", r.branch, shortSHA(head), r.branch), err)
	}

	if r.opts.preVerified {
		style.PrintWarning("skipping the local gate (--pre-verified); the landing worker still runs make gate on the merged tree")
	} else if err := runDoneLocalGate(r, head); err != nil {
		return err
	}

	comment := fmt.Sprintf("Submitted for landing: %s @ %s onto %s", land.NoteField(r.branch), head, land.NoteField(target))
	if err := bd.AddComment(r.issueID, comment); err != nil {
		return doneExit(doneExitReadyFailed, fmt.Sprintf("could not record the submission on %s", r.issueID), err)
	}
	// Worker names a polecat seat whose intent record the landing worker
	// clears; a crew member has none, so it stays empty.
	work := land.Work{BeadID: r.issueID, Rig: r.rigName, Branch: r.branch, Head: head, Target: target}
	if err := markReadyToLand(bd, work); err != nil {
		return doneExit(doneExitReadyFailed, fmt.Sprintf("branch %s is on origin at %s but %s could not be marked ready to land", r.branch, shortSHA(head), r.issueID), err)
	}

	fmt.Printf("%s Submitted for landing\n", style.Bold.Render("✓"))
	fmt.Printf("  Bead:   %s\n", r.issueID)
	fmt.Printf("  Branch: %s @ %s\n", r.branch, shortSHA(head))
	fmt.Printf("  Target: %s\n", target)
	fmt.Printf("  Actor:  %s\n\n", r.sender)
	fmt.Printf("%s\n", style.Dim.Render("The daemon's landing worker merges it after gating the merged tree."))
	return nil
}

// firstNonEmpty is the first of values that is not blank, trimmed.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
