package gitfake

import (
	"strings"
	"testing"
)

// RunWorkTreeContract checks the WorkTree behavior every implementation must
// share, over the same fixtures as RunRepoContract. A case that fails
// against *git.Git means the case is wrong; correct it to what git does,
// then make the fake copy it.
func RunWorkTreeContract(t *testing.T, newEnv func(t *testing.T) Env) {
	t.Run("CommitTime is the committer time of a commit", func(t *testing.T) {
		fx := newFixture(t, newEnv(t))
		g := fx.env.Open(fx.clone).(WorkTree)
		base, err := g.CommitTime(fx.base)
		if err != nil {
			t.Fatalf("CommitTime(base): %v", err)
		}
		head, err := g.CommitTime("origin/" + fixtureBranch)
		if err != nil {
			t.Fatalf("CommitTime(origin/%s): %v", fixtureBranch, err)
		}
		// The fixtures' commits are dated a second apart from a fixed epoch.
		if base.Unix() != commitEpoch+1 || head.Unix() != commitEpoch+2 {
			t.Errorf("CommitTime = %v, %v; want %d and %d", base, head, commitEpoch+1, commitEpoch+2)
		}
		if _, err := g.CommitTime(strings.Repeat("1", 40)); err == nil {
			t.Error("CommitTime of an unknown commit succeeded")
		}
		if _, err := g.CommitTime("no-such-branch"); err == nil {
			t.Error("CommitTime of an unknown ref succeeded")
		}
		if _, err := fx.env.Open(t.TempDir()).(WorkTree).CommitTime("HEAD"); err == nil {
			t.Error("CommitTime outside a repository succeeded")
		}
	})
}
