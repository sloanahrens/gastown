package daemon

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/constants"
	gtgit "github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	defaultCheckpointDogInterval = 10 * time.Minute
)

// CheckpointDogConfig holds configuration for the checkpoint_dog patrol.
type CheckpointDogConfig struct {
	// Enabled controls whether the checkpoint dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "10m").
	IntervalStr string `json:"interval,omitempty"`
}

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

	dec := evaluatePatrolDue(d.config.TownRoot, "checkpoint_dog", time.Time{}, time.Now(), checkpointDogInterval(d.patrolConfig))
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
// compactor_dog's SQL operations). The daemon pours a molecule for
// observability, then runs git commands via exec.Command.
func (d *Daemon) runCheckpointDog() {
	if !d.isPatrolActive("checkpoint_dog") {
		return
	}

	// Record that a cycle was attempted, regardless of outcome below — the
	// same "attempted" semantics as the ticker firing before gt-ima2/gt-gxpwc.
	defer func() {
		if err := savePatrolLastRun(d.config.TownRoot, "checkpoint_dog", time.Now()); err != nil {
			d.logger.Printf("checkpoint_dog: WARNING: cannot persist last-run time (%v) — "+
				"the next check may re-run sooner than expected", err)
		}
	}()

	d.logger.Printf("checkpoint_dog: starting cycle")

	mol := d.pourDogMolecule(constants.MolDogCheckpoint, nil)
	defer mol.close()

	rigs := d.getKnownRigs()
	totalScanned := 0
	totalCheckpointed := 0

	for _, rigName := range rigs {
		scanned, checkpointed := d.checkpointRigPolecats(rigName)
		totalScanned += scanned
		totalCheckpointed += checkpointed
	}

	mol.closeStep("scan")
	mol.closeStep("checkpoint")

	d.logger.Printf("checkpoint_dog: cycle complete — scanned %d worktrees, checkpointed %d",
		totalScanned, totalCheckpointed)
	mol.closeStep("report")
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
		sessionName := session.PolecatSessionName(session.PrefixFor(rigName), polecatName)
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
		workDir := resolveCheckpointWorkDir(polecatsDir, polecatName, rigName)
		if workDir == "" {
			continue // Neither layout has a usable .git — skip silently.
		}
		if d.checkpointWorktree(workDir, rigName, polecatName) {
			checkpointed++
		}
	}

	return scanned, checkpointed
}

// checkpointWorktree creates a WIP checkpoint commit for a single worktree.
// Returns true if a checkpoint was created.
func (d *Daemon) checkpointWorktree(workDir, rigName, polecatName string) bool {
	// Check git status (exclude runtime dirs from consideration)
	statusOut, err := runGitCmd(workDir, "status", "--porcelain")
	if err != nil {
		d.logger.Printf("checkpoint_dog: git status failed in %s/%s: %v", rigName, polecatName, err)
		return false
	}
	if strings.TrimSpace(statusOut) == "" {
		return false // Clean worktree
	}

	// Stage everything
	if _, err := runGitCmd(workDir, "add", "-A"); err != nil {
		d.logger.Printf("checkpoint_dog: git add -A failed in %s/%s: %v", rigName, polecatName, err)
		return false
	}

	// Unstage runtime/ephemeral artifacts using the same centralized policy as
	// gt done. Scanning staged paths catches tracked nested runtime dirs that
	// git add -A can restage despite ignore rules.
	stagedOut, err := runGitCmdRaw(workDir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached failed in %s/%s: %v", rigName, polecatName, err)
		return false
	}
	for _, pathspec := range gtgit.RuntimeArtifactPathspecs(splitNullSeparatedPaths(stagedOut)) {
		if _, err := runGitCmd(workDir, "reset", "HEAD", "--", pathspec); err != nil {
			d.logger.Printf("checkpoint_dog: git reset runtime artifact %q failed in %s/%s: %v", pathspec, rigName, polecatName, err)
			return false
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
	addedOut, err := runGitCmdRaw(workDir, "diff", "--cached", "--name-only", "--diff-filter=A", "-z")
	if err != nil {
		d.logger.Printf("checkpoint_dog: git diff --cached --diff-filter=A failed in %s/%s: %v", rigName, polecatName, err)
		return false
	}
	for _, path := range checkpoint.ThrowawayPaths(splitNullSeparatedPaths(addedOut)) {
		if _, err := runGitCmd(workDir, "reset", "HEAD", "--", path); err != nil {
			d.logger.Printf("checkpoint_dog: git reset throwaway path %q failed in %s/%s: %v", path, rigName, polecatName, err)
			return false
		}
		d.logger.Printf("checkpoint_dog: skipping throwaway path %q in %s/%s (left untracked)", path, rigName, polecatName)
	}

	// Unstage deletions of tracked files. A checkpoint should preserve work
	// (additions + modifications), never commit deletions of tracked files.
	// This prevents the bug where a polecat's working tree has a missing
	// tracked file and the checkpoint commits the deletion (gt-pvx fix).
	if delOut, err := runGitCmd(workDir, "diff", "--cached", "--name-only", "--diff-filter=D"); err == nil {
		if dels := strings.TrimSpace(delOut); dels != "" {
			for _, f := range strings.Split(dels, "\n") {
				if f != "" {
					_, _ = runGitCmd(workDir, "reset", "HEAD", "--", f)
				}
			}
		}
	}

	// Check if anything is staged after exclusions
	diffOut, err := runGitCmd(workDir, "diff", "--cached", "--quiet")
	if err == nil && strings.TrimSpace(diffOut) == "" {
		// --quiet exits 0 if no diff → nothing staged
		return false
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
		return false
	}

	// Commit the checkpoint
	if _, err := runGitCmd(workDir, "commit", "-m", "WIP: checkpoint (auto)"); err != nil {
		d.logger.Printf("checkpoint_dog: git commit failed in %s/%s: %v", rigName, polecatName, err)
		return false
	}

	d.logger.Printf("checkpoint_dog: created WIP checkpoint in %s/%s", rigName, polecatName)
	return true
}

// checkpointRevertTarget is the ref checkpointRevertGuard compares the
// pending checkpoint tree against. Hardcoded rather than read from rig
// config: every rig in this town bases its polecat branches on main, the same
// assumption gt done's own revert guard makes (internal/cmd/done.go).
const checkpointRevertTarget = "origin/main"

// checkpointRevertGuard reports whether the currently staged index would, if
// committed, revert content already merged to checkpointRevertTarget (gt-2bp8).
//
// It writes the index to a tree object without committing — the WIP commit
// does not exist yet to inspect — and runs the same content-based revert
// detection gt done applies to its own final commit (git.DetectRevertedMerges),
// against that pending tree instead of HEAD's.
//
// When checkpointRevertTarget cannot even be resolved locally (no origin
// remote, or it was never fetched into this worktree) there is no known
// merged baseline to compare against, so the guard passes rather than blocks
// every checkpoint in an environment that simply lacks the ref. Once the
// target does resolve, any further failure to complete the check fails
// closed: a check that could not run must not read as a check that passed.
func (d *Daemon) checkpointRevertGuard(workDir, rigName, polecatName string) (blocked bool, reason string) {
	if _, err := runGitCmd(workDir, "rev-parse", "--verify", "--quiet", checkpointRevertTarget); err != nil {
		return false, ""
	}

	pendingTree, err := runGitCmd(workDir, "write-tree")
	if err != nil {
		return true, fmt.Sprintf("could not inspect the pending checkpoint tree (git write-tree failed): %v", err)
	}

	found, err := gtgit.DetectRevertedMerges(gtgit.NewGit(workDir), checkpointRevertTarget, pendingTree)
	if err != nil {
		return true, fmt.Sprintf("could not verify the pending checkpoint against %s: %v", checkpointRevertTarget, err)
	}
	if len(found) == 0 {
		return false, ""
	}

	var paths []string
	for _, f := range found {
		paths = append(paths, f.Paths...)
	}
	reason = fmt.Sprintf(
		"staged content reverts %d commit(s) already merged to %s, across %d path(s): %s",
		len(found), checkpointRevertTarget, len(paths), strings.Join(paths, ", "))

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

// isGitWorktree reports whether the given directory is the root of a git
// worktree (has its own `.git` file or directory). Used to guard checkpoint
// commits against the "wrong-dir" failure mode where git operations in a
// non-worktree directory walk up the filesystem tree and commit on the
// parent workspace's branch.
func isGitWorktree(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// resolveCheckpointWorkDir picks the actual git-worktree directory for a
// polecat, supporting both the new nested layout (polecats/<name>/<rigName>/)
// and the legacy flat layout (polecats/<name>/) that polecat.Manager still
// recognizes for backward compatibility. Returns "" if neither candidate is
// a git worktree, in which case the caller MUST skip the polecat — never
// fall back to a parent directory, since git would walk up to the top-level
// workspace's .git and commit on the wrong branch (this is the bug this
// helper exists to prevent).
func resolveCheckpointWorkDir(polecatsDir, polecatName, rigName string) string {
	nested := filepath.Join(polecatsDir, polecatName, rigName)
	if isGitWorktree(nested) {
		return nested
	}
	flat := filepath.Join(polecatsDir, polecatName)
	if isGitWorktree(flat) {
		return flat
	}
	return ""
}

// runGitCmd executes a git command in the given directory and returns stdout.
func runGitCmd(workDir string, args ...string) (string, error) {
	out, err := runGitCmdRaw(workDir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func runGitCmdRaw(workDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	util.SetDetachedProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%s: %s", err, errMsg)
		}
		return "", err
	}

	return stdout.String(), nil
}

func splitNullSeparatedPaths(out string) []string {
	if out == "" {
		return nil
	}
	parts := strings.Split(out, "\x00")
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}
