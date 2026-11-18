package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pairDatedClone dates every commit at at, so a case about another part of the
// pass reads green as young and logs no unaged ref.
type pairDatedClone struct {
	pairReadyGit
	at time.Time
}

func (c pairDatedClone) CommitTime(string) (time.Time, error) { return c.at, nil }

// pairUndatedClone judges ancestry and offers no commit time.
type pairUndatedClone struct{}

func (pairUndatedClone) HasObject(string) (bool, error)          { return true, nil }
func (pairUndatedClone) IsAncestor(string, string) (bool, error) { return true, nil }

// pairZeroTimeClone is a clone whose commits carry no time.
type pairZeroTimeClone struct{ pairReadyGit }

func (pairZeroTimeClone) CommitTime(string) (time.Time, error) { return time.Time{}, nil }

// stuckFixture is a rig whose green commit is newer than the running one with no
// image published: green, the running commit, and green's commit time.
func stuckFixture(t *testing.T) (fx *pairFixture, green, running string, greenAt time.Time) {
	t.Helper()
	clone, c := pairCloneHistory(t)
	fx = newPairCloneFixture(t, clone, c[0], c[2])
	fx.reg = &fakePairRegistry{}
	greenAt, err := clone.(pairReadyAged).CommitTime(c[2])
	require.NoError(t, err)
	return fx, c[2], c[0], greenAt
}

// stuckAt91 is stuckFixture read once at 91 minutes, so the item stands.
func stuckAt91(t *testing.T) (fx *pairFixture, green string, greenAt time.Time) {
	t.Helper()
	fx, green, _, greenAt = stuckFixture(t)
	fx.run(greenAt.Add(91 * time.Minute))
	require.Len(t, fx.check.observed(), 1)
	return fx, green, greenAt
}

// TestDeployPairStuckAtTheBound: a green commit newer than the running one whose
// pair is unpublished raises the item once it is pairReadyStuckAfter old, naming
// the rig, the commit and its age, and not a minute before. It is raised beside
// a failed images run, so no images run is needed for it.
func TestDeployPairStuckAtTheBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		age       time.Duration
		failedRun bool
		want      []attention.Kind
	}{
		{"89 minutes", 89 * time.Minute, false, nil},
		{"90 minutes", 90 * time.Minute, false, []attention.Kind{attention.KindDeployPairStuck}},
		{"91 minutes", 91 * time.Minute, false, []attention.Kind{attention.KindDeployPairStuck}},
		{"90 minutes with a failed images run", 90 * time.Minute, true,
			[]attention.Kind{attention.KindDeployPairStuck, attention.KindImagesFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx, green, _, greenAt := stuckFixture(t)
			if tc.failedRun {
				fx.setImagesRun(green, deployRunFailed)
			}
			fx.run(greenAt.Add(tc.age))

			items := fx.check.observed()
			var kinds []attention.Kind
			for _, it := range items {
				kinds = append(kinds, it.Kind)
			}
			require.Equal(t, tc.want, kinds)
			if len(tc.want) == 0 {
				return
			}
			it := items[0]
			assert.Equal(t, "deploy-pair-stuck:gastown:"+green[:12], it.Key)
			assert.Equal(t, "gastown", it.Rig)
			assert.Equal(t, green, it.SHA)
			assert.Contains(t, it.Summary, strconv.Itoa(int(tc.age/time.Minute))+" minutes old")
			assert.Contains(t, it.Summary, "pair is not published")
		})
	}
}

// TestDeployPairStuckNextRead: the item clears when the pair publishes, on a scan
// that breaks on another branch after judging green, and when the running commit
// reaches green; a read that did not answer keeps it beside the unreadable item.
func TestDeployPairStuckNextRead(t *testing.T) {
	t.Parallel()
	t.Run("when the pair is published", func(t *testing.T) {
		t.Parallel()
		fx, green, greenAt := stuckAt91(t)
		fx.reg = &fakePairRegistry{digests: map[string]string{
			imagevalues.DaemonImage + "@" + green[:12]:  fx.daemon,
			imagevalues.PolecatImage + "@" + green[:12]: fx.polecat,
		}}
		// A second green the clone does not hold and Forgejo has no comparison
		// for stops the scan after green was judged.
		fx.api.branches = append(fx.api.branches, pairBranch(pairOther))
		fx.run(greenAt.Add(91*time.Minute + pairReadyInterval))

		items := fx.check.observed()
		require.Len(t, items, 2)
		assert.Equal(t, attention.KindDeployPairReady, items[0].Kind)
		assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
	})
	t.Run("when the running commit reaches green", func(t *testing.T) {
		t.Parallel()
		fx, green, greenAt := stuckAt91(t)
		fx.writeRunning(t, green)
		fx.run(greenAt.Add(91*time.Minute + pairReadyInterval))

		assert.Empty(t, fx.check.observed(), "a green commit the running one reached left its stuck item standing")
	})
	t.Run("when the read fails", func(t *testing.T) {
		t.Parallel()
		fx, _, greenAt := stuckAt91(t)
		fx.api.branchErr = errors.New("forgejo answered 500")
		fx.run(greenAt.Add(91*time.Minute + pairReadyInterval))

		items := fx.check.observed()
		require.Len(t, items, 2)
		assert.Equal(t, attention.KindDeployPairStuck, items[0].Kind)
		assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
	})
}

// TestDeployPairStuckUnagedRefRaisesNothing: a green commit the check cannot
// date raises no stuck item, so it is read neither as stuck nor as young, and
// the pass names the ref it could not age.
func TestDeployPairStuckUnagedRefRaisesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		clone func(pairReadyGit) pairReadyGit
		want  string
	}{
		{"no clone", func(pairReadyGit) pairReadyGit { return nil }, "no local clone that dates a commit"},
		{"a clone that cannot date", func(pairReadyGit) pairReadyGit { return pairUndatedClone{} }, "no local clone that dates a commit"},
		{"a commit carrying no time", func(clone pairReadyGit) pairReadyGit { return pairZeroTimeClone{clone} }, "since its commit carries no time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx, green, running, greenAt := stuckFixture(t)
			fx.git = tc.clone(fx.git)
			fx.api.compares = map[string]int{green + "..." + running: 0, running + "..." + green: 3}

			fx.run(greenAt.Add(91 * time.Minute))

			assert.Empty(t, fx.check.observed(), "an unaged green commit raised an item")
			require.Len(t, fx.logs, 1)
			assert.Contains(t, fx.logs[0], "green/"+green[:12])
			assert.Contains(t, fx.logs[0], tc.want)
		})
	}
}

// TestDeployPairStuckReachesTheQueue: the stuck item is a kind of its own from
// the pair check's read, so the queue reads it through a collector of its own.
func TestDeployPairStuckReachesTheQueue(t *testing.T) {
	t.Parallel()
	stuck := attention.Item{Key: "deploy-pair-stuck:gastown:" + pairGreen12, Kind: attention.KindDeployPairStuck}
	s := &attentionSources{pairReady: func() []attention.Item { return []attention.Item{stuck} }}

	var own []attentionCollector
	for _, c := range s.collectors() {
		if c.kind == attention.KindDeployPairStuck {
			own = append(own, c)
		}
	}
	require.Len(t, own, 1, "no collector is registered for %q, so its items never reach the queue", attention.KindDeployPairStuck)

	observed := collectAttention(context.Background(), attention.State{}, own, func(string, ...any) {})
	assert.Equal(t, []attention.Item{stuck}, observed)
}
