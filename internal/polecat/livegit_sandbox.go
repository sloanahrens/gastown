package polecat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SandboxSeatGitState is the gateway's git answer for a sandbox seat. It never
// reads the host worktree, because a sandbox polecat edits and commits inside
// its sandbox and the host copy goes stale.
//
//   - While the sandbox runs, or when the gateway errors, the work may live in
//     the sandbox and the host cannot know: the answer is unknown. A gateway
//     blip must never read as a gone sandbox, or a live one would look clean.
//   - Once the gateway affirmatively reports no sandbox of that name, only what
//     was pushed survives, and this answer is a clean seat with no measured
//     facts. A caller that can name the seat's host worktree replaces it with
//     the host's measurement (SandboxSeatHostGitState); a caller with no
//     worktree to measure (the gateway probe itself) keeps this answer. Clean
//     is not "safe to discard": a seat holding a bead with no pushed or resume
//     branch is still lost work, and the recovery verdict turns it into a
//     rework.
//
// branch is the host worktree's branch name, kept as a label only.
func SandboxSeatGitState(branch string, exists bool, existsErr error, sandboxName string) LiveGitState {
	switch {
	case existsErr != nil:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("could not ask the gateway whether sandbox %s exists: %v", sandboxName, existsErr),
		}
	case exists:
		return LiveGitState{
			Branch:       branch,
			Source:       GitStateSourceUnknown,
			FailedReason: fmt.Sprintf("git state lives in the running sandbox %s", sandboxName),
		}
	}
	return LiveGitState{Branch: branch, Source: GitStateSourceLive}
}

// SandboxSeatGitRepo is the repository surface SandboxSeatHostGitState reads,
// so a caller can measure a seat through a repository model instead of a real
// clone.
type SandboxSeatGitRepo = gitRepo

// SandboxSeatGitOpener opens the repositories SandboxSeatHostGitState measures,
// and its zero value opens the real git binary.
type SandboxSeatGitOpener struct {
	Open    func(workDir string) SandboxSeatGitRepo
	OpenDir func(gitDir, workDir string) SandboxSeatGitRepo
}

// opener adapts the exported seam to the package's gitOpener.
func (o SandboxSeatGitOpener) opener() gitOpener {
	return gitOpener{open: o.Open, openDir: o.OpenDir}
}

// SandboxSeatHostGitState judges a gone sandbox seat from its host worktree, or
// from the branch refs its rig's shared repository still holds once that
// directory is gone, and returns unknown whenever that judgment cannot be read.
func SandboxSeatHostGitState(gone LiveGitState, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	return SandboxSeatHostGitStateWith(SandboxSeatGitOpener{}, gone, worktreePath, rigRoot, pushedBranch)
}

// SandboxSeatHostGitStateWith is SandboxSeatHostGitState over the repositories
// o opens.
func SandboxSeatHostGitStateWith(o SandboxSeatGitOpener, gone LiveGitState, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	return sandboxSeatHostGitState(o.opener(), gone, worktreePath, rigRoot, pushedBranch)
}

// sandboxSeatHostGitState is SandboxSeatHostGitState over the injected git
// seam. Only a gone seat (a live source) is measured; every other answer is
// returned unchanged, so a caller may pass any sandbox seat's state.
func sandboxSeatHostGitState(gits gitOpener, gone LiveGitState, worktreePath, rigRoot, pushedBranch string) LiveGitState {
	if gone.Source != GitStateSourceLive {
		return gone
	}
	return sandboxSeatGoneHostGitState(gits, worktreePath, rigRoot, pushedBranch, gone.Branch)
}

// sandboxSeatGoneHostGitState is the host measurement behind
// SandboxSeatHostGitState. An unresolved path, or one that is present but
// cannot be read, is unknown, never clean, because a comparison that did not
// run says nothing about the work it holds. A worktree's HEAD is compared with
// the pushed branch by patch identity, as is the rig shared repository's copy
// of the recorded branch; once the worktree directory is gone, every branch ref
// the shared repository holds for the seat is judged instead (gt-zgoo3).
//
// worktreePath is the seat's host clone; rigRoot holds the shared repository
// that survives the clone; pushedBranch is the branch the sandbox pushed, and
// an empty one lets the worktree's own branch stand in while the worktree
// exists (gt-zgoo3).
func sandboxSeatGoneHostGitState(gits gitOpener, worktreePath, rigRoot, pushedBranch, label string) LiveGitState {
	path := strings.TrimSpace(worktreePath)
	if path == "" {
		return liveGitUnknown("(none)", "resolving the host worktree",
			errors.New("the caller resolved no host worktree path"))
	}
	// The agent bead writes the recorded branch, so it is untrusted: a name git
	// would read as an option or a malformed ref never reaches a git command.
	recorded := strings.TrimSpace(pushedBranch)
	if recorded != "" && !validRecordedBranch(recorded) {
		return liveGitUnknown(path, "naming the seat's local branch",
			fmt.Errorf("the recorded branch %q is not a valid ref name", recorded))
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return sandboxSeatGoneSharedRepoGitState(gits, path, rigRoot, recorded, label)
		}
		return liveGitUnknown(path, "reading the host worktree", err)
	}
	if !isWorktreeRoot(gits, path) {
		// Git resolves upward, so a directory that is not a worktree root would
		// answer with the enclosing repository's branch and commits — a "live"
		// verdict about the wrong tree. Fail closed instead.
		return liveGitUnknown(path, "resolving worktree root", errors.New("path is not a git worktree root"))
	}

	g := gits.Open(path)
	hostBranch, err := g.CurrentBranch()
	if err != nil {
		return liveGitUnknown(path, "reading branch", err)
	}
	// The label stays a label: the agent bead's recorded branch names what the
	// sandbox pushed, and replacing it with the stale host clone's branch would
	// mislabel the seat. Only the comparison falls back to the host branch.
	pushed := recorded
	if pushed == "" {
		pushed = hostBranch
	}

	// Patch identity, not ancestry: a commit whose patch is already on the
	// pushed branch is not unique work, however its SHA was rewritten. When the
	// pushed branch is not on this clone's view of origin — the seat pushed
	// nothing, or the branch was pruned after landing — the remote's default
	// branch is the comparison that still proves the host's commits are not
	// unique; a missing default branch too fails the comparison, and unknown is
	// never clean.
	ref := "origin/" + pushed
	if _, err := g.Rev(ref); err != nil {
		ref = "origin/" + g.RemoteDefaultBranch()
	}
	preservation, err := g.RefPreservedByRef("HEAD", ref)
	if err != nil {
		return liveGitUnknown(path, "comparing HEAD with "+ref, err)
	}
	unpushed := preservation.UnpreservedPatchCount
	if recorded != "" {
		// The worktree's HEAD is not the only copy of the seat's branch: the
		// shared repository keeps the ref even when the worktree was reset or
		// checked out elsewhere, so a commit only that ref holds is counted too.
		shared, sharedRef, ok, err := sharedRepoSeatRef(gits, rigRoot, recorded)
		if err != nil {
			return liveGitUnknown(path, "reading refs/heads/"+recorded+" in the shared repository", err)
		}
		if ok {
			sharedUnpushed, err := sharedRepoRefUnpushed(shared, sharedRef, recorded)
			if err != nil {
				return liveGitUnknown(path, "judging "+sharedRef, err)
			}
			unpushed = mergeSharedRepoUnpushed(hostBranch == recorded, unpushed, sharedUnpushed)
		}
	}
	status, err := g.CheckUncommittedWork()
	if err != nil {
		return liveGitUnknown(path, "checking worktree state", err)
	}

	state := LiveGitState{
		Branch:          strings.TrimSpace(label),
		StashCount:      status.StashCount,
		UnpushedCommits: unpushed,
		Source:          GitStateSourceLive,
	}
	if !status.CleanExcludingRuntimeAndIndexSkew(g) {
		state.Dirty = true
		state.DirtyReason = fmt.Sprintf("git_state=has_uncommitted uncommitted_files=%d", len(status.NonRuntimeNonSkewPaths(g)))
	}
	return state
}

// sandboxSeatGoneSharedRepoGitState judges a gone seat whose host worktree
// directory is missing by the branch refs that worktree leaves in the rig's
// shared repository. A recorded branch names the one ref to compare; with no
// branch recorded, every generated branch the repository holds for the seat is
// compared, so a ref only it holds still blocks. An absent ref holds nothing
// unpushed; a repository that cannot be located, a branch list or comparison
// that cannot be read, and a worktree path that names no seat are unknown,
// never clean (gt-zgoo3).
func sandboxSeatGoneSharedRepoGitState(gits gitOpener, worktreePath, rigRoot, branch, label string) LiveGitState {
	root := strings.TrimSpace(rigRoot)
	if root == "" {
		return liveGitUnknown(worktreePath, "locating the shared repository",
			errors.New("the caller named no rig root"))
	}
	repo := rigGitRepo(root)
	if repo == "" {
		return liveGitUnknown(worktreePath, "locating the shared repository", noBareRepoErr(root))
	}
	g := openRigRepo(gits, repo)

	if branch != "" {
		localRef := "refs/heads/" + branch
		exists, err := g.RefExists(localRef)
		if err != nil {
			return liveGitUnknown(worktreePath, "reading "+localRef, err)
		}
		if !exists {
			// The repository never held the branch: nothing unpushed survives.
			return LiveGitState{Branch: strings.TrimSpace(label), Source: GitStateSourceLive}
		}
		unpushed, err := sharedRepoRefUnpushed(g, localRef, branch)
		if err != nil {
			return liveGitUnknown(worktreePath, "judging "+localRef, err)
		}
		return LiveGitState{
			Branch:          strings.TrimSpace(label),
			UnpushedCommits: unpushed,
			Source:          GitStateSourceLive,
		}
	}

	// No branch was recorded, so no ref is named: the seat's own generated
	// branches are the refs its worktree left under its name.
	seat, ok := sandboxSeatNameFromWorktree(root, worktreePath)
	if !ok {
		return liveGitUnknown(worktreePath, "naming the seat whose branches the shared repository holds",
			errors.New("the host worktree path names no polecat under polecats/"))
	}
	branches, err := g.ListBranches("polecat/*")
	if err != nil {
		return liveGitUnknown(worktreePath, "listing the shared repository's branches", err)
	}
	unpushed := 0
	for _, name := range branches {
		meta, generated := ParseGeneratedBranchName(name)
		if !generated || meta.Polecat != seat {
			continue
		}
		count, err := sharedRepoRefUnpushed(g, "refs/heads/"+name, name)
		if err != nil {
			return liveGitUnknown(worktreePath, "judging refs/heads/"+name, err)
		}
		unpushed += count
	}
	return LiveGitState{
		Branch:          strings.TrimSpace(label),
		UnpushedCommits: unpushed,
		Source:          GitStateSourceLive,
	}
}

// sharedRepoSeatRef locates the shared repository's local ref for a recorded
// branch. ok is false when the rig holds no ref for the branch; a rig root with
// no .repo.git is ErrNoBareRepo (fails closed), and a repository or ref that
// cannot be read is an error, so a missing repository is never read as a ref
// that holds nothing.
func sharedRepoSeatRef(gits gitOpener, rigRoot, branch string) (g gitRepo, localRef string, ok bool, err error) {
	root := strings.TrimSpace(rigRoot)
	if root == "" {
		return nil, "", false, nil
	}
	repo := rigGitRepo(root)
	if repo == "" {
		return nil, "", false, noBareRepoErr(root)
	}
	g = openRigRepo(gits, repo)
	localRef = "refs/heads/" + branch
	exists, err := g.RefExists(localRef)
	if err != nil {
		return nil, "", false, err
	}
	if !exists {
		return nil, "", false, nil
	}
	return g, localRef, true, nil
}

// sharedRepoRefUnpushed counts the commits localRef holds whose patches are on
// neither the pushed branch nor, when the repository never fetched it, the
// remote's default branch. A comparison that cannot be read is an error, which
// is never clean.
func sharedRepoRefUnpushed(g gitRepo, localRef, branch string) (int, error) {
	ref := "origin/" + branch
	if _, err := g.Rev(ref); err != nil {
		ref = "origin/" + g.RemoteDefaultBranch()
	}
	preservation, err := g.RefPreservedByRef(localRef, ref)
	if err != nil {
		return 0, fmt.Errorf("comparing %s with %s: %w", localRef, ref, err)
	}
	return preservation.UnpreservedPatchCount, nil
}

// mergeSharedRepoUnpushed combines the worktree's count with the shared
// repository's copy of the seat's branch. A worktree on the recorded branch and
// that ref describe the same commits, so the larger count is used; a worktree on
// another branch may hold different work, so the counts are summed, which can
// over-count rather than clear a seat that holds work.
func mergeSharedRepoUnpushed(sameBranch bool, headCount int, sharedCount int) int {
	if sameBranch {
		if sharedCount > headCount {
			return sharedCount
		}
		return headCount
	}
	return headCount + sharedCount
}

// sandboxSeatNameFromWorktree returns the polecat name a host worktree path
// encodes: <rigRoot>/polecats/<name>[/<rigName>]. ok is false for a path
// outside that layout, which names no seat whose branches to judge.
func sandboxSeatNameFromWorktree(rigRoot, worktreePath string) (string, bool) {
	rel, err := filepath.Rel(rigRoot, worktreePath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "polecats" || parts[1] == "" {
		return "", false
	}
	if len(parts) == 3 && parts[2] == "" {
		return "", false
	}
	name := parts[1]
	if !sandboxSeatSegmentRe.MatchString(name) {
		return "", false
	}
	return name, true
}

// validRecordedBranch reports whether branch is a name git accepts for a local
// branch, so an agent-written record can never reach git as an option or a
// malformed ref. It follows git check-ref-format --branch: no leading '-', no
// control characters or space, none of "~^:?*[\\", no "..", "@{" or "//", no
// leading or trailing '/', no component starting with '.', and no component
// ending in ".lock".
func validRecordedBranch(branch string) bool {
	if branch == "" || branch == "HEAD" || branch == "@" || strings.HasPrefix(branch, "-") {
		return false
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return false
		}
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") {
		return false
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") {
		return false
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}
