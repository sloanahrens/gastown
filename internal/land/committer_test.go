package land

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// landingEnvValue is the value git reads for key: the last entry, since that
// is the one os/exec keeps when a key appears twice.
func landingEnvValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v, true
		}
	}
	return "", false
}

// A landing commit changes the committer's name and nothing else: the email
// and the author stay the repository's and the range's (gt-dc4y3).
func TestCommittingEnvSetsOnlyTheCommitterName(t *testing.T) {
	t.Parallel()
	env := CommittingEnv()
	if len(env) != 1 || env[0] != "GIT_COMMITTER_NAME="+LandingCommitterName {
		t.Fatalf("CommittingEnv = %q, want only GIT_COMMITTER_NAME=%s", env, LandingCommitterName)
	}
	for _, key := range []string{"GIT_COMMITTER_EMAIL", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"} {
		if v, ok := landingEnvValue(env, key); ok {
			t.Errorf("%s = %q; a landing sets no committer email and no author", key, v)
		}
	}
}

// The merge candidate is committed by the Git the lander opens for its
// worktree, so that handle hands git the landing committer (gt-dc4y3).
func TestLandMergeWorktreeCarriesTheLandingCommitter(t *testing.T) {
	t.Parallel()
	opened := (&Lander{}).open(t.TempDir())
	g, ok := opened.(*git.Git)
	if !ok {
		t.Fatalf("Lander.open returned %T, want *git.Git", opened)
	}
	if v, _ := landingEnvValue(g.Environ(), "GIT_COMMITTER_NAME"); v != LandingCommitterName {
		t.Fatalf("merge worktree GIT_COMMITTER_NAME = %q, want %q", v, LandingCommitterName)
	}
}

// A committer name the environment already carried does not win over the
// landing one: the handle's entry is appended after the process's, and exec
// keeps the last entry for a key (gt-dc4y3). The integration tier repeats
// this against a real inherited environment.
func TestLandingCommitterFollowsAnEarlierCommitterName(t *testing.T) {
	t.Parallel()
	g := CommittingGit(t.TempDir())
	inherited := []string{"GIT_COMMITTER_NAME=Sloan Ahrens"}
	if v, _ := landingEnvValue(append(inherited, g.Environ()...), "GIT_COMMITTER_NAME"); v != LandingCommitterName {
		t.Fatalf("effective GIT_COMMITTER_NAME = %q, want %q; handle env %q", v, LandingCommitterName, g.Environ())
	}
}
