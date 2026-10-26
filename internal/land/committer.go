package land

import "github.com/steveyegge/gastown/internal/git"

// LandingCommitterName is the committer name every landing commit carries:
// automation made the commit, not the human whose git config the landing
// worktree inherits (gt-dc4y3).
const LandingCommitterName = "gt-landing"

// CommittingEnv is the environment a landing commit is made under. Only the
// committer name changes: the email stays the repository's and the author the
// range's, so it sets neither GIT_COMMITTER_EMAIL nor GIT_AUTHOR_* (gt-dc4y3).
// An environment entry, not -c user.name: -c would rename the merge commit's
// author too.
func CommittingEnv() []string {
	return []string{"GIT_COMMITTER_NAME=" + LandingCommitterName}
}

// CommittingGit opens dir as the Git a landing commit is made through, with
// CommittingEnv set on every call. The entry lands after the process's own,
// and os/exec keeps the last entry for a key, so an inherited
// GIT_COMMITTER_NAME loses (gt-dc4y3).
func CommittingGit(dir string) *git.Git {
	return git.NewGit(dir).WithEnv(CommittingEnv())
}
