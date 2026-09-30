package gitfake

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// clone is git clone --single-branch of url into dest, bare or checked out,
// then (for a bare clone) the fetch of that branch into its remote-tracking
// ref that *git.Git's bare clones make. branch "" is the remote's HEAD
// branch. An empty remote clones to an empty repository; a named branch it
// lacks fails.
func (h *handle) clone(url, dest, branch string, bare bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"clone", "--single-branch"}
	if bare {
		args = append(args, "--bare")
	}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dest)
	src := h.f.repos[clean(url)]
	if src == nil {
		return gitErr(128, fmt.Sprintf("fatal: repository '%s' does not exist", url), args...)
	}
	dest = clean(dest)
	head := src.head
	if branch != "" {
		head = "refs/heads/" + branch
		if _, ok := src.refs[head]; !ok {
			return gitErr(128, fmt.Sprintf("warning: Could not find remote branch %s to clone.\nfatal: Remote branch %s not found in upstream origin", branch, branch), args...)
		}
	}
	r := &repo{path: dest, bare: bare, refs: map[string]string{}, head: head, remotes: map[string]string{"origin": url},
		has: map[string]bool{}, worktrees: map[string]*worktree{}}
	if !bare {
		r.main = &worktree{path: dest}
	}
	if id, ok := src.refs[head]; ok {
		h.f.copyObjects(r, id)
		r.refs[head] = id
		r.refs["refs/remotes/origin/"+strings.TrimPrefix(head, "refs/heads/")] = id
	}
	write := func() error { return layoutRepo(r) }
	if !bare {
		write = func() error {
			if err := materializeCheckout(dest, h.f.treeOf(r.refs[head])); err != nil {
				return err
			}
			return layoutRepo(r)
		}
	}
	if err := write(); err != nil {
		return gitErr(128, "fatal: "+err.Error(), args...)
	}
	h.f.repos[dest] = r
	return nil
}

func (h *handle) CloneBareWithBranch(url, dest, branch string) error {
	return h.clone(url, dest, branch, true)
}

func (h *handle) CloneBareWithReferenceAndBranch(url, dest, _, branch string) error {
	return h.clone(url, dest, branch, true)
}

func (h *handle) CloneBarePartialWithBranch(url, dest, _, branch string) error {
	return h.clone(url, dest, branch, true)
}

func (h *handle) CloneBarePartialWithReferenceAndBranch(url, dest, _, _, branch string) error {
	return h.clone(url, dest, branch, true)
}

func (h *handle) CloneBranch(url, dest, branch string) error {
	return h.clone(url, dest, branch, false)
}

func (h *handle) CloneBranchWithReference(url, dest, branch, _ string) error {
	return h.clone(url, dest, branch, false)
}

func (h *handle) CloneBranchPartial(url, dest, branch, _ string) error {
	return h.clone(url, dest, branch, false)
}

func (h *handle) CloneBranchPartialWithReference(url, dest, branch, _, _ string) error {
	return h.clone(url, dest, branch, false)
}

// RemoteHasRefs is ls-remote --refs: whether the remote (a configured remote
// of this repository, or a path) has any ref at all.
func (h *handle) RemoteHasRefs(remote string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"ls-remote", "--refs", remote}
	url := remote
	if r, _, err := h.locate(args...); err == nil {
		if u, ok := r.remotes[remote]; ok {
			url = u
		}
	}
	rr := h.f.repos[clean(url)]
	if rr == nil {
		return false, gitErr(128, fmt.Sprintf("fatal: '%s' does not appear to be a git repository\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.", remote), args...)
	}
	return len(rr.refs) > 0, nil
}

// FetchBranchShallow fetches branch from remote into its remote-tracking ref.
func (h *handle) FetchBranchShallow(remote, branch string) error {
	return h.FetchRefspecWithTimeout(remote, branch+":refs/remotes/"+remote+"/"+branch, time.Minute)
}

func noSuchRemote(remote string, args ...string) error {
	return gitErr(2, fmt.Sprintf("error: No such remote '%s'", remote), args...)
}

func (h *handle) RemoteURL(remote string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "get-url", remote}
	r, _, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	url, ok := r.remotes[remote]
	if !ok {
		return "", noSuchRemote(remote, args...)
	}
	return url, nil
}

// GetPushURL is the push URL when one is set, else the fetch URL, as git
// remote get-url --push reports it.
func (h *handle) GetPushURL(remote string) (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "get-url", "--push", remote}
	r, _, err := h.locate(args...)
	if err != nil {
		return "", err
	}
	url, ok := r.remotes[remote]
	if !ok {
		return "", noSuchRemote(remote, args...)
	}
	if push, ok := r.pushURLs[remote]; ok {
		return push, nil
	}
	return url, nil
}

func (h *handle) ConfigurePushURL(remote, pushURL string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"remote", "set-url", remote, "--push", pushURL}
	r, _, err := h.locate(args...)
	if err != nil {
		return err
	}
	if _, ok := r.remotes[remote]; !ok {
		return noSuchRemote(remote, args...)
	}
	if r.pushURLs == nil {
		r.pushURLs = map[string]string{}
	}
	r.pushURLs[remote] = pushURL
	return nil
}

// ClearPushURL unsets the push URL; unsetting one that is not set succeeds.
func (h *handle) ClearPushURL(remote string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("config", "--unset-all", "remote."+remote+".pushurl")
	if err != nil {
		return err
	}
	delete(r.pushURLs, remote)
	return nil
}

// AddUpstreamRemote adds the upstream remote, or points it at upstreamURL.
func (h *handle) AddUpstreamRemote(upstreamURL string) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("remote", "add", "upstream", upstreamURL)
	if err != nil {
		return err
	}
	r.remotes["upstream"] = upstreamURL
	return nil
}

func (h *handle) IsRepo() bool {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	_, _, err := h.locate()
	return err == nil
}

// IsEmpty reports whether the repository has no refs at all.
func (h *handle) IsEmpty() (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("show-ref")
	if err != nil {
		return false, err
	}
	return len(r.refs) == 0, nil
}

// DefaultBranch is the branch HEAD names, or "main" when HEAD is detached or
// dir is no repository.
func (h *handle) DefaultBranch() string {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate()
	if err != nil {
		return "main"
	}
	if branch, ok := strings.CutPrefix(headOf(r, wt), "refs/heads/"); ok {
		return branch
	}
	return "main"
}

// RefExists looks a full ref name up exactly, and anything else the way
// rev-parse --verify resolves it.
func (h *handle) RefExists(ref string) (bool, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.locate("rev-parse", "--verify", ref)
	if err != nil {
		return false, err
	}
	if strings.HasPrefix(ref, "refs/") {
		_, ok := r.refs[ref]
		return ok, nil
	}
	_, ok := h.resolve(r, wt, ref)
	return ok, nil
}

// CommonDir is the git directory a repository shares with its worktrees: the
// bare repository itself, or the main checkout's .git.
func (h *handle) CommonDir() (string, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, _, err := h.locate("rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if r.bare {
		return r.path, nil
	}
	return filepath.Join(r.path, ".git"), nil
}
