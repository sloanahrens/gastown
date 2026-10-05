package gitfake

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// remoteHeadKey is where the fake keeps refs/remotes/<remote>/HEAD, the
// symbolic ref git clone writes, as a branch name in the repository's config.
func remoteHeadKey(remote string) string { return "\x00remote-head." + remote }

// cloned records what git clone configures beyond the refs: origin's HEAD
// branch, and the checked-out branch tracking it.
func cloned(r, src *repo) {
	if b, ok := strings.CutPrefix(src.head, "refs/heads/"); ok {
		r.configMap()[remoteHeadKey("origin")] = b
		if _, exists := r.refs["refs/remotes/origin/"+b]; exists {
			setUpstream(r, b, "origin/"+b)
		}
	}
}

func (r *repo) configMap() map[string]string {
	if r.config == nil {
		r.config = map[string]string{}
	}
	return r.config
}

// setUpstream records what git's branch.autoSetupMerge does for a branch
// started from a remote-tracking ref: the branch tracks it.
func setUpstream(r *repo, branch, startPoint string) {
	for remote := range r.remotes {
		if b, ok := strings.CutPrefix(strings.TrimPrefix(startPoint, "refs/remotes/"), remote+"/"); ok {
			if _, exists := r.refs["refs/remotes/"+remote+"/"+b]; exists {
				r.configMap()["branch."+branch+".remote"] = remote
				r.configMap()["branch."+branch+".merge"] = "refs/heads/" + b
			}
			return
		}
	}
}

func (h *handle) ConfigGet(key string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("config", "--get", key)
	if err != nil {
		return "", nil // git.Git reads any failure as unset
	}
	return r.configMap()[key], nil
}

func (h *handle) ConfigSet(key, value string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("config", key, value)
	if err != nil {
		return err
	}
	r.configMap()[key] = value
	return nil
}

// globRE turns a git branch --list pattern into a regexp: * and ? match any
// characters, / included, as git's wildmatch does without pathname mode.
func globRE(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for _, c := range pattern {
		switch c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (h *handle) ListBranches(pattern string) ([]string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("branch", "--list", pattern)
	if err != nil {
		return nil, err
	}
	var names []string
	for ref := range r.refs {
		name, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || (pattern != "" && !globRE(pattern).MatchString(name)) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (h *handle) DeleteBranch(name string, force bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	flag := "-d"
	if force {
		flag = "-D"
	}
	args := []string{"branch", flag, name}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + name
	id, ok := r.refs[ref]
	if !ok {
		return gitErr(1, fmt.Sprintf("error: branch '%s' not found.", name), args...)
	}
	if at := checkedOutAt(r, name); at != "" {
		return gitErr(1, fmt.Sprintf("error: Cannot delete branch '%s' checked out at '%s'", name, at), args...)
	}
	if !force {
		into := headCommit(r, wt)
		if up := r.configMap()["branch."+name+".merge"]; up != "" {
			if tip, ok := r.refs["refs/remotes/"+r.configMap()["branch."+name+".remote"]+"/"+strings.TrimPrefix(up, "refs/heads/")]; ok {
				into = tip
			}
		}
		if into == "" || !h.f.isAncestor(id, into) {
			return gitErr(1, fmt.Sprintf("error: the branch '%s' is not fully merged.", name), args...)
		}
	}
	delete(r.refs, ref)
	delete(r.configMap(), "branch."+name+".remote")
	delete(r.configMap(), "branch."+name+".merge")
	return nil
}

func (h *handle) RemoteDefaultBranch() string {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("symbolic-ref", "refs/remotes/origin/HEAD")
	if err != nil {
		return "main"
	}
	if b := r.configMap()[remoteHeadKey("origin")]; b != "" {
		return b
	}
	if _, ok := r.refs["refs/remotes/origin/master"]; ok {
		return "master"
	}
	return "main"
}

// fetchInto copies remote's branches into r's refs/remotes/<name>/, and
// records the remote's HEAD branch as refs/remotes/<name>/HEAD.
func (h *handle) fetchInto(r, rr *repo, name string, only string) error {
	found := false
	for ref, id := range rr.refs {
		branch, ok := strings.CutPrefix(ref, "refs/heads/")
		if !ok || (only != "" && branch != only) {
			continue
		}
		found = true
		h.f.copyObjects(r, id)
		r.refs["refs/remotes/"+name+"/"+branch] = id
	}
	if only != "" && !found {
		return gitErr(128, "fatal: couldn't find remote ref "+only, "fetch", name, only)
	}
	if b, ok := strings.CutPrefix(rr.head, "refs/heads/"); ok && only == "" {
		r.configMap()[remoteHeadKey(name)] = b
	}
	return nil
}

func (h *handle) Fetch(remote string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"fetch", remote}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	return h.fetchInto(r, rr, remote, "")
}

// FetchPrune is Fetch that also drops remote-tracking refs whose branch the
// remote no longer has.
func (h *handle) FetchPrune(remote string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"fetch", "--prune", remote}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	prefix := "refs/remotes/" + remote + "/"
	for ref := range r.refs {
		if branch, ok := strings.CutPrefix(ref, prefix); ok {
			if _, kept := rr.refs["refs/heads/"+branch]; !kept {
				delete(r.refs, ref)
			}
		}
	}
	return h.fetchInto(r, rr, remote, "")
}

// DeleteRemoteBranchIfAt deletes branch on remote only while it still points
// at expectedHash, and drops its remote-tracking ref, as git push does.
func (h *handle) DeleteRemoteBranchIfAt(remote, branch, expectedHash string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	ref := "refs/heads/" + branch
	args := []string{"push", "--force-with-lease=" + ref + ":" + expectedHash, remote, ":" + ref}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	if rr.refs[ref] != expectedHash {
		return gitErr(1, fmt.Sprintf("To %s\n ! [rejected]        (delete) -> %s (stale info)\nerror: failed to push some refs to '%s'", rr.path, branch, rr.path), args...)
	}
	delete(rr.refs, ref)
	delete(r.refs, "refs/remotes/"+remote+"/"+branch)
	return nil
}

// GC has nothing to collect in the model; it fails only outside a
// repository.
func (h *handle) GC() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	_, _, err := h.locate("gc", "--quiet")
	return err
}

func (h *handle) FetchBranch(remote, branch string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"fetch", remote, branch}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	return h.fetchInto(r, rr, remote, strings.TrimPrefix(branch, "refs/heads/"))
}

// RefreshRemoteDefaultBranch is git.Git.RefreshRemoteDefaultBranch: fetch
// remote's default branch into this clone's remote-tracking ref for it, and
// nothing else.
func (h *handle) RefreshRemoteDefaultBranch(remote string) error {
	branch := h.RemoteDefaultBranch()
	return h.FetchRefspecWithTimeout(remote,
		"+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch, time.Second)
}

func (h *handle) ListRemoteRefsWithHashes(remote, prefix string) ([]git.RemoteRef, error) {
	return h.ListRemoteRefsWithHashesTimeout(remote, prefix, 0)
}

func (h *handle) ListRemoteRefsWithHashesTimeout(remote, prefix string, _ time.Duration) ([]git.RemoteRef, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"ls-remote", "--refs", remote, prefix + "*"}
	r, _, err := h.locate(args...)
	if err != nil {
		return nil, err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return nil, err
	}
	var refs []git.RemoteRef
	for ref, id := range rr.refs {
		if strings.HasPrefix(ref, prefix) && (strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")) {
			refs = append(refs, git.RemoteRef{Hash: id, Name: ref})
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// FirstParentContains reports whether commit is on descendant's first-parent
// line.
func (h *handle) FirstParentContains(commit, descendant string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"rev-list", "--first-parent", descendant}
	r, wt, err := h.locate(args...)
	if err != nil {
		return false, err
	}
	id, ok := h.resolve(r, wt, descendant)
	if !ok {
		return false, unknownRevision(descendant, args...)
	}
	for id != "" {
		if id == commit {
			return true, nil
		}
		c := h.f.objects[id]
		if c == nil || len(c.parents) == 0 {
			break
		}
		id = c.parents[0]
	}
	return false, nil
}

// commitPatch is one non-merge commit's patch id against its first parent.
func (f *Fake) commitPatch(c *commit) string {
	var parent map[string]string
	if len(c.parents) > 0 {
		parent = f.treeOf(c.parents[0])
	}
	return patchOf(parent, c.tree)
}

// Cherry is git cherry upstream head: one line per non-merge commit in
// upstream..head, oldest first, "-" when upstream..head's other side holds an
// equivalent patch and "+" when it does not.
func (h *handle) Cherry(upstream, head string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	u, hd, err := h.resolveRange(upstream, head, "cherry", upstream, head)
	if err != nil {
		return "", err
	}
	theirs := map[string]bool{}
	for _, c := range h.f.rangeCommits(hd, u) {
		if len(c.parents) <= 1 {
			theirs[h.f.commitPatch(c)] = true
		}
	}
	mine := h.f.rangeCommits(u, hd)
	var lines []string
	for i := len(mine) - 1; i >= 0; i-- {
		c := mine[i]
		if len(c.parents) > 1 {
			continue
		}
		mark := "+"
		if theirs[h.f.commitPatch(c)] {
			mark = "-"
		}
		lines = append(lines, mark+" "+c.id)
	}
	return strings.Join(lines, "\n"), nil
}

func (h *handle) CountCommitsBehind(ref string) (int, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	head, r, err := h.resolveRange("HEAD", ref, "rev-list", "--count", "HEAD.."+ref)
	if err != nil {
		return 0, err
	}
	return len(h.f.rangeCommits(head, r)), nil
}

// checkoutTree moves the checkout at dir from tree old to tree new the way
// git does: files old tracked and new lacks are removed, files new changes
// are written (with force, every file of new, discarding local edits), and
// untracked files are left alone.
func checkoutTree(dir string, old, new map[string]string, force bool) error {
	for p := range old {
		if _, keep := new[p]; !keep {
			path := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			removeEmptyParents(dir, filepath.Dir(path))
		}
	}
	for p, content := range new {
		if base, tracked := old[p]; tracked && base == content && !force {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // G306: a checkout's files are world-readable, as git writes them
			return err
		}
	}
	return nil
}

func removeEmptyParents(root, dir string) {
	for dir != root && strings.HasPrefix(dir, root) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// wouldOverwrite is git's refusal to switch a checkout from tree old to tree
// new when that would overwrite a local change: a tracked file edited on disk
// that the switch changes, or an untracked file new would write over.
func wouldOverwrite(dir string, old, new map[string]string, args ...string) error {
	disk, err := readWorktree(dir)
	if err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	var tracked, untracked []string
	for p, content := range disk {
		base, isTracked := old[p]
		switch {
		case isTracked && content != base && new[p] != base:
			tracked = append(tracked, p)
		case !isTracked && hasKey(new, p) && new[p] != content:
			untracked = append(untracked, p)
		}
	}
	for p := range old {
		if _, onDisk := disk[p]; !onDisk && hasKey(new, p) && new[p] != old[p] {
			tracked = append(tracked, p)
		}
	}
	sort.Strings(tracked)
	sort.Strings(untracked)
	switch {
	case len(tracked) > 0:
		return gitErr(1, "error: Your local changes to the following files would be overwritten by checkout:\n\t"+strings.Join(tracked, "\n\t")+"\nPlease commit your changes or stash them before you switch branches.\nAborting", args...)
	case len(untracked) > 0:
		return gitErr(1, "error: The following untracked working tree files would be overwritten by checkout:\n\t"+strings.Join(untracked, "\n\t")+"\nPlease move or remove them before you switch branches.\nAborting", args...)
	}
	return nil
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// moveHead points wt's HEAD at head (a branch ref or a commit id) and updates
// the checkout from the old HEAD's tree to the new one, refusing as git does
// when that would overwrite a local change.
func (h *handle) moveHead(r *repo, wt *worktree, head string, args ...string) error {
	old := h.f.treeOf(headCommit(r, wt))
	next := head
	if strings.HasPrefix(head, "refs/") {
		next = r.refs[head]
	}
	if err := wouldOverwrite(wt.path, old, h.f.treeOf(next), args...); err != nil {
		return err
	}
	if wt == r.main {
		r.head = head
	} else {
		wt.head = head
	}
	return checkoutTree(wt.path, old, h.f.treeOf(headCommit(r, wt)), false)
}

func alreadyUsed(branch, at string, args ...string) error {
	return gitErr(128, fmt.Sprintf("fatal: '%s' is already used by worktree at '%s'", branch, at), args...)
}

func (h *handle) Checkout(ref string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"checkout", ref}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	if _, ok := r.refs["refs/heads/"+ref]; ok {
		if at := checkedOutAt(r, ref); at != "" && at != wt.path {
			return alreadyUsed(ref, at, args...)
		}
		return h.moveHead(r, wt, "refs/heads/"+ref, args...)
	}
	// git's DWIM: a name only a remote has becomes a local tracking branch.
	if id, ok := r.refs["refs/remotes/origin/"+ref]; ok {
		if err := wouldOverwrite(wt.path, h.f.treeOf(headCommit(r, wt)), h.f.treeOf(id), args...); err != nil {
			return err
		}
		r.refs["refs/heads/"+ref] = id
		setUpstream(r, ref, "origin/"+ref)
		return h.moveHead(r, wt, "refs/heads/"+ref, args...)
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return gitErr(1, fmt.Sprintf("error: pathspec '%s' did not match any file(s) known to git", ref), args...)
	}
	return h.moveHead(r, wt, id, args...)
}

func (h *handle) checkoutBranch(branch, startPoint string, reset bool, args ...string) error {
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, startPoint)
	if !ok {
		return gitErr(128, fmt.Sprintf("fatal: '%s' is not a commit and a branch '%s' cannot be created from it", startPoint, branch), args...)
	}
	if _, exists := r.refs["refs/heads/"+branch]; exists {
		if !reset {
			return gitErr(128, fmt.Sprintf("fatal: a branch named '%s' already exists", branch), args...)
		}
		if at := checkedOutAt(r, branch); at != "" && at != wt.path {
			return alreadyUsed(branch, at, args...)
		}
	}
	old := h.f.treeOf(headCommit(r, wt))
	if err := wouldOverwrite(wt.path, old, h.f.treeOf(id), args...); err != nil {
		return err
	}
	r.refs["refs/heads/"+branch] = id
	setUpstream(r, branch, startPoint)
	if wt == r.main {
		r.head = "refs/heads/" + branch
	} else {
		wt.head = "refs/heads/" + branch
	}
	return checkoutTree(wt.path, old, h.f.treeOf(id), false)
}

func (h *handle) CheckoutNewBranch(branch, startPoint string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.checkoutBranch(branch, startPoint, false, "checkout", "-b", branch, startPoint)
}

func (h *handle) CheckoutResetBranch(branch, startPoint string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.checkoutBranch(branch, startPoint, true, "checkout", "-B", branch, startPoint)
}

func (h *handle) CheckoutDetachForce(ref string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"checkout", "--detach", "--force", ref}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return unknownRevision(ref, args...)
	}
	old := h.f.treeOf(headCommit(r, wt))
	wt.merge = nil
	if wt != r.main {
		wt.head = id
	} else {
		r.head = id
	}
	return checkoutTree(wt.path, old, h.f.treeOf(id), true)
}

func (h *handle) ResetHard(ref string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"reset", "--hard", ref}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return unknownRevision(ref, args...)
	}
	old := h.f.treeOf(headCommit(r, wt))
	wt.merge = nil
	setHead(r, wt, id)
	return checkoutTree(wt.path, old, h.f.treeOf(id), true)
}

// CleanForce is git clean -fd --exclude=.runtime: untracked files and
// directories go; ignored ones and .runtime stay.
func (h *handle) CleanForce() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"clean", "-fd", "--exclude=.runtime"}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	tracked := h.f.treeOf(headCommit(r, wt))
	current, err := readWorktree(wt.path)
	if err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	for p := range current {
		if _, ok := tracked[p]; ok || p == ".runtime" || strings.HasPrefix(p, ".runtime/") {
			continue
		}
		path := filepath.Join(wt.path, filepath.FromSlash(p))
		if err := os.Remove(path); err != nil {
			return gitErr(1, "warning: failed to remove "+p+": "+err.Error(), args...)
		}
		removeEmptyParents(wt.path, filepath.Dir(path))
	}
	return nil
}
