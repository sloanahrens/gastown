package polecatimage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// buildTailLines is how much of a failed build reaches the log: the end is what
// explains the failure, and the middle is progress noise.
const buildTailLines = 15

// shortCommit is the abbreviated form the log lines name a commit by.
func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}

// BuildAt builds the image from a detached worktree of the rig repo at commit,
// under BuildRoot, and checks the label it ends up with.
//
// The worktree it makes stays in Worktree for Cleanup, so a failure part way
// through leaves nothing behind; the error it returns has already been logged.
func (r *Run) BuildAt(commit string) error {
	if !isCommit(commit) {
		return r.failf("the installed commit %s is not a 7-character hex commit", commit)
	}
	resolved := r.call(r.Cfg.Git, "-C", r.Cfg.RigRepo, "rev-parse", "--verify", commit+"^{commit}")
	full := strings.TrimSpace(resolved.combined())
	if resolved.failed() {
		return r.failf("the installed commit %s is not in %s: %s", commit, r.Cfg.RigRepo, full)
	}
	if !isCommit(full) {
		return r.failf("%s resolved %s to %q, which is not a commit", r.Cfg.RigRepo, commit, full)
	}
	r.Worktree = filepath.Join(r.Cfg.BuildRoot, full)
	if err := os.MkdirAll(r.Cfg.BuildRoot, 0o755); err != nil {
		return r.failf("cannot make %s: %v", r.Cfg.BuildRoot, err)
	}
	_ = os.RemoveAll(r.Worktree)
	made := r.call(r.Cfg.Git, "-C", r.Cfg.RigRepo, "worktree", "add", "--detach", r.Worktree, full)
	if made.failed() {
		return r.failf("cannot make a worktree of %s under %s", full, r.Cfg.BuildRoot)
	}
	r.Logf("building %s at %s (BD_SRC=%s)", r.Cfg.Image, shortCommit(full), r.Cfg.BDSrc)
	started := r.clock()
	build := r.in(r.Worktree, r.Cfg.Make, "polecat-image", "BD_SRC="+r.Cfg.BDSrc)
	if build.failed() {
		lines := strings.Split(strings.TrimRight(build.combined(), "\n"), "\n")
		if len(lines) > buildTailLines {
			lines = lines[len(lines)-buildTailLines:]
		}
		fmt.Fprintln(r.writer(), strings.Join(lines, "\n"))
		return r.failf("make polecat-image failed at %s", shortCommit(full))
	}
	unmade := r.call(r.Cfg.Git, "-C", r.Cfg.RigRepo, "worktree", "remove", "--force", r.Worktree)
	if unmade.failed() {
		_ = os.RemoveAll(r.Worktree)
	}
	r.Worktree = ""
	r.Logf("build took %ds at %s", int(r.clock().Sub(started).Seconds()), shortCommit(full))
	if !SameCommit(r.ImageCommit(), full) {
		return r.failf("after the build %s carries '%s', not %s", r.Cfg.Image, r.ImageCommit(), shortCommit(full))
	}
	return nil
}
