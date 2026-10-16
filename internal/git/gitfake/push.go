package gitfake

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// Push pushes refspec (a branch, or src:dst) to remote at its push URL when
// one is set, else its URL, as git push does. A non-fast-forward update is
// rejected unless force; a successful push moves the remote-tracking ref.
func (h *handle) Push(remote, refspec string, force bool) error {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	args := []string{"push", remote, refspec}
	if force {
		args = append(args, "--force")
	}
	r, wt, err := h.locate(args...)
	if err != nil {
		return err
	}
	url, ok := r.pushURLs[remote]
	if !ok {
		url, ok = r.remotes[remote]
	}
	if !ok {
		url = remote
	}
	if !filepath.IsAbs(url) {
		url = filepath.Join(r.path, url)
	}
	rr := h.f.repos[clean(url)]
	if rr == nil {
		return gitErr(128, fmt.Sprintf("fatal: '%s' does not appear to be a git repository\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.", url), args...)
	}
	src, dst, found := strings.Cut(refspec, ":")
	if !found {
		dst = src
	}
	id, ok := h.resolve(r, wt, src)
	if !ok {
		return gitErr(1, "error: src refspec "+src+" does not match any\nerror: failed to push some refs to '"+url+"'", args...)
	}
	dstRef := dst
	if !strings.HasPrefix(dstRef, "refs/") {
		dstRef = "refs/heads/" + dst
	}
	cur := rr.refs[dstRef]
	if path, size, over := h.f.oversizePushBlob(rr, cur, id); over {
		return gitErr(1, fmt.Sprintf("remote: error: GH001: Large files detected. You may want to try Git Large File Storage - https://git-lfs.github.com.\nremote: error: File %s is %.2f MB; this exceeds GitHub's file size limit of %.2f MB\nTo %s\n ! [remote rejected] %s -> %s (pre-receive hook declined)\nerror: failed to push some refs to '%s'",
			path, float64(size)/1e6, float64(h.f.pushFileLimit)/1e6, url, src, dst, url), args...)
	}
	if cur != "" && !force && !h.f.isAncestor(cur, id) {
		return gitErr(1, fmt.Sprintf("To %s\n ! [rejected]        %s -> %s (non-fast-forward)\nerror: failed to push some refs to '%s'", url, src, dst, url), args...)
	}
	h.f.copyObjects(rr, id)
	rr.refs[dstRef] = id
	if branch, ok := strings.CutPrefix(dstRef, "refs/heads/"); ok {
		if _, isRemote := r.remotes[remote]; isRemote {
			r.refs["refs/remotes/"+remote+"/"+branch] = id
		}
	}
	return nil
}

// oversizePushBlob reports one file that pushing id to rr would send over the
// world's per-file push limit, with its size: the first in path order, so the
// refusal names the same file every time. A push sends the objects reachable
// from id that rr does not already hold, which is why a blob living only in an
// unpushed commit blocks a push whose own tree no longer holds it (the shape
// of the failure the JSONL backup recovers from, gt-q30zx). ok is false when
// the world sets no limit, or when every file being sent fits.
func (f *Fake) oversizePushBlob(rr *repo, cur, id string) (path string, size int64, ok bool) {
	if f.pushFileLimit <= 0 {
		return "", 0, false
	}
	// The remote's own objects are not sent, so they are not weighed.
	held := map[string]bool{}
	for c := range rr.has {
		if obj := f.objects[c]; obj != nil {
			for _, content := range obj.tree {
				held[blobID(content)] = true
			}
		}
	}
	for _, c := range f.rangeCommits(cur, id) {
		for p, content := range c.tree {
			n := int64(len(content))
			if n <= f.pushFileLimit || held[blobID(content)] {
				continue
			}
			if !ok || p < path {
				path, size, ok = p, n, true
			}
		}
	}
	return path, size, ok
}

// StashCount is the number of stash entries made on the checkout's branch
// (Fake.Stash records them), as git.Git's StashCount filters them.
func (h *handle) StashCount() (int, error) {
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	r, wt, err := h.workTree("stash", "list")
	if err != nil {
		return 0, err
	}
	return stashCount(r, wt), nil
}

// CheckUncommittedWorkLocalFailClosed reports the checkout's uncommitted
// work, stashes and unpushed commits, reading origin only from the
// remote-tracking refs the checkout holds.
//
// Unpushed commits are judged by ancestry alone: zero when origin's copy of
// the current branch contains HEAD; otherwise the commits HEAD holds that
// neither that branch nor origin's default branch does; and 1 (fail closed)
// when neither ref exists. Git also counts a commit whose patch origin
// already carries (cherry) or whose merge changes nothing as preserved; the
// fake does not.
func (h *handle) CheckUncommittedWorkLocalFailClosed() (*git.UncommittedWorkStatus, error) {
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
	head := headCommit(r, wt)
	var candidates []string
	if branch, ok := strings.CutPrefix(headOf(r, wt), "refs/heads/"); ok {
		if tip, ok := r.refs["refs/remotes/origin/"+branch]; ok {
			if h.f.isAncestor(head, tip) {
				return status, nil
			}
			candidates = append(candidates, tip)
		}
	}
	for _, def := range []string{"master", "main"} {
		if tip, ok := r.refs["refs/remotes/origin/"+def]; ok {
			candidates = append(candidates, tip)
			break
		}
	}
	if len(candidates) == 0 {
		status.UnpushedCommits = 1
		return status, nil
	}
	preserved := map[string]bool{}
	for _, c := range candidates {
		for _, a := range h.f.ancestors(c) {
			preserved[a] = true
		}
	}
	for _, a := range h.f.ancestors(head) {
		if !preserved[a] {
			status.UnpushedCommits++
		}
	}
	return status, nil
}

// PushWithTimeout is Push; the fake takes no time.
func (h *handle) PushWithTimeout(remote, refspec string, force bool, _ time.Duration) error {
	return h.Push(remote, refspec, force)
}

// PushWithEnv is Push; the fake models no process environment, so the
// variables git would run with (a deploy key's GIT_SSH_COMMAND) change
// nothing here.
func (h *handle) PushWithEnv(remote, refspec string, force bool, _ []string) error {
	return h.Push(remote, refspec, force)
}
