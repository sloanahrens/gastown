package gitfake

import (
	"crypto/sha1" //nolint:gosec // G505: patch ids, not security
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// Repo is the part of *git.Git the fake answers: the methods converted
// consumers call. *git.Git satisfies it, and RunRepoContract holds both to
// the same behavior.
type Repo interface {
	Rev(ref string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
	TreesIdentical(a, b string) (bool, error)
	CommitMessages(base, head string) ([]git.CommitMessage, error)
	CommitLineStatsInRange(revRange string, limit int) ([]git.CommitLineStats, error)
	PatchID(base, head string) (string, error)
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
	PushRemoteBranchTip(remote, branch string) (string, error)
	PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error
	VerifyPushedCommit(remote, branch, commit string) error
	WorktreeAddDetached(path, ref string) error
	WorktreeRemove(path string, force bool) error
	WorktreePrune() error
	MergeNoFF(branch, message string) error
	MergeSquash(branch, message string) error
	GetConflictingFiles() ([]string, error)
	AbortMerge() error

	// Clones and remotes (clone.go).
	CloneBareWithBranch(url, dest, branch string) error
	CloneBareWithReferenceAndBranch(url, dest, reference, branch string) error
	CloneBarePartialWithBranch(url, dest, filter, branch string) error
	CloneBarePartialWithReferenceAndBranch(url, dest, filter, reference, branch string) error
	CloneBranch(url, dest, branch string) error
	CloneBranchWithReference(url, dest, branch, reference string) error
	CloneBranchPartial(url, dest, branch, filter string) error
	CloneBranchPartialWithReference(url, dest, branch, filter, reference string) error
	RemoteHasRefs(remote string) (bool, error)
	FetchBranchShallow(remote, branch string) error
	RemoteURL(remote string) (string, error)
	GetPushURL(remote string) (string, error)
	ConfigurePushURL(remote, pushURL string) error
	ClearPushURL(remote string) error
	AddUpstreamRemote(upstreamURL string) error
	IsRepo() bool
	IsEmpty() (bool, error)
	DefaultBranch() string
	RefExists(ref string) (bool, error)
	CommonDir() (string, error)
}

var _ Repo = (*git.Git)(nil)

// handle is Repo for one directory: a repository, or a worktree of one.
type handle struct {
	f   *Fake
	dir string
}

var _ Repo = (*handle)(nil)

// locate finds the repository and worktree at h.dir. wt is nil for a bare
// repository. Callers hold f.mu.
func (h *handle) locate(args ...string) (*repo, *worktree, error) {
	if r, wt := h.f.at(h.dir); r != nil && onDisk(r, wt) {
		return r, wt, nil
	}
	// Like git, an existing directory inside a checkout resolves to that
	// checkout; a directory that does not exist is no repository at all
	// (git.Git refuses a missing working directory before running git).
	if _, err := os.Stat(h.dir); err == nil {
		for dir := filepath.Dir(h.dir); ; dir = filepath.Dir(dir) {
			if r, wt := h.f.at(dir); r != nil && onDisk(r, wt) {
				return r, wt, nil
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return nil, nil, gitErr(128, "fatal: not a git repository (or any of the parent directories): .git", args...)
}

// workTree is locate for commands that need a checkout.
func (h *handle) workTree(args ...string) (*repo, *worktree, error) {
	r, wt, err := h.locate(args...)
	if err != nil {
		return nil, nil, err
	}
	if wt == nil {
		return nil, nil, gitErr(128, "fatal: this operation must be run in a work tree", args...)
	}
	return r, wt, nil
}

func headOf(r *repo, wt *worktree) string {
	if wt != nil && wt != r.main {
		return wt.head
	}
	return r.head
}

// headCommit resolves HEAD in wt.
func headCommit(r *repo, wt *worktree) string {
	head := headOf(r, wt)
	if strings.HasPrefix(head, "refs/") {
		return r.refs[head]
	}
	return head
}

// setHead moves wt's HEAD: the branch it is on, or the detached commit.
func setHead(r *repo, wt *worktree, id string) {
	head := headOf(r, wt)
	switch {
	case strings.HasPrefix(head, "refs/"):
		r.refs[head] = id
	case wt != nil && wt != r.main:
		wt.head = id
	default:
		r.head = id
	}
}

// resolve finds the commit rev names in r, the way rev-parse searches refs.
// A full id must be one r holds.
func (h *handle) resolve(r *repo, wt *worktree, rev string) (string, bool) {
	rev = strings.TrimSuffix(rev, "^{commit}")
	if rev == "HEAD" {
		id := headCommit(r, wt)
		return id, id != ""
	}
	if isFullID(rev) {
		return rev, r.has[rev]
	}
	for _, ref := range []string{rev, "refs/" + rev, "refs/tags/" + rev, "refs/heads/" + rev, "refs/remotes/" + rev, "refs/remotes/" + rev + "/HEAD"} {
		if id, ok := r.refs[ref]; ok {
			return id, true
		}
	}
	return "", false
}

func unknownRevision(rev string, args ...string) error {
	return gitErr(128, fmt.Sprintf("fatal: ambiguous argument '%s': unknown revision or path not in the working tree.\nUse '--' to separate paths from revisions, like this:\n'git <command> [<revision>...] -- [<file>...]'", rev), args...)
}

// remoteRepo finds the repository remote names from r: a configured remote,
// or a path.
func (h *handle) remoteRepo(r *repo, remote string, args ...string) (*repo, error) {
	url, ok := r.remotes[remote]
	if !ok {
		url = remote
	}
	if !filepath.IsAbs(url) {
		url = filepath.Join(r.path, url)
	}
	if rr := h.f.repos[clean(url)]; rr != nil {
		return rr, nil
	}
	return nil, gitErr(128, fmt.Sprintf("fatal: '%s' does not appear to be a git repository\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.", remote), args...)
}

func (h *handle) Rev(ref string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"rev-parse", ref}
	r, wt, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	// rev-parse echoes a bare full id without looking it up.
	if isFullID(ref) {
		return ref, nil
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return "", unknownRevision(ref, args...)
	}
	return id, nil
}

func isFullID(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (h *handle) IsAncestor(ancestor, descendant string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"merge-base", "--is-ancestor", ancestor, descendant}
	r, wt, err := h.locate(args...)
	if err != nil {
		return false, err
	}
	a, ok := h.resolve(r, wt, ancestor)
	if !ok {
		return false, gitErr(128, "fatal: Not a valid commit name "+ancestor, args...)
	}
	d, ok := h.resolve(r, wt, descendant)
	if !ok {
		return false, gitErr(128, "fatal: Not a valid commit name "+descendant, args...)
	}
	return h.f.isAncestor(a, d), nil
}

func (h *handle) TreesIdentical(a, b string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("rev-parse", a+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve tree of %s: %w", a, err)
	}
	ai, ok := h.resolve(r, wt, a)
	if !ok {
		return false, fmt.Errorf("resolve tree of %s: %w", a, unknownRevision(a+"^{tree}", "rev-parse", a+"^{tree}"))
	}
	bi, ok := h.resolve(r, wt, b)
	if !ok {
		return false, fmt.Errorf("resolve tree of %s: %w", b, unknownRevision(b+"^{tree}", "rev-parse", b+"^{tree}"))
	}
	return sameTree(h.f.treeOf(ai), h.f.treeOf(bi)), nil
}

// resolveRange resolves base..head for a log.
func (h *handle) resolveRange(base, head string, args ...string) (string, string, error) {
	r, wt, err := h.locate(args...)
	if err != nil {
		return "", "", err
	}
	b, ok := h.resolve(r, wt, base)
	if !ok {
		return "", "", unknownRevision(base+".."+head, args...)
	}
	hd, ok := h.resolve(r, wt, head)
	if !ok {
		return "", "", unknownRevision(base+".."+head, args...)
	}
	return b, hd, nil
}

func (h *handle) CommitMessages(base, head string) ([]git.CommitMessage, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	b, hd, err := h.resolveRange(base, head, "log", "--format=%H%x1f%B%x1e", base+".."+head)
	if err != nil {
		return nil, err
	}
	var msgs []git.CommitMessage
	for _, c := range h.f.rangeCommits(b, hd) {
		msgs = append(msgs, git.CommitMessage{SHA: c.id, Message: strings.TrimSpace(c.message)})
	}
	return msgs, nil
}

func (h *handle) CommitLineStatsInRange(revRange string, limit int) ([]git.CommitLineStats, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"log", "--no-merges", "--numstat", "--format=%H%n%s", "-n", fmt.Sprint(limit), revRange}
	base, head, ok := strings.Cut(revRange, "..")
	if !ok {
		return nil, fmt.Errorf("gitfake: CommitLineStatsInRange takes base..head, got %q", revRange)
	}
	b, hd, err := h.resolveRange(base, head, args...)
	if err != nil {
		return nil, err
	}
	var stats []git.CommitLineStats
	for _, c := range h.f.rangeCommits(b, hd) {
		if len(c.parents) > 1 {
			continue
		}
		if len(stats) == limit {
			break
		}
		var parent map[string]string
		if len(c.parents) == 1 {
			parent = h.f.treeOf(c.parents[0])
		}
		s := git.CommitLineStats{Commit: c.id, Subject: strings.TrimSpace(strings.SplitN(c.message, "\n", 2)[0])}
		for _, p := range changedPaths(parent, c.tree) {
			removed, added := lineDiff(parent[p], c.tree[p])
			s.Added += len(added)
			s.Removed += len(removed)
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// changedPaths returns the paths whose content differs between a and b,
// sorted.
func changedPaths(a, b map[string]string) []string {
	var paths []string
	for p, c := range a {
		if bc, ok := b[p]; !ok || bc != c {
			paths = append(paths, p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// PatchID hashes the change base..head the way patch-id --stable does: by
// the lines each file removes and adds, without context, line numbers or
// whitespace, so the same change on another base has the same id.
func (h *handle) PatchID(base, head string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	b, hd, err := h.resolveRange(base, head, "diff", base+".."+head)
	if err != nil {
		return "", err
	}
	bt, ht := h.f.treeOf(b), h.f.treeOf(hd)
	paths := changedPaths(bt, ht)
	if len(paths) == 0 {
		return "", fmt.Errorf("git patch-id produced no output for range %s..%s", base, head)
	}
	sum := sha1.New() //nolint:gosec // G401: patch ids, not security
	for _, p := range paths {
		removed, added := lineDiff(bt[p], ht[p])
		fmt.Fprintf(sum, "%s\x00", p)
		for _, l := range removed {
			fmt.Fprintf(sum, "-%s\x00", strings.Join(strings.Fields(l), ""))
		}
		for _, l := range added {
			fmt.Fprintf(sum, "+%s\x00", strings.Join(strings.Fields(l), ""))
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func (h *handle) FetchRefspecWithTimeout(remote, refspec string, _ time.Duration) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"fetch", remote, refspec}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	force := strings.HasPrefix(refspec, "+")
	src, dst, _ := strings.Cut(strings.TrimPrefix(refspec, "+"), ":")
	if !strings.HasPrefix(src, "refs/") {
		src = "refs/heads/" + src
	}
	id, ok := rr.refs[src]
	if !ok {
		return gitErr(128, "fatal: couldn't find remote ref "+src, args...)
	}
	h.f.copyObjects(r, id)
	if dst == "" {
		return nil
	}
	if cur, ok := r.refs[dst]; ok && !force && !h.f.isAncestor(cur, id) {
		return gitErr(1, fmt.Sprintf(" ! [rejected]        %s -> %s  (non-fast-forward)", src, dst), args...)
	}
	r.refs[dst] = id
	return nil
}

func (h *handle) PushRemoteBranchTip(remote, branch string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"ls-remote", remote, "refs/heads/" + branch}
	r, _, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return "", err
	}
	return rr.refs["refs/heads/"+branch], nil
}

func (h *handle) PushForceWithLease(remote, refspec, branchRef, expectedSHA string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"push", remote, refspec, fmt.Sprintf("--force-with-lease=%s:%s", branchRef, expectedSHA)}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	src, dst, _ := strings.Cut(refspec, ":")
	id, ok := h.resolve(r, wt, src)
	if !ok {
		return gitErr(1, "error: src refspec "+src+" does not match any\nerror: failed to push some refs to '"+rr.path+"'", args...)
	}
	if rr.refs[branchRef] != expectedSHA {
		short := strings.TrimPrefix(dst, "refs/heads/")
		return gitErr(1, fmt.Sprintf("To %s\n ! [rejected]        %s -> %s (stale info)\nerror: failed to push some refs to '%s'", rr.path, src, short, rr.path), args...)
	}
	h.f.copyObjects(rr, id)
	rr.refs[dst] = id
	return nil
}

// VerifyPushedCommit is *git.Git's check, over PushRemoteBranchTip.
func (h *handle) VerifyPushedCommit(remote, branch, commit string) error {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return fmt.Errorf("verified_push_failed: empty commit for %s/%s", remote, branch)
	}
	tip, err := h.PushRemoteBranchTip(remote, branch)
	if err != nil {
		return fmt.Errorf("verified_push_failed: unable to read %s/%s: %w", remote, branch, err)
	}
	if tip == "" {
		return fmt.Errorf("verified_push_failed: branch %s/%s missing after push (expected %s)", remote, branch, short(commit))
	}
	if tip != commit {
		return fmt.Errorf("verified_push_failed: commit %s not on %s/%s (remote tip %s)", short(commit), remote, branch, short(tip))
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (h *handle) WorktreeAddDetached(path, ref string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"worktree", "add", "--detach", path, ref}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return gitErr(128, "fatal: invalid reference: "+ref, args...)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(h.dir, abs)
	}
	abs = clean(abs)
	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return gitErr(128, fmt.Sprintf("fatal: '%s' already exists", path), args...)
	}
	if err := materialize(abs, h.f.treeOf(id)); err != nil {
		return gitErr(128, "fatal: could not create work tree dir '"+path+"': "+err.Error(), args...)
	}
	r.worktrees[abs] = &worktree{path: abs, head: id}
	if err := layoutWorktree(r, abs); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	return nil
}

func (h *handle) WorktreeRemove(path string, _ bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"worktree", "remove", path}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(h.dir, abs)
	}
	abs = clean(abs)
	if r.worktrees[abs] == nil {
		return gitErr(128, fmt.Sprintf("fatal: '%s' is not a working tree", path), args...)
	}
	delete(r.worktrees, abs)
	_ = os.RemoveAll(worktreeGitDir(r, abs))
	if err := os.RemoveAll(abs); err != nil {
		return gitErr(128, "fatal: failed to delete '"+path+"': "+err.Error(), args...)
	}
	return nil
}

// WorktreePrune forgets worktrees whose directory is gone.
func (h *handle) WorktreePrune() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("worktree", "prune")
	if err != nil {
		return err
	}
	for p := range r.worktrees {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			delete(r.worktrees, p)
		}
	}
	return nil
}

func (h *handle) MergeNoFF(branch, message string) error {
	return h.merge(branch, message, false)
}

func (h *handle) MergeSquash(branch, message string) error {
	return h.merge(branch, message, true)
}

// merge is merge --no-ff -m message branch, or merge --squash branch
// followed by commit -m message.
func (h *handle) merge(branch, message string, squash bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"merge", "--no-ff", "-m", message, branch}
	if squash {
		args = []string{"merge", "--squash", branch}
	}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	if wt.merge != nil {
		return gitErr(128, "error: Merging is not possible because you have unmerged files.\nhint: Fix them up in the work tree, and then use 'git add/rm <file>'\nhint: as appropriate to mark resolution and make a commit.\nfatal: Exiting because of an unresolved conflict.", args...)
	}
	theirs, ok := h.resolve(r, wt, branch)
	if !ok {
		return gitErr(1, "merge: "+branch+" - not something we can merge", args...)
	}
	ours := headCommit(r, wt)
	if h.f.isAncestor(theirs, ours) {
		if squash {
			return gitErr(1, "nothing to commit, working tree clean", "commit", "-m", message)
		}
		return nil // Already up to date.
	}
	base := h.f.mergeBase(ours, theirs)
	if base == "" {
		return gitErr(128, "fatal: refusing to merge unrelated histories", args...)
	}
	result, conflicts := merge3(h.f.treeOf(base), h.f.treeOf(ours), h.f.treeOf(theirs))
	if len(conflicts) > 0 {
		wt.merge = &mergeState{squash: squash, conflicts: conflicts}
		for _, p := range conflicts {
			result[p] = "<<<<<<< HEAD\n" + h.f.treeOf(ours)[p] + "=======\n" + h.f.treeOf(theirs)[p] + ">>>>>>> " + branch + "\n"
		}
		_ = materialize(wt.path, result)
		var out strings.Builder
		for _, p := range conflicts {
			fmt.Fprintf(&out, "CONFLICT (content): Merge conflict in %s\n", p)
		}
		out.WriteString("Automatic merge failed; fix conflicts and then commit the result.")
		return &git.GitError{Command: "merge", Args: args, Stdout: out.String(), Err: exitError(1)}
	}
	parents := []string{ours, theirs}
	if squash {
		if sameTree(result, h.f.treeOf(ours)) {
			return gitErr(1, "nothing to commit, working tree clean", "commit", "-m", message)
		}
		parents = []string{ours}
	}
	id := h.f.newCommit(parents, result, message)
	r.has[id] = true
	setHead(r, wt, id)
	if err := materialize(wt.path, result); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	return nil
}

func (h *handle) GetConflictingFiles() ([]string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	_, wt, err := h.workTree("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	if wt.merge == nil {
		return nil, nil
	}
	return append([]string(nil), wt.merge.conflicts...), nil
}

func (h *handle) AbortMerge() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"merge", "--abort"}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	if wt.merge == nil || wt.merge.squash {
		return gitErr(128, "fatal: There is no merge to abort (MERGE_HEAD missing).", args...)
	}
	wt.merge = nil
	if err := materialize(wt.path, h.f.treeOf(headCommit(r, wt))); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	return nil
}
