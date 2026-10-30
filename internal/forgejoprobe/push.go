package forgejoprobe

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Pusher sends the probe's candidate to the Forgejo copy as the probe branch.
// When the rig's own repository is present the push rides a throwaway worktree
// of it, so the rig's pre-push hook runs; when it is absent the push rides the
// clone, which carries no hooks, and the run says so.
type Pusher struct {
	Git        Git
	Bare       string                                 // the rig's own repository, <town>/<rig>/.repo.git
	Copy       string                                 // the full clone of the GitHub repository
	Work       string                                 // the probe's temporary directory; the worktree is Work/wt
	GitURL     string                                 // the Forgejo git remote the branch is pushed to
	MainBranch string                                 // the branch the copy is taken from
	Branch     string                                 // the probe branch, land/probe-<rig>
	Stat       func(name string) (fs.FileInfo, error) // nil means os.Stat
	Log        func(msg string)                       // nil discards

	worktree bool // a worktree of Bare was added and Cleanup must remove it
}

// Push sends sha to the Forgejo copy as the probe branch. A push the rig's hook
// refuses fails with the hook's own output, which is the finding the probe
// exists to produce.
func (p *Pusher) Push(sha string) error {
	for _, v := range []struct{ what, value string }{
		{"commit", sha},
		{"git URL", p.GitURL},
		{"main branch", p.MainBranch},
		{"branch", p.Branch},
		{"probe worktree", p.worktreePath()},
	} {
		if strings.HasPrefix(v.value, "-") {
			return fmt.Errorf("refusing the %s: it starts with -", v.what)
		}
	}
	_, err := p.stat(p.Bare)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not stat the rig repository %s: %w", p.Bare, err)
	}
	if err == nil {
		return p.pushFromWorktree(sha)
	}
	p.log("warning: no rig repository at " + p.Bare + ", so the probe pushes from the clone and the rig's hooks were not exercised")
	return p.push(p.Copy, sha, "refs/remotes/origin/"+p.MainBranch+":refs/heads/"+p.Branch)
}

// pushFromWorktree cuts a detached worktree of the rig's repository at sha and
// pushes from it. The worktree is remembered only once it exists, so Cleanup
// removes nothing a failed add did not create.
func (p *Pusher) pushFromWorktree(sha string) error {
	if _, err := p.Git.Run(p.Bare, nil, "rev-parse", "--verify", "--quiet", sha+"^{commit}"); err != nil {
		return fmt.Errorf("the rig repository %s has no %s, the %s the probe would push; fetch %s in %s and run the probe again, since the hooks it must exercise live there",
			p.Bare, sha, p.MainBranch, p.MainBranch, p.Bare)
	}
	wt := p.worktreePath()
	// LFS smudging is off, as internal/git's WorktreeAddDetached sets it: the
	// worktree is cut for its .githooks, not for a checkout of the tree.
	if _, err := p.Git.Run(p.Bare, []string{"GIT_LFS_SKIP_SMUDGE=1"}, "worktree", "add", "--detach", wt, sha); err != nil {
		return fmt.Errorf("could not add a worktree of %s at %s under %s", p.Bare, sha, wt)
	}
	p.worktree = true
	p.log("pushing from a throwaway worktree of " + p.Bare + ", so the rig's hooks run")
	return p.push(wt, sha, sha+":refs/heads/"+p.Branch)
}

// push sends refspec from dir to the git URL. Its failure carries git's output,
// with the URL replaced by its redacted form, since a hook's refusal is the
// finding and the URL may hold a credential.
func (p *Pusher) push(dir, sha, refspec string) error {
	if _, err := p.Git.Run(dir, nil, "push", "--quiet", "--force", "--", p.GitURL, refspec); err != nil {
		return fmt.Errorf("git push of %s (%s) to %s as %s failed from %s: %s",
			p.MainBranch, sha, redactedURL(p.GitURL), p.Branch, dir, redact(err.Error(), p.GitURL))
	}
	return nil
}

// Cleanup removes the worktree a push added from the rig's repository. It
// removes and never prunes: a prune would churn the registrations of other
// landings sharing that repository. A second call does nothing.
func (p *Pusher) Cleanup() error {
	if !p.worktree {
		return nil
	}
	p.worktree = false
	wt := p.worktreePath()
	if _, err := p.Git.Run(p.Bare, nil, "worktree", "remove", "--force", wt); err != nil {
		p.log("warning: the gate verdict above stands, but the probe worktree " + wt + " could not be removed from " + p.Bare + "; run 'git -C " + p.Bare + " worktree prune' to clear the registration")
		return fmt.Errorf("could not remove the probe worktree %s from %s", wt, p.Bare)
	}
	return nil
}

// worktreePath is where the probe's worktree is cut: Work/wt.
func (p *Pusher) worktreePath() string {
	return filepath.Join(p.Work, "wt")
}

// stat reads the rig repository's path through the Stat seam.
func (p *Pusher) stat(name string) (fs.FileInfo, error) {
	if p.Stat != nil {
		return p.Stat(name)
	}
	return os.Stat(name)
}

// log writes one line through the Log seam.
func (p *Pusher) log(msg string) {
	if p.Log != nil {
		p.Log(msg)
	}
}

// redactedURL is the git URL as an error may carry it: net/url's Redacted form,
// so the password of user:password@ never reaches a message.
func redactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<repository URL>"
	}
	return u.Redacted()
}
