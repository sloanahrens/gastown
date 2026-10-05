package gitfake

import (
	"crypto/sha1" //nolint:gosec // G505: patch ids, not security
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// stash is one stash entry: the branch label git writes into its message
// ("WIP on <label>:"), which StashCount filters on.
type stash struct {
	label   string
	message string
}

// detachedLabel is the label git gives a stash made on a detached HEAD.
const detachedLabel = "(no branch)"

// patchOf hashes the change from tree a to tree b the way PatchID does.
func patchOf(a, b map[string]string) string {
	sum := sha1.New() //nolint:gosec // G401: patch ids, not security
	for _, p := range changedPaths(a, b) {
		removed, added := lineDiff(a[p], b[p])
		fmt.Fprintf(sum, "%s\x00", p)
		for _, l := range removed {
			fmt.Fprintf(sum, "-%s\x00", strings.Join(strings.Fields(l), ""))
		}
		for _, l := range added {
			fmt.Fprintf(sum, "+%s\x00", strings.Join(strings.Fields(l), ""))
		}
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// Stash records a stash of the checkout at dir, as git stash push -m message
// does: an entry labeled with the current branch (or (no branch) when
// detached), and tracked files put back to HEAD. Untracked files stay.
func (f *Fake) Stash(t testing.TB, dir, message string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	h := &handle{f: f, dir: clean(dir)}
	r, wt, err := h.workTree("stash")
	if err != nil {
		t.Fatalf("gitfake: Stash: %v", err)
	}
	label := detachedLabel
	if b, ok := strings.CutPrefix(headOf(r, wt), "refs/heads/"); ok {
		label = b
	}
	r.stashes = append([]stash{{label: label, message: message}}, r.stashes...)
	head := f.treeOf(headCommit(r, wt))
	if err := checkoutTree(wt.path, head, head, true); err != nil {
		t.Fatalf("gitfake: Stash: %v", err)
	}
}

// ClassifyIndexSkew answers nothing is checkout skew: the fake has no index,
// so no path is ever staged-only.
func (h *handle) ClassifyIndexSkew([]string) []string { return nil }

// stashCount is git.Git's StashCount: the entries made on wt's branch.
func stashCount(r *repo, wt *worktree) int {
	label := detachedLabel
	if b, ok := strings.CutPrefix(headOf(r, wt), "refs/heads/"); ok {
		label = b
	}
	n := 0
	for _, s := range r.stashes {
		if s.label == label {
			n++
		}
	}
	return n
}

func (h *handle) CheckUncommittedWork() (*git.UncommittedWorkStatus, error) {
	return h.checkUncommittedWork(false)
}

func (h *handle) CheckUncommittedWorkLocal() (*git.UncommittedWorkStatus, error) {
	return h.checkUncommittedWork(true)
}

func (h *handle) checkUncommittedWork(local bool) (*git.UncommittedWorkStatus, error) {
	st, err := h.Status()
	if err != nil {
		return nil, fmt.Errorf("checking git status: %w", err)
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	status := &git.UncommittedWorkStatus{
		HasUncommittedChanges: !st.Clean,
		ModifiedFiles:         append(append(append([]string(nil), st.Modified...), st.Added...), st.Deleted...),
		UntrackedFiles:        st.Untracked,
		UnmergedFiles:         st.Unmerged,
		StagedOnly:            st.StagedOnly,
		StashCount:            stashCount(r, wt),
	}
	branch := ""
	if b, ok := strings.CutPrefix(headOf(r, wt), "refs/heads/"); ok {
		branch = b
	}
	p, err := h.preservation(r, wt, branch, "origin", nil, true, local)
	switch {
	case errors.Is(err, errNoComparisonRefs):
	case err != nil:
		return nil, fmt.Errorf("checking unpushed commits: %w", err)
	default:
		status.UnpushedCommits = p.UnpreservedPatchCount
	}
	return status, nil
}

var errNoComparisonRefs = errors.New("no comparison refs resolved")

func (h *handle) BranchPreservationStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error) {
	return h.lockedPreservation(localBranch, remote, targets, true)
}

func (h *handle) BranchTargetStatus(localBranch, remote string, targets []string) (git.BranchPreservationStatus, error) {
	return h.lockedPreservation(localBranch, remote, targets, false)
}

func (h *handle) BranchPushedToRemote(localBranch, remote string) (bool, int, error) {
	status, err := h.lockedPreservation(localBranch, remote, nil, true)
	if err != nil {
		return false, 0, err
	}
	return status.Preserved, status.UnpreservedPatchCount, nil
}

func (h *handle) RefPreservedByRef(head, ref string) (git.BranchPreservationStatus, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("merge-base", "--is-ancestor", head, ref)
	if err != nil {
		return git.BranchPreservationStatus{ComparisonBase: ref}, err
	}
	return h.refAgainstRef(r, wt, head, ref)
}

func (h *handle) lockedPreservation(localBranch, remote string, targets []string, includeExact bool) (git.BranchPreservationStatus, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("rev-parse", "HEAD")
	if err != nil {
		return git.BranchPreservationStatus{}, err
	}
	return h.preservation(r, wt, localBranch, remote, targets, includeExact, false)
}

// refAgainstRef is git.Git's preservationOfRefAgainstRef: head is preserved
// by ref as an ancestor, when merging it into ref changes nothing, or when
// every one of its commits has an equivalent patch in ref.
func (h *handle) refAgainstRef(r *repo, wt *worktree, head, ref string) (git.BranchPreservationStatus, error) {
	status := git.BranchPreservationStatus{ComparisonBase: ref}
	hd, ok := h.resolve(r, wt, head)
	if !ok {
		return status, unknownRevision(head, "cherry", ref, head)
	}
	rf, ok := h.resolve(r, wt, ref)
	if !ok {
		return status, unknownRevision(ref, "cherry", ref, head)
	}
	if h.f.isAncestor(hd, rf) {
		status.Preserved, status.Evidence = true, "ancestor"
		return status, nil
	}
	if base := h.f.mergeBase(rf, hd); base != "" {
		merged, conflicts := merge3(h.f.treeOf(base), h.f.treeOf(rf), h.f.treeOf(hd))
		if len(conflicts) == 0 && sameTree(merged, h.f.treeOf(rf)) {
			status.Preserved, status.Evidence = true, "merge_tree_noop"
			return status, nil
		}
	}
	theirs := map[string]bool{}
	for _, c := range h.f.rangeCommits(hd, rf) {
		if len(c.parents) <= 1 {
			theirs[h.f.commitPatch(c)] = true
		}
	}
	for _, c := range h.f.rangeCommits(rf, hd) {
		if len(c.parents) <= 1 && !theirs[h.f.commitPatch(c)] {
			status.UnpreservedPatchCount++
		}
	}
	status.Preserved = status.UnpreservedPatchCount == 0
	if status.Preserved {
		status.Evidence = "cherry"
	}
	return status, nil
}

// preservation is git.Git's branchPreservationStatusWith: the exact pushed
// branch first, a detached HEAD's custody on any remote branch, then the
// targets and the upstream, and the remote's default branch only when
// nothing else is evidence. local reads the exact branch and the custody
// from remote-tracking refs instead of the remote — and, like the local level
// there, does not refresh the default-branch tracking ref.
func (h *handle) preservation(r *repo, wt *worktree, localBranch, remote string, targets []string, includeExact, local bool) (git.BranchPreservationStatus, error) {
	if remote == "" {
		remote = "origin"
	}
	var result git.BranchPreservationStatus
	var candidates []string
	targets = uniqueNonEmpty(targets)
	hasEvidence := len(targets) > 0
	head := headCommit(r, wt)
	rr, remoteErr := h.remoteRepo(r, remote)

	if includeExact && localBranch != "" && localBranch != "HEAD" {
		var tip string
		if local {
			tip = r.refs["refs/remotes/"+remote+"/"+localBranch]
		} else if remoteErr == nil {
			tip = rr.refs["refs/heads/"+localBranch]
		}
		if tip != "" {
			hasEvidence = true
			result.ComparisonBase = remote + "/" + localBranch
			if head != "" && (tip == head || h.f.isAncestor(head, tip)) {
				result.Preserved, result.Evidence = true, "exact_remote_branch"
				return result, nil
			}
			candidates = append(candidates, tip)
		}
	}

	if includeExact && (localBranch == "" || localBranch == "HEAD") && head != "" {
		if ref, ok := h.custody(r, rr, remote, head, local); ok {
			result.Preserved, result.ComparisonBase, result.Evidence = true, ref, "detached_head_on_remote_branch"
			return result, nil
		}
	}

	// git.Git.branchPreservationStatusWith refreshes the default-branch
	// tracking ref from the remote before judging against it — the live level
	// only, since the local one stays offline. Mirror that here: copy the
	// remote's current default branch into the clone's tracking ref, once, at
	// the same two points (before the fallback resolves its refs, and before
	// judging a target or upstream that resolved to that ref).
	refreshed := false
	refreshDefault := func() {
		if refreshed || local || remoteErr != nil {
			return
		}
		refreshed = true
		def := defaultBranchOf(r)
		tip, ok := rr.refs["refs/heads/"+def]
		if !ok {
			return
		}
		h.f.copyObjects(r, tip)
		r.refs["refs/remotes/"+remote+"/"+def] = tip
	}

	for _, target := range targets {
		if ref, ok := h.comparisonRef(r, wt, target, remote); ok {
			candidates = append(candidates, ref)
		}
	}

	if upstream := upstreamOf(r, localBranch); upstream != "" {
		foreign := strings.HasPrefix(localBranch, "polecat/") && strings.HasPrefix(upstream, remote+"/polecat/") && upstream != remote+"/"+localBranch
		self := strings.HasPrefix(localBranch, "polecat/") && upstream == remote+"/"+localBranch
		if !foreign && (includeExact || !self) {
			hasEvidence = true
			candidates = append(candidates, upstream)
		}
	}

	if !hasEvidence {
		refreshDefault()
		def := defaultBranchOf(r)
		for _, ref := range []string{remote + "/" + def, remote + "/main", remote + "/master"} {
			if resolved, ok := h.comparisonRef(r, wt, ref, remote); ok {
				candidates = append(candidates, resolved)
			}
		}
	}

	candidates = uniqueNonEmpty(candidates)
	if !refreshed && candidatesAreDefault(r, candidates, remote) {
		refreshDefault()
	}
	if len(candidates) == 0 {
		if hasEvidence {
			return result, fmt.Errorf("no target/custody refs resolved")
		}
		return result, errNoComparisonRefs
	}
	var lastErr error
	judged := false
	for _, ref := range candidates {
		candidate, err := h.refAgainstRef(r, wt, "HEAD", ref)
		if err != nil {
			lastErr = err
			continue
		}
		if candidate.Evidence == "" {
			candidate.Evidence = "comparison_ref"
		}
		if candidate.Preserved {
			return candidate, nil
		}
		if !judged {
			judged = true
			if result.ComparisonBase != "" {
				candidate.ComparisonBase = result.ComparisonBase
			}
			result = candidate
		}
	}
	if judged || result.ComparisonBase != "" {
		return result, nil
	}
	if lastErr != nil {
		return result, lastErr
	}
	return result, fmt.Errorf("no usable comparison refs")
}

// defaultBranchOf is git.Git.RemoteDefaultBranch for this clone: the branch its
// remote HEAD records, then master, then main. It is the branch the fallback
// compares against, and the one a live verdict refreshes first.
func defaultBranchOf(r *repo) string {
	if b := r.configMap()[remoteHeadKey("origin")]; b != "" {
		return b
	}
	if _, ok := r.refs["refs/remotes/origin/master"]; ok {
		return "master"
	}
	return "main"
}

// candidatesAreDefault is git.Git.candidatesAreDefaultBranch: whether any ref
// about to be judged is this clone's tracking ref for the remote's default
// branch.
func candidatesAreDefault(r *repo, candidates []string, remote string) bool {
	def := defaultBranchOf(r)
	for _, candidate := range candidates {
		switch candidate {
		case remote + "/" + def, remote + "/main", remote + "/master":
			return true
		}
	}
	return false
}

// custody finds a remote branch holding head: a remote-tracking ref that
// contains it, or (unless local) a branch on the remote whose tip it is.
func (h *handle) custody(r, rr *repo, remote, head string, local bool) (string, bool) {
	var names []string
	for ref, id := range r.refs {
		if name, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/"); ok && name != "HEAD" && (id == head || h.f.isAncestor(head, id)) {
			names = append(names, remote+"/"+name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0], true
	}
	if local || rr == nil {
		return "", false
	}
	for ref, id := range rr.refs {
		if name, ok := strings.CutPrefix(ref, "refs/heads/"); ok && id == head {
			return remote + "/" + name, true
		}
	}
	return "", false
}

// comparisonRef is git.Git's resolveComparisonRef.
func (h *handle) comparisonRef(r *repo, wt *worktree, ref, remote string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	var candidates []string
	switch {
	case strings.HasPrefix(ref, "refs/") || strings.HasPrefix(ref, remote+"/") || strings.HasPrefix(ref, "upstream/"):
		candidates = []string{ref}
	case !strings.Contains(ref, "/") && remote != "upstream":
		candidates = []string{"upstream/" + ref, remote + "/" + ref, ref}
	default:
		candidates = []string{remote + "/" + ref, ref}
	}
	for _, c := range candidates {
		if strings.HasPrefix(c, "refs/") {
			if _, ok := r.refs[c]; ok {
				return c, true
			}
			continue
		}
		if _, ok := h.resolve(r, wt, c); ok {
			return c, true
		}
	}
	return "", false
}

// upstreamOf is @{u} for branch: remote/branch from its tracking config, when
// that remote-tracking ref exists.
func upstreamOf(r *repo, branch string) string {
	if branch == "" || branch == "HEAD" {
		return ""
	}
	remote := r.configMap()["branch."+branch+".remote"]
	merge := strings.TrimPrefix(r.configMap()["branch."+branch+".merge"], "refs/heads/")
	if remote == "" || merge == "" {
		return ""
	}
	if _, ok := r.refs["refs/remotes/"+remote+"/"+merge]; !ok {
		return ""
	}
	return remote + "/" + merge
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
