package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/checkpoint"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/revertwaiver"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	defaultCheckpointDogInterval = 10 * time.Minute
)

// checkpointDogInterval returns the configured interval, or the default (10m).
func checkpointDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.CheckpointDog != nil {
		if config.Patrols.CheckpointDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.CheckpointDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultCheckpointDogInterval
}

// triggerCheckpointDog runs a checkpoint_dog cycle when the patrol is due, on
// its own goroutine.
//
// The ticker that drives this call is a check cadence, not a run cadence: an
// in-process ticker resets its countdown on every daemon restart, so due-ness
// is instead decided from the persisted last-run time in
// daemon/patrol_last_run.json, which survives a restart (gt-ima2, gt-gxpwc).
//
// Dispatched onto its own goroutine, like the gt-ima2 fix for compactor_dog:
// a cycle shells out to git in every active polecat worktree across every
// rig, and running it inline would stall every other tick behind it.
func (d *Daemon) triggerCheckpointDog() {
	if !d.isPatrolActive("checkpoint_dog") {
		return
	}

	dec := evaluatePatrolDue(d.config.TownRoot, "checkpoint_dog", d.checkpointDogRun.last(), time.Now(), checkpointDogInterval(d.patrolConfig))
	if !dec.due {
		d.logger.Printf("checkpoint_dog: not due — %s", dec.note)
		return
	}

	if !d.checkpointDogRunning.CompareAndSwap(false, true) {
		d.logger.Printf("checkpoint_dog: previous cycle still running — skipping this check")
		return
	}

	if dec.warn != "" {
		d.logger.Printf("checkpoint_dog: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("checkpoint_dog: due — %s", dec.note)
	}

	go func() {
		defer d.checkpointDogRunning.Store(false)
		d.runCheckpointDog()
	}()
}

// runCheckpointDog auto-commits WIP changes in active polecat worktrees.
// This protects against data loss when sessions crash or hit context limits.
//
// ## ZFC Exemption
// The checkpoint dog executes git operations directly (same pattern as
// compactor_dog's SQL operations), via exec.Command.
func (d *Daemon) runCheckpointDog() {
	if !d.isPatrolActive("checkpoint_dog") {
		return
	}

	// Record that a cycle was attempted, regardless of outcome below — the
	// same "attempted" semantics as the ticker firing before gt-ima2/gt-gxpwc.
	defer func() {
		d.recordPatrolRun(&d.checkpointDogRun, "checkpoint_dog", time.Now())
	}()

	d.logger.Printf("checkpoint_dog: starting cycle")

	cycle := d.startDogCycle("checkpoint_dog")
	defer cycle.close()

	rigs := d.getKnownRigs()
	totalScanned := 0
	totalCheckpointed := 0

	for _, rigName := range rigs {
		scanned, checkpointed := d.checkpointRigPolecats(rigName)
		totalScanned += scanned
		totalCheckpointed += checkpointed
	}

	cycle.closeStep("scan")
	cycle.closeStep("checkpoint")

	d.logger.Printf("checkpoint_dog: cycle complete — scanned %d worktrees, checkpointed %d",
		totalScanned, totalCheckpointed)
	cycle.closeStep("report")
}

// checkpointRigPolecats checkpoints dirty polecat worktrees in a single rig.
// Returns (scanned, checkpointed) counts.
func (d *Daemon) checkpointRigPolecats(rigName string) (int, int) {
	polecatsDir := filepath.Join(d.config.TownRoot, rigName, "polecats")
	polecats, err := listPolecatWorktrees(polecatsDir)
	if err != nil {
		return 0, 0
	}

	scanned := 0
	checkpointed := 0

	for _, polecatName := range polecats {
		scanned++

		// Check if tmux session is alive — only checkpoint active sessions.
		// Dead sessions can't benefit from checkpoints.
		sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rigName), polecatName)
		alive, err := d.tmux.HasSession(sessionName)
		if err != nil {
			d.logger.Printf("checkpoint_dog: error checking session %s: %v", sessionName, err)
			continue
		}
		if !alive {
			continue
		}

		// Polecat layout: prefer <polecatsDir>/<name>/<rigName>/ (the new
		// nested layout where the outer <name>/ dir is a container with
		// per-polecat scaffolding and the inner dir is the actual git
		// worktree). Fall back to <polecatsDir>/<name>/ for the legacy
		// flat layout still supported by polecat.Manager. Both candidates
		// must contain `.git` — never fall back to a parent dir, since
		// the original bug here was exactly that: an empty <name>/
		// container caused git to walk up to the top-level workspace's
		// .git and commit "WIP: checkpoint (auto)" on the workspace's
		// branch (usually main) instead of the polecat's branch.
		// (gt-checkpoint-workdir fix.)
		workDir := resolvePolecatWorktree(polecatsDir, polecatName, rigName)
		if workDir == "" {
			continue // Neither layout has a usable .git — skip silently.
		}
		if d.checkpointWorktree(workDir, rigName, polecatName) {
			checkpointed++
		}
	}

	return scanned, checkpointed
}

// checkpointOutcome is how one checkpoint attempt ended.
type checkpointOutcome int

const (
	// checkpointClean: no work to keep, so none is at risk.
	checkpointClean checkpointOutcome = iota
	// checkpointCreated: the work is in a WIP commit on the branch.
	checkpointCreated
	// checkpointRefused: git failed or the worktree is mid-merge or mid-rebase,
	// so the work may exist only as dirty files.
	checkpointRefused
)

// checkpointWorktree creates a WIP checkpoint commit for a single worktree.
// Returns true if a checkpoint was created.
func (d *Daemon) checkpointWorktree(workDir, rigName, polecatName string) bool {
	return d.checkpointWorktreeOutcome(workDir, rigName, polecatName) == checkpointCreated
}

// checkpointWorktreeOutcome is checkpointWorktree with a clean worktree told
// apart from a refused checkpoint; the overrun guard retries only the latter.
func (d *Daemon) checkpointWorktreeOutcome(workDir, rigName, polecatName string) (outcome checkpointOutcome) {
	g := d.gitAt(workDir)

	// Check git status (exclude runtime dirs from consideration)
	status, err := g.Status()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git status failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}

	// A worktree mid-merge or mid-rebase, or holding unmerged index entries,
	// is left alone (gt-kbp1t): `git add -A` would stage the conflict markers
	// or the half-applied operation, and the WIP commit would carry literal
	// `<<<<<<<` lines onto the polecat's branch.
	if reason := checkpointBlockedReason(g, status); reason != "" {
		d.logger.Printf("checkpoint_dog: skipping %s/%s: %s", rigName, polecatName, reason)
		return checkpointRefused
	}

	if status.Clean {
		return checkpointClean
	}

	// Whatever `git add -A` staged stays in the index when the checkpoint
	// ends without a commit, and the polecat's next `git commit` would record
	// it: the content the revert guard refused, the deletions and conflict
	// leftovers it did not. Every outcome but a created checkpoint unstages
	// what the checkpoint itself staged. The files stay as the polecat left
	// them; only the index is reset.
	//
	// A path the polecat had already staged is not the checkpoint's to
	// unstage, so the paths staged before `git add -A` are read first and left
	// alone. `git add -A` restages such a path from the working tree, so one
	// the polecat staged and then edited stays staged at its newer content.
	before, err := g.StagedChanges()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}
	ownStaging := stagedPaths(before, 0)
	defer func() {
		if outcome == checkpointCreated {
			return
		}
		if err := unstageCheckpointed(g, ownStaging); err != nil {
			d.logger.Printf("checkpoint_dog: git reset after refused checkpoint failed in %s/%s: %v", rigName, polecatName, err)
		}
	}()

	// Stage everything
	if err := g.Add("-A"); err != nil {
		d.logger.Printf("checkpoint_dog: git add -A failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}

	// Unstage runtime/ephemeral artifacts using the same centralized policy as
	// gt done. Scanning staged paths catches tracked nested runtime dirs that
	// git add -A can restage despite ignore rules.
	//
	// The staged list holds only paths that differ from HEAD, so g supplies
	// the tracked source beside them: a fixture under a coverage/ or .cache/
	// directory that holds unchanged tracked source is work, not output
	// (gt-za3u1).
	staged, err := g.StagedChanges()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}
	for _, pathspec := range g.RuntimeArtifactPathspecs(stagedPaths(staged, 0)) {
		if err := g.ResetFiles(pathspec); err != nil {
			d.logger.Printf("checkpoint_dog: git reset runtime artifact %q failed in %s/%s: %v", pathspec, rigName, polecatName, err)
			return checkpointRefused
		}
	}

	// Unstage throwaway files the polecat left in the worktree (gt-ozo4). The
	// `git add -A` above sweeps them in, and gt done submits HEAD's tree, so a
	// scratch file that existed between two dog cycles lands on main even if
	// the polecat deletes it afterwards.
	//
	// Only paths this checkpoint would ADD are excluded — a modification to a
	// file git already tracks is real work whatever its name. Excluded files
	// are left untracked where the polecat left them and named in the log: a
	// polecat that meant to keep one now has no checkpoint for it, and only the
	// log says why.
	//
	// StagedChanges runs without rename detection: `add -A` stages a tracked
	// file moved to a scratch name as a delete plus an add, and a rename entry
	// would hide the add.
	staged, err = g.StagedChanges()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached --diff-filter=A failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}
	for _, path := range checkpoint.ThrowawayPaths(stagedPaths(staged, 'A')) {
		if err := g.ResetFiles(path); err != nil {
			d.logger.Printf("checkpoint_dog: git reset throwaway path %q failed in %s/%s: %v", path, rigName, polecatName, err)
			return checkpointRefused
		}
		d.logger.Printf("checkpoint_dog: skipping throwaway path %q in %s/%s (left untracked)", path, rigName, polecatName)
	}

	// Unstage deletions of tracked files. A checkpoint should preserve work
	// (additions + modifications), never commit deletions of tracked files.
	// This prevents the bug where a polecat's working tree has a missing
	// tracked file and the checkpoint commits the deletion (gt-pvx fix).
	//
	// Both the listing and each reset must succeed: a deletion still staged
	// here is recorded by the commit below, so a failure to read or to unstage
	// is a reason to skip the checkpoint, not to go on (gt-2czgm).
	staged, err = g.StagedChanges()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}
	for _, f := range stagedPaths(staged, 'D') {
		if err := g.ResetFiles(f); err != nil {
			d.logger.Printf("checkpoint_dog: git reset deleted path %q failed in %s/%s: %v", f, rigName, polecatName, err)
			return checkpointRefused
		}
	}

	// Check if anything is staged after exclusions. A listing that cannot be
	// read must not read as "nothing to commit" (gt-2czgm).
	staged, err = g.StagedChanges()
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}
	if len(staged) == 0 {
		return checkpointClean
	}

	// Refuse to checkpoint a revert of already-merged work (gt-2bp8): a
	// shared-worktree reuse or reset can leave the working tree holding stale
	// pre-merge content for paths this session never touched, and `git add -A`
	// stages that content same as real work. Committing it would silently
	// revert other polecats' already-merged work under a generic "WIP:
	// checkpoint (auto)" subject. Checked here, after staging, because the
	// check needs the tree the pending commit would actually record.
	if blocked, reason := d.checkpointRevertGuard(workDir, rigName, polecatName); blocked {
		d.logger.Printf("checkpoint_dog: refusing checkpoint in %s/%s: %s", rigName, polecatName, reason)
		return checkpointRefused
	}

	// Commit the checkpoint
	if err := g.Commit("WIP: checkpoint (auto)"); err != nil {
		d.logger.Printf("checkpoint_dog: git commit failed in %s/%s: %v", rigName, polecatName, err)
		return checkpointRefused
	}

	d.logger.Printf("checkpoint_dog: created WIP checkpoint in %s/%s", rigName, polecatName)
	return checkpointCreated
}

// checkpointBlockedReason reports why a worktree must not be checkpointed, or
// "" when a checkpoint is safe: `git add -A` plus a commit in these git states
// would record a half-finished operation as work (gt-kbp1t).
//
//   - unmerged index entries: a merge or rebase stopped on conflicts. The
//     staged tree would hold the conflict markers themselves.
//   - MERGE_HEAD in the worktree's git directory: a merge stopped before it
//     was committed, conflict markers or not.
//   - rebase-merge/ or rebase-apply/: a rebase (interactive or not) in
//     progress, whose half-replayed commits are not work to keep.
//
// Detection reads git state, never file contents, so a file that merely
// contains a marker is ordinary work. Nothing here resolves or alters the
// conflict.
//
// A git directory that cannot be located blocks the checkpoint: a safety
// check that could not run must not read as a check that passed.
func checkpointBlockedReason(g daemonGit, status *gtgit.GitStatus) string {
	if len(status.Unmerged) > 0 {
		return fmt.Sprintf("unmerged paths: %s", strings.Join(status.Unmerged, ", "))
	}

	gitDir, err := g.GitDir()
	if err != nil {
		return fmt.Sprintf("cannot locate the worktree's git directory: %v", err)
	}
	for _, op := range []struct{ marker, name string }{
		{"MERGE_HEAD", "merge"},
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, op.marker)); err == nil {
			return op.name + " in progress"
		}
	}
	return ""
}

// unstageCheckpointed resets the index to HEAD for every path except those in
// keep, the paths the polecat had staged before the checkpoint began. With
// nothing to keep it resets the whole index.
func unstageCheckpointed(g daemonGit, keep []string) error {
	if len(keep) == 0 {
		return g.ResetFiles(".")
	}
	staged, err := g.StagedChanges()
	if err != nil {
		return err
	}
	var paths []string
	for _, p := range stagedPaths(staged, 0) {
		if !slices.Contains(keep, p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return g.ResetFiles(paths...)
}

// stagedPaths returns the paths of staged changes with the given status, or
// of all of them when status is 0.
func stagedPaths(changes []gtgit.StagedChange, status byte) []string {
	var paths []string
	for _, c := range changes {
		if status == 0 || c.Status == status {
			paths = append(paths, c.Path)
		}
	}
	return paths
}

// checkpointRevertTargetDefault is the branch name assumed when the rig's
// config cannot be read — most rigs in this town default to main.
const checkpointRevertTargetDefault = "main"

// checkpointRevertTarget is the ref checkpointRevertGuard compares the
// pending checkpoint tree against. It is NOT always origin/main: gt done's
// own revert guard resolves its base with git.CleanDefaultBranchBaseRef,
// which reads the rig's configured default branch and, in a fork-backed rig
// (origin is a fork with a distinct upstream remote), targets
// upstream/<default> instead of origin/<default> — origin/main would be the
// fork's stale copy there, not the shared history the guard needs to protect
// (internal/done's submit path, CleanBaseRef). This mirrors
// that same resolution so the two guards agree on what "already merged" means.
func (d *Daemon) checkpointRevertTarget(workDir, rigName string) string {
	defaultBranch := checkpointRevertTargetDefault
	if d.config != nil && d.config.TownRoot != "" {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		// A config.json that does not decode is reported once rather than
		// silently guarding against the wrong branch (gt-8xk9k).
		rigCfg, cfgErr := rig.LoadRigConfigIfPresent(rigPath)
		if cfgErr != nil {
			rig.WarnRigConfigOnce(rigPath, cfgErr)
		}
		if rigCfg != nil && rigCfg.DefaultBranch != "" {
			defaultBranch = rigCfg.DefaultBranch
		}
	}
	return d.gitAt(workDir).CleanDefaultBranchBaseRef("origin", defaultBranch)
}

// checkpointRevertGuard reports whether the currently staged index would, if
// committed, revert content already merged to the rig's clean base ref
// (gt-2bp8).
//
// It writes the index to a tree object without committing — the WIP commit
// does not exist yet to inspect — and runs the same content-based revert
// detection gt done applies to its own final commit (git.DetectRevertedMerges),
// against that pending tree instead of HEAD's.
//
// When the target ref cannot even be resolved locally (no origin remote, or
// it was never fetched into this worktree) there is no known merged baseline
// to compare against, so the guard passes rather than blocks every checkpoint
// in an environment that simply lacks the ref. Once the target does resolve,
// any further failure to complete the check fails closed: a check that could
// not run must not read as a check that passed.
func (d *Daemon) checkpointRevertGuard(workDir, rigName, polecatName string) (blocked bool, reason string) {
	target := d.checkpointRevertTarget(workDir, rigName)
	g := d.gitAt(workDir)
	if _, err := g.Rev(target); err != nil {
		return false, ""
	}

	pendingTree, err := g.WriteTree()
	if err != nil {
		return true, fmt.Sprintf("could not inspect the pending checkpoint tree (git write-tree failed): %v", err)
	}

	report, err := gtgit.DetectRevertedMerges(g, target, pendingTree)
	if err != nil {
		return true, fmt.Sprintf("could not verify the pending checkpoint against %s: %v", target, err)
	}
	// Relocations are not a reason to block a checkpoint: the code they move
	// survives in the package, so the checkpoint cannot delete merged work
	// (gt-x748o).
	if len(report.Reverted) == 0 {
		return false, ""
	}

	// A seat on a deletes-by-spec bead is directed to delete what merged
	// commits added, so its deletions are work, not a stale tree. The waiver is
	// the one gt done applies (the label plus a description naming every
	// reverted path), so the dog and gt done agree on a directed deletion. The
	// dog stands down without an escalation.
	//
	// Reading the bead is the slow part of this guard — a bd list and a show,
	// each bounded by workBeadReadTimeout — and the polecat goes on working
	// while they run, so the waiver judges the index as it is after those
	// reads, not the tree read before them: a commit that landed in the
	// meantime is decided by what would be committed now, and a tree that
	// changed under the reads does not inherit a waiver for a different
	// deletion (gt-0h4i0).
	reverted := report.Reverted
	if issue := d.checkpointWaiverBead(workDir, rigName, polecatName); issue != nil {
		reverts, err := d.revertsNow(g, target)
		if err != nil {
			return true, fmt.Sprintf("could not re-inspect the pending checkpoint after reading the deletes-by-spec bead: %v", err)
		}
		if len(reverts) == 0 {
			// The tree stopped reverting while the bead was read: nothing is
			// left to waive and nothing to refuse.
			return false, ""
		}
		// The refusal below names what is staged now, not the set read
		// before the bd reads.
		reverted = reverts
		if note, waived := revertwaiver.Note(issue, reverts); waived {
			d.logger.Printf("checkpoint_dog: %s/%s: %s", rigName, polecatName, note)
			return false, ""
		}
	}

	var paths []string
	for _, f := range reverted {
		paths = append(paths, f.Paths...)
	}
	reason = fmt.Sprintf(
		"staged content reverts %d commit(s) already merged to %s, across %d path(s): %s",
		len(reverted), target, len(paths), strings.Join(paths, ", "))

	if alert := d.checkpointRevertAlert; alert != nil {
		key := fmt.Sprintf("checkpoint_dog:revert-guard:%s/%s", rigName, polecatName)
		alert(key, "checkpoint_dog", fmt.Sprintf(
			"checkpoint_dog refused a WIP checkpoint in %s/%s: %s\n"+
				"The worktree likely still has the reverted content staged/unstaged; "+
				"it was left uncommitted rather than landed on the polecat's branch.",
			rigName, polecatName, reason))
	}

	return true, reason
}

// checkpointWaiverBead reads the bead whose direction a checkpoint's deletions
// could follow: the one checkpointedWorkBead names. A bead that cannot be
// found or read waives nothing — nil — so the refusal stands, because a check
// that could not run must not read as a check that passed. The caller decides
// the waiver against the tree staged after this read (revertwaiver.Note).
func (d *Daemon) checkpointWaiverBead(workDir, rigName, polecatName string) *beads.Issue {
	if d.config == nil || d.config.TownRoot == "" {
		return nil
	}
	id, err := d.checkpointedWorkBead(workDir, rigName, polecatName)
	if err != nil || id == "" {
		return nil
	}
	issue, err := d.workBeads(d.workBeadsEnv(rigName), workBeadReadTimeout).Show(id)
	if err != nil {
		return nil
	}
	return issue
}

// revertsNow re-reads the index the way checkpointRevertGuard starts: what the
// tree staged right now would revert of target. It runs after the waiver's bd
// reads, so the guard decides on the deletion a commit made now would record.
func (d *Daemon) revertsNow(g daemonGit, target string) ([]gtgit.RevertedMerge, error) {
	tree, err := g.WriteTree()
	if err != nil {
		return nil, fmt.Errorf("git write-tree: %w", err)
	}
	report, err := gtgit.DetectRevertedMerges(g, target, tree)
	if err != nil {
		return nil, fmt.Errorf("verifying the pending checkpoint against %s: %w", target, err)
	}
	return report.Reverted, nil
}

// checkpointedWorkBead returns the work bead that directs this checkpoint's
// deletions: the one gt done would submit the same branch under. That is the
// issue encoded in the seat's polecat branch (gt-rp83g), except on stale-branch
// reuse: a redispatched seat hooked to gt-NEW and checked out on its old gt-OLD
// branch is submitted under gt-NEW, so the waiver is read from gt-NEW here too
// (gt-aupx1). The seat's assigned active bead answers alone only when the
// branch names no issue for this seat — a legacy branch, a detached HEAD, or a
// branch another seat wrote — and only when there is exactly one of them.
func (d *Daemon) checkpointedWorkBead(workDir, rigName, polecatName string) (string, error) {
	assignee := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
	if branch, err := d.gitAt(workDir).CurrentBranch(); err == nil {
		if meta, ok := polecat.ParseBranchName(branch); ok && meta.Issue != "" && meta.Polecat == polecatName {
			return d.resolveStaleBranchBead(rigName, assignee, meta.Issue)
		}
	}
	return d.soleActiveWorkBead(rigName, assignee)
}

// soleActiveWorkBead returns the one work bead assigned to the seat with status
// hooked or in_progress, and "" when the seat holds none or more than one.
// A polecat_branch_template the package cannot parse leaves the seat's own
// assignments as the only source, and a seat holding two active beads cannot
// say which of them the deletions belong to: reading the waiver from an
// arbitrary one would let a checkpoint through on the wrong bead's authority
// (gt-0h4i0). A list that cannot be read is an error, never "none", because
// the failed status may be the one holding the other bead.
func (d *Daemon) soleActiveWorkBead(rigName, assignee string) (string, error) {
	ids, err := d.assignedActiveWorkBeads(rigName, assignee, 2)
	switch {
	case len(ids) > 1:
		return "", nil
	case err != nil:
		return "", err
	case len(ids) == 1:
		return ids[0], nil
	}
	return "", nil
}

// resolveStaleBranchBead applies gt done's stale-branch guard (hq-l0fj) to the
// issue a branch names, sharing that guard's selection with gt done
// (polecat.SelectAssignedIssue). The branch's issue stands when the seat holds
// it or a parent of it (a subtask branch gt-abc.1 under hooked gt-abc).
// Otherwise the seat's single active assignment replaces it.
//
// A status list that cannot be read names no bead even when the other status
// answered: the failed status may be the one holding the branch's issue, so a
// partial list is not a check that ran (gt-sl0np). Several assignments are
// equally unknowable. Either way the caller waives nothing and the checkpoint
// refusal stands.
func (d *Daemon) resolveStaleBranchBead(rigName, assignee, branchIssue string) (string, error) {
	var assigned []string
	for _, status := range []string{"hooked", "in_progress"} {
		issues, err := d.assignedWork(rigName, assignee, status)
		if err != nil {
			return "", fmt.Errorf("bd list --status=%s: %w", status, err)
		}
		for _, issue := range issues {
			if issue != nil && issue.ID != "" && !slices.Contains(assigned, issue.ID) {
				assigned = append(assigned, issue.ID)
			}
		}
		if len(assigned) > 0 {
			break
		}
	}
	selected, ambiguous := polecat.SelectAssignedIssue(branchIssue, assigned)
	if ambiguous {
		return "", fmt.Errorf("branch issue %s is not held and %s has %d active assignments", branchIssue, assignee, len(assigned))
	}
	if selected == "" {
		return branchIssue, nil
	}
	return selected, nil
}

// isGitWorktree reports whether the given directory is the root of a git
// worktree (has its own `.git` file or directory). Used to guard checkpoint
// commits against the "wrong-dir" failure mode where git operations in a
// non-worktree directory walk up the filesystem tree and commit on the
// parent workspace's branch.
func isGitWorktree(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// resolvePolecatWorktree picks the actual git-worktree directory for a
// polecat, supporting both the new nested layout (polecats/<name>/<rigName>/)
// and the legacy flat layout (polecats/<name>/) that polecat.Manager still
// recognizes for backward compatibility. Returns "" if neither candidate is
// a git worktree, in which case the caller MUST skip the polecat — never
// fall back to a parent directory, since git would walk up to the top-level
// workspace's .git and commit on the wrong branch (this is the bug this
// helper exists to prevent).
func resolvePolecatWorktree(polecatsDir, polecatName, rigName string) string {
	for _, dir := range polecatWorktreeCandidates(polecatsDir, polecatName, rigName) {
		if isGitWorktree(dir) {
			return dir
		}
	}
	return ""
}

// polecatWorktreeCandidates lists the directories resolvePolecatWorktree
// considers, in the order it prefers them. A caller with no worktree to
// resolve names these in its error, so the layout rule stays here rather than
// being spelled out a second time at the call site.
func polecatWorktreeCandidates(polecatsDir, polecatName, rigName string) []string {
	return []string{
		filepath.Join(polecatsDir, polecatName, rigName),
		filepath.Join(polecatsDir, polecatName),
	}
}
