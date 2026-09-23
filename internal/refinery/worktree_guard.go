package refinery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// ErrExternallyDirtyWorktree reports uncommitted or staged changes in the
// Refinery's own rig worktree that this process did not create. Callers must
// treat it as fatal for the merge: a gate reads whatever is staged there, so a
// tree an external writer has touched is no longer evidence about the MR.
var ErrExternallyDirtyWorktree = errors.New("refinery worktree carries changes the refinery did not create")

// beginWorktreeOwnedMerge guards and then claims the merge worktree for one
// operation. Every path that is about to mutate the tree — doMerge, and the
// multi-MR batch that stages its rebase stack itself — must call it before its
// first change, and run the returned cleanup when it is done.
//
// The returned error is ErrExternallyDirtyWorktree when the tree is an external
// writer's; callers surface that as their operation's failure without touching
// the MR, since the MR is not what is wrong.
func (e *Engineer) beginWorktreeOwnedMerge(stage string) (func(), error) {
	if err := e.assertWorktreeNotExternallyDirty(stage); err != nil {
		return nil, err
	}
	// Record that the Refinery owns this tree: residue the operation leaves
	// behind (a crash between squash and push, say) must read as the
	// Refinery's own and be restored, not as an external writer's.
	if err := e.markWorktreeInFlight(); err != nil {
		return nil, err
	}
	return e.clearWorktreeInFlightWhenClean, nil
}

// dirtyWorktreeReportInterval bounds how often a blocked merge re-escalates.
// The worktree stays dirty until a human clears it, so every cycle produces the
// same refusal; the interval keeps that from flooding the witness while still
// re-reporting a blockage nobody has picked up.
const dirtyWorktreeReportInterval = 30 * time.Minute

// reportExternallyDirtyWorktree escalates a refused merge to the rig witness,
// at most once per dirtyWorktreeReportInterval. A blocked merge queue needs a
// human, and nothing else in the town observes this: the refusal is not a
// verdict about any MR, so the MR-side notification paths stay silent.
func (e *Engineer) reportExternallyDirtyWorktree(detail string) {
	now := time.Now()
	last := e.dirtyWorktreeReportedAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < dirtyWorktreeReportInterval {
		return
	}
	if !e.dirtyWorktreeReportedAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	e.escalateToWitness(fmt.Sprintf(
		"REFINERY_WORKTREE_DIRTY: every merge is refused until the refinery rig worktree is clean — %s: %s",
		e.workDir, firstLine(detail)))
}

// firstLine collapses an escalation's detail to its headline; the full remedy
// is in the engineer's log next to the refusal.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// worktreeInFlightMarker names the file, inside this worktree's git directory,
// that records "the Refinery is mid-merge here". It is what separates a crashed
// run's own residue from an external writer's work, and the gate needs that
// distinction: residue is restored (gt-032w, gt-u093), while an external edit
// blocks the merge until a human moves it.
//
// The git directory is the right home for it: `git reset --hard` and
// `git clean` both leave it alone, so the marker outlives exactly the residue
// it exists to explain.
const worktreeInFlightMarker = "gt-refinery-worktree-inflight"

// maxReportedDirtyPaths bounds how many offending paths a refusal names before
// summarizing the rest; the operator needs a starting point, not a transcript.
const maxReportedDirtyPaths = 10

// assertWorktreeNotExternallyDirty refuses a merge whose staging tree already
// carries changes, unless the in-flight marker attributes them to this
// worktree's own interrupted merge.
//
// It runs before the first mutating step of a merge — restoreTargetToOrigin's
// reset would otherwise discard an external writer's uncommitted work without
// ever showing it to anyone — and it fails closed: a status it cannot read, and
// a marker it cannot read, both refuse.
//
// Only changes to files git tracks refuse. An untracked file cannot be the
// merge's own residue (a merge only rewrites tracked files), so treating it as
// a refusal would stop the queue for good the first time any tool left one
// behind — the failure mode where a guard that cannot be satisfied protects
// nothing. Untracked files are reported instead.
func (e *Engineer) assertWorktreeNotExternallyDirty(stage string) error {
	// Submodules are excluded deliberately: a superproject reads a submodule
	// as modified whenever the commit checked out inside it differs from the
	// recorded gitlink, which is ordinary submodule state rather than an edit
	// to this worktree. doMerge pushes submodule pointers on its own path.
	status, err := e.git.StatusIgnoringSubmodules()
	if err != nil {
		return fmt.Errorf("read refinery worktree status before %s: %w", stage, err)
	}

	if len(status.Untracked) > 0 {
		_, _ = fmt.Fprintf(e.output, "[Engineer] WARNING: %d untracked file(s) in the refinery worktree — a gate may compile or read them: %s\n",
			len(status.Untracked), summarizePaths(status.Untracked))
	}

	if !hasTrackedChanges(status) {
		// Nothing tracked is at risk, so a marker left by an operation that
		// finished is stale. Clearing it here is what keeps the marker
		// meaningful: for as long as it exists, the next tracked dirt reads as
		// residue and is restored without a human.
		if e.worktreeInFlight() {
			if err := e.clearWorktreeInFlight(); err != nil {
				return fmt.Errorf("clear stale worktree in-flight marker before %s: %w", stage, err)
			}
		}
		return nil
	}

	if e.worktreeInFlight() {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Refinery worktree carries %s from an interrupted merge — restoring it before %s\n",
			describeTrackedChanges(status), stage)
		return nil
	}

	return fmt.Errorf("%w: %s\n%s", ErrExternallyDirtyWorktree,
		describeTrackedChanges(status), externallyDirtyRemedy(e.workDir, stage))
}

// hasTrackedChanges reports whether git tracks any file whose content or index
// entry changed. Untracked files are excluded on purpose — see
// assertWorktreeNotExternallyDirty.
func hasTrackedChanges(status *git.GitStatus) bool {
	if status == nil {
		// An unreadable status is treated as dirty, so callers refuse.
		return true
	}
	return len(status.Modified) > 0 || len(status.Added) > 0 ||
		len(status.Deleted) > 0 || len(status.Unmerged) > 0
}

// markWorktreeInFlight records that this process is about to mutate the merge
// worktree, so residue it leaves behind is attributed to the Refinery rather
// than to an external writer. It fails closed — a merge that cannot record
// provenance would turn its own crash into a permanent refusal.
func (e *Engineer) markWorktreeInFlight() error {
	path, err := e.worktreeInFlightPath()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("refinery merge in flight\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// clearWorktreeInFlight drops the marker once the worktree is left clean.
func (e *Engineer) clearWorktreeInFlight() error {
	path, err := e.worktreeInFlightPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// clearWorktreeInFlightWhenClean drops the in-flight marker when a merge left
// no tracked change behind, so a marker never outlives the operation that set
// it. doMerge defers it: a marker that survives on an otherwise-clean tree
// would let a later external write read as the Refinery's own residue and be
// reset away without anyone seeing it. A worktree left with tracked dirt keeps
// its marker on purpose — that is what tells the next run the dirt is its own.
func (e *Engineer) clearWorktreeInFlightWhenClean() {
	if !e.worktreeInFlight() {
		return
	}
	status, err := e.git.StatusIgnoringSubmodules()
	if err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: could not re-read the worktree before clearing its in-flight marker: %v\n", err)
		return
	}
	if hasTrackedChanges(status) {
		return
	}
	if err := e.clearWorktreeInFlight(); err != nil {
		_, _ = fmt.Fprintf(e.output, "[Engineer] Warning: failed to clear the worktree in-flight marker: %v\n", err)
	}
}

// worktreeInFlight reports whether a marker attributes this worktree's dirt to
// the Refinery. An unreadable marker reports false, which the guard turns into
// a refusal (fail closed).
func (e *Engineer) worktreeInFlight() bool {
	path, err := e.worktreeInFlightPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (e *Engineer) worktreeInFlightPath() (string, error) {
	gitDir, err := e.git.GitDir()
	if err != nil {
		return "", fmt.Errorf("resolve refinery git directory: %w", err)
	}
	if strings.TrimSpace(gitDir) == "" {
		return "", errors.New("resolve refinery git directory: git returned an empty path")
	}
	return filepath.Join(gitDir, worktreeInFlightMarker), nil
}

// describeTrackedChanges renders the changes the guard refuses on, in the
// vocabulary that tells an operator whether they staged the work or merely
// edited it.
func describeTrackedChanges(status *git.GitStatus) string {
	if status == nil {
		return "an unreadable worktree state"
	}

	unmerged := pathSet(status.Unmerged)
	// `git status --porcelain`'s index column is not preserved in GitStatus,
	// but an added file is staged by definition, and a modified file whose
	// working tree already matches the index (StagedOnly) has nothing left
	// unstaged.
	staged := pathSet(status.StagedOnly)
	for _, path := range status.Added {
		staged[path] = true
	}

	var stagedPaths, unstagedPaths []string
	for _, path := range trackedChangePaths(status) {
		switch {
		case unmerged[path]:
			// Reported on its own below: a conflict is neither staged nor
			// merely uncommitted, and naming it as either misleads.
		case staged[path]:
			stagedPaths = append(stagedPaths, path)
		default:
			unstagedPaths = append(unstagedPaths, path)
		}
	}

	parts := []string{}
	if len(status.Unmerged) > 0 {
		parts = append(parts, fmt.Sprintf("%d unmerged (%s)", len(status.Unmerged), summarizePaths(status.Unmerged)))
	}
	if len(stagedPaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d staged (%s)", len(stagedPaths), summarizePaths(stagedPaths)))
	}
	if len(unstagedPaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted (%s)", len(unstagedPaths), summarizePaths(unstagedPaths)))
	}
	if len(parts) == 0 {
		return "an unclassified tracked change"
	}
	return strings.Join(parts, ", ")
}

// trackedChangePaths lists every path the guard refuses on, in the order
// git.Status groups them: conflicts first, then modifications, additions and
// deletions.
func trackedChangePaths(status *git.GitStatus) []string {
	paths := make([]string, 0,
		len(status.Unmerged)+len(status.Modified)+len(status.Added)+len(status.Deleted))
	paths = append(paths, status.Unmerged...)
	paths = append(paths, status.Modified...)
	paths = append(paths, status.Added...)
	paths = append(paths, status.Deleted...)
	return paths
}

func pathSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, path := range paths {
		set[path] = true
	}
	return set
}

func summarizePaths(paths []string) string {
	if len(paths) <= maxReportedDirtyPaths {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:maxReportedDirtyPaths], ", ") +
		fmt.Sprintf(", … %d more", len(paths)-maxReportedDirtyPaths)
}

// externallyDirtyRemedy tells the operator what to do about a refusal. The
// guard has not touched the tree, so the remedy is to move the work somewhere
// it belongs and re-run — never to reset it here, which is the failure this
// guard exists to replace.
func externallyDirtyRemedy(workDir, stage string) string {
	return fmt.Sprintf(`The refinery rig worktree is a merge checkout, not a working copy: %s.
A gate reads this tree, so work left in it can reach the merge target unnoticed.

Nothing was reset and nothing was lost — the changes are still in the tree.
Move them somewhere they belong and the merge proceeds:

    git -C %[1]s status
    git -C %[1]s diff HEAD
    git -C %[1]s checkout -b unused/refinery-rig-$(date +%%Y%%m%%d)
    git -C %[1]s commit -am "wip: moved off the refinery rig worktree"

Commit to a branch rather than a stash: refs/stash belongs to the shared git
directory, so a stash here collides with the polecat worktrees using it.

Refinery merges stay refused until this tree is clean (%[2]s).`, workDir, stage)
}
