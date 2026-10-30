package land

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/git"
)

// LandingCommitterName and LandingCommitterEmail are the committer every
// landing commit carries: automation made the commit, not the human whose git
// config the landing worktree inherits (gt-dc4y3, gt-vuv2n).
const (
	LandingCommitterName  = "gt-landing"
	LandingCommitterEmail = "gt-landing@gastown.local"
)

// Identity is a commit author: a name and an email.
type Identity struct {
	Name  string
	Email string
}

// authorReader is the Git read a landing takes the head's author from: the
// author of one commit. The production handle has it. A handle without it
// cannot name the author, so the landing fails closed rather than falling back
// to git config (gt-vuv2n).
type authorReader interface {
	CommitIdentity(rev string) (name, email string, err error)
}

// CommittingEnv is the environment a merge commit is made under: the landing
// committer, and the author of the range's head, the seat that did the work
// (gt-vuv2n). All four identity variables are set explicitly, so neither git
// config nor an inherited GIT_AUTHOR_* or GIT_COMMITTER_* reaches the commit.
// An author with no name or no email is an error, never a fallback. The
// entries are environment entries, not -c user.name, so the landing never
// reads or writes git config.
func CommittingEnv(author Identity) ([]string, error) {
	if author.Name == "" || author.Email == "" {
		return nil, fmt.Errorf("landing author has no name or no email (name set: %t, email set: %t)", author.Name != "", author.Email != "")
	}
	return []string{
		"GIT_COMMITTER_NAME=" + LandingCommitterName,
		"GIT_COMMITTER_EMAIL=" + LandingCommitterEmail,
		"GIT_AUTHOR_NAME=" + author.Name,
		"GIT_AUTHOR_EMAIL=" + author.Email,
	}, nil
}

// RevertEnv is the environment a revert commit is made under. A revert has no
// seat author: automation made it, so the landing committer is the author too,
// and all four identity variables are set explicitly (gt-vuv2n).
func RevertEnv() []string {
	return []string{
		"GIT_COMMITTER_NAME=" + LandingCommitterName,
		"GIT_COMMITTER_EMAIL=" + LandingCommitterEmail,
		"GIT_AUTHOR_NAME=" + LandingCommitterName,
		"GIT_AUTHOR_EMAIL=" + LandingCommitterEmail,
	}
}

// RevertGit opens dir as the Git a revert commit is made through, with
// RevertEnv set on every call (gt-vuv2n).
func RevertGit(dir string) *git.Git {
	return git.NewGit(dir).WithEnv(RevertEnv())
}

// CommittingGit opens dir as the Git a landing's reads are made through, with
// the landing committer set on every call. The entries land after the
// process's own, and os/exec keeps the last entry for a key, so an inherited
// GIT_COMMITTER_* loses (gt-dc4y3). A landing's merge commit is made through a
// handle opened with CommittingEnv instead, which also sets the author, and a
// revert commit through RevertGit.
func CommittingGit(dir string) *git.Git {
	return git.NewGit(dir).WithEnv([]string{
		"GIT_COMMITTER_NAME=" + LandingCommitterName,
		"GIT_COMMITTER_EMAIL=" + LandingCommitterEmail,
	})
}
