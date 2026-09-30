package gitfake

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The remote, path-state and repair calls internal/doctor's checks make.
// Known limits: PullRebase only fast-forwards (a branch that diverged from
// its upstream is refused, where git would replay it), and
// DisableSparseCheckout only records the setting, since the fake does not
// hide files for a sparse checkout.

// Remotes returns the repository's remote names, sorted as git lists them.
func (h *handle) Remotes() ([]string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("remote")
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range r.remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// RemoveRemote deletes a remote with its push URL and remote-tracking refs.
func (h *handle) RemoveRemote(name string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "remove", name}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	if _, ok := r.remotes[name]; !ok {
		return gitErr(2, fmt.Sprintf("error: No such remote: '%s'", name), args...)
	}
	delete(r.remotes, name)
	delete(r.pushURLs, name)
	for ref := range r.refs {
		if strings.HasPrefix(ref, "refs/remotes/"+name+"/") {
			delete(r.refs, ref)
		}
	}
	return nil
}

// PullRebase fetches the current branch's upstream and moves the branch to
// it when the branch has nothing of its own.
func (h *handle) PullRebase() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"pull", "--rebase"}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	branch, onBranch := strings.CutPrefix(headOf(r, wt), "refs/heads/")
	remote := r.configMap()["branch."+branch+".remote"]
	merge := strings.TrimPrefix(r.configMap()["branch."+branch+".merge"], "refs/heads/")
	if !onBranch || remote == "" || merge == "" {
		return gitErr(1, "There is no tracking information for the current branch.", args...)
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	if err := h.fetchInto(r, rr, remote, merge); err != nil {
		return err
	}
	upstream := r.refs["refs/remotes/"+remote+"/"+merge]
	head := headCommit(r, wt)
	switch {
	case h.f.isAncestor(upstream, head):
		return nil // up to date
	case h.f.isAncestor(head, upstream):
		old := h.f.treeOf(head)
		r.refs["refs/heads/"+branch] = upstream
		return checkoutTree(wt.path, old, h.f.treeOf(upstream), false)
	}
	return gitErr(1, "gitfake: pull --rebase of a branch that diverged from its upstream is not modeled", args...)
}

// CheckoutDetach detaches HEAD at ref, which a bare repository refuses.
func (h *handle) CheckoutDetach(ref string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"checkout", "--detach", ref}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	id, ok := h.resolve(r, wt, ref)
	if !ok {
		return unknownRevision(ref, args...)
	}
	return h.moveHead(r, wt, id, args...)
}

func (h *handle) IsBareRepository() (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("rev-parse", "--is-bare-repository")
	if err != nil {
		return false, err
	}
	return r.bare && wt == nil, nil
}

// checkoutPath is path (relative to the handle's directory) relative to the
// root of the checkout holding it.
func (h *handle) checkoutPath(wtPath, path string) (string, error) {
	rel, err := filepath.Rel(wtPath, filepath.Join(h.dir, path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("gitfake: %s is outside the checkout at %s", path, wtPath)
	}
	return filepath.ToSlash(rel), nil
}

// IsTracked reports whether path is in the index.
func (h *handle) IsTracked(path string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("ls-files", "--", path)
	if err != nil {
		return false, err
	}
	p, err := h.checkoutPath(wt.path, path)
	if err != nil {
		return false, err
	}
	for f := range h.f.stage(r, wt).tree {
		if underSpec(f, p) {
			return true, nil
		}
	}
	return false, nil
}

// IsIgnored reports whether the checkout's .gitignore files or info/exclude
// ignore path.
func (h *handle) IsIgnored(path string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	_, wt, err := h.workTree("check-ignore", "-q", "--", path)
	if err != nil {
		return false, err
	}
	p, err := h.checkoutPath(wt.path, path)
	if err != nil {
		return false, err
	}
	disk, err := readWorkTree(wt.path)
	if err != nil {
		return false, err
	}
	return isIgnored(checkoutRules(wt.path, disk), p), nil
}

// PathChanged reports whether path differs between HEAD and the index, or
// between the index and disk.
func (h *handle) PathChanged(path string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("diff", "--quiet", "--", path)
	if err != nil {
		return false, err
	}
	p, err := h.checkoutPath(wt.path, path)
	if err != nil {
		return false, err
	}
	head := h.f.treeOf(headCommit(r, wt))
	ix := h.f.stage(r, wt).tree
	disk, err := readWorkTree(wt.path)
	if err != nil {
		return false, err
	}
	for f := range unionPaths(head, ix) {
		if !underSpec(f, p) {
			continue
		}
		hc, inHead := head[f]
		ic, inIndex := ix[f]
		dc, onDisk := disk[f]
		if inHead != inIndex || hc != ic || (inIndex && (!onDisk || dc != ic)) {
			return true, nil
		}
	}
	return false, nil
}

func unionPaths(trees ...map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, t := range trees {
		for p := range t {
			out[p] = true
		}
	}
	return out
}

// UntrackedPaths returns the untracked, unignored files under pathspec, with
// a directory holding nothing tracked or ignored listed once, as "dir/".
func (h *handle) UntrackedPaths(pathspec string) ([]string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("status", "--porcelain", "--ignored", "--", pathspec)
	if err != nil {
		return nil, err
	}
	spec, err := h.checkoutPath(wt.path, pathspec)
	if err != nil {
		return nil, err
	}
	head := h.f.treeOf(headCommit(r, wt))
	ix := h.f.stage(r, wt).tree
	disk, err := readWorkTree(wt.path)
	if err != nil {
		return nil, err
	}
	rules := checkoutRules(wt.path, disk)
	var untracked, other []string // other: tracked or ignored files under spec
	for p := range disk {
		if !underSpec(p, spec) {
			continue
		}
		_, inHead := head[p]
		_, inIndex := ix[p]
		if !inHead && !inIndex && !isIgnored(rules, p) {
			untracked = append(untracked, p)
		} else {
			other = append(other, p)
		}
	}
	sort.Strings(untracked)
	// git collapses a directory whose every file is untracked into one entry.
	if len(untracked) > 0 && len(other) == 0 && spec != "." && spec != "" && !contains(untracked, spec) {
		return []string{spec + "/"}, nil
	}
	return untracked, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// DisableSparseCheckout records sparse checkout as off.
func (h *handle) DisableSparseCheckout() error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.workTree("sparse-checkout", "disable")
	if err != nil {
		return err
	}
	r.configMap()["core.sparseCheckout"] = "false"
	return nil
}

// FetchDefaultBranchWithTimeout fetches the remote's default branch into its
// remote-tracking ref, as git fetch <remote> <branch> does under the refspec
// a clone configures.
func (h *handle) FetchDefaultBranchWithTimeout(remote string, timeout time.Duration) error {
	branch := h.RemoteDefaultBranch()
	return h.FetchRefspecWithTimeout(remote, branch+":refs/remotes/"+remote+"/"+branch, timeout)
}
