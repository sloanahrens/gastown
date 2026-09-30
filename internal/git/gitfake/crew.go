package gitfake

import (
	"fmt"
	"sort"
	"strings"
)

// Clone clones url to dest on url's HEAD branch, as git clone does; an empty
// repository clones to an empty checkout with no commits.
func (h *handle) Clone(url, dest string) error {
	return h.clone(url, dest, "", false)
}

// CloneWithReference is Clone; the fake shares no objects, so a reference
// changes nothing it can observe.
func (h *handle) CloneWithReference(url, dest, _ string) error {
	return h.clone(url, dest, "", false)
}

// Remotes lists the repository's remote names, sorted as git remote prints
// them.
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

// AddRemote adds a remote; git refuses a name that exists.
func (h *handle) AddRemote(name, url string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "add", name, url}
	r, _, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	if _, ok := r.remotes[name]; ok {
		return "", gitErr(3, fmt.Sprintf("error: remote %s already exists.", name), args...)
	}
	r.remotes[name] = url
	return "", nil
}

// SetRemoteURL changes a remote's URL; its push URL, when one is set, stays.
func (h *handle) SetRemoteURL(name, url string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "set-url", name, url}
	r, _, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	if _, ok := r.remotes[name]; !ok {
		return "", noSuchRemote(name, args...)
	}
	r.remotes[name] = url
	return "", nil
}

// CreateBranch makes a branch at HEAD without checking it out.
func (h *handle) CreateBranch(name string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"branch", name}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	head := headCommit(r, wt)
	if head == "" {
		return gitErr(128, fmt.Sprintf("fatal: not a valid object name: '%s'", strings.TrimPrefix(headOf(r, wt), "refs/heads/")), args...)
	}
	if _, ok := r.refs["refs/heads/"+name]; ok {
		return gitErr(128, fmt.Sprintf("fatal: a branch named '%s' already exists", name), args...)
	}
	r.refs["refs/heads/"+name] = head
	return nil
}

// HasUncommittedChanges reports whether Status finds anything.
func (h *handle) HasUncommittedChanges() (bool, error) {
	st, err := h.Status()
	if err != nil {
		return false, err
	}
	return !st.Clean, nil
}

// Pull fetches branch from remote (the remote's HEAD branch when branch is
// "") and fast-forwards the checkout to it. A history that has diverged is
// refused, as git does with no pull.rebase or pull.ff configured.
func (h *handle) Pull(remote, branch string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"pull", remote, branch}
	r, wt, err := h.workTree(args...)
	if err != nil {
		return err
	}
	rr, err := h.remoteRepo(r, remote, args...)
	if err != nil {
		return err
	}
	src := rr.head
	if branch != "" {
		src = "refs/heads/" + branch
	}
	id, ok := rr.refs[src]
	if !ok {
		return gitErr(1, "fatal: couldn't find remote ref "+strings.TrimPrefix(src, "refs/heads/"), args...)
	}
	h.f.copyObjects(r, id)
	r.refs["refs/remotes/"+remote+"/"+strings.TrimPrefix(src, "refs/heads/")] = id
	head := headCommit(r, wt)
	switch {
	case head != "" && h.f.isAncestor(id, head):
		return nil // Already up to date.
	case head == "" || h.f.isAncestor(head, id):
		old := h.f.treeOf(head)
		if err := wouldOverwrite(wt.path, old, h.f.treeOf(id), args...); err != nil {
			return err
		}
		setHead(r, wt, id)
		return checkoutTree(wt.path, old, h.f.treeOf(id), false)
	default:
		return gitErr(128, "hint: You have divergent branches and need to specify how to reconcile them.\nfatal: Need to specify how to reconcile divergent branches.", args...)
	}
}
