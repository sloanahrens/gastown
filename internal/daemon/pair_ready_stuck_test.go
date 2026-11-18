package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// pairDatedClone dates every commit at at, so a case about another part of the
// pass raises no stuck item and logs no unaged ref.
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

// publish makes green's pair available in the fixture's registry.
func (fx *pairFixture) publish(green string) {
	fx.reg = &fakePairRegistry{digests: map[string]string{
		imagevalues.DaemonImage + "@" + green[:12]:  fx.daemon,
		imagevalues.PolecatImage + "@" + green[:12]: fx.polecat,
	}}
}

// TestDeployPairStuckAtTheBound: a green commit newer than the running one whose
// pair is unpublished raises the item once it is pairReadyStuckAfter old, naming
// the rig, the commit and its age, and not a minute before.
func TestDeployPairStuckAtTheBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"89 minutes", 89 * time.Minute, false},
		{"90 minutes", 90 * time.Minute, true},
		{"91 minutes", 91 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx, green, _, greenAt := stuckFixture(t)
			fx.run(greenAt.Add(tc.age))

			items := fx.check.observed()
			if !tc.want {
				assert.Empty(t, items, "a green commit younger than the stuck bound raised an item")
				return
			}
			require.Len(t, items, 1)
			it := items[0]
			assert.Equal(t, attention.KindDeployPairStuck, it.Kind)
			assert.Equal(t, "deploy-pair-stuck:gastown:"+green[:12], it.Key)
			assert.Equal(t, "gastown", it.Rig)
			assert.Equal(t, green, it.SHA)
			assert.Contains(t, it.Summary, fmt.Sprint(int(tc.age/time.Minute))+" minutes old")
			assert.Contains(t, it.Summary, "pair is not published")
		})
	}
}

// TestDeployPairStuckRaisesBesideAFailedImagesRun: the item names a pair no roll
// can start from whether or not an images run exists.
func TestDeployPairStuckRaisesBesideAFailedImagesRun(t *testing.T) {
	t.Parallel()
	fx, green, _, greenAt := stuckFixture(t)
	fx.setImagesRun(green, deployRunFailed)

	fx.run(greenAt.Add(pairReadyStuckAfter))

	items := fx.check.observed()
	require.Len(t, items, 2)
	assert.Equal(t, attention.KindDeployPairStuck, items[0].Kind)
	assert.Equal(t, attention.KindImagesFailed, items[1].Kind)
}

// TestDeployPairStuckClears: the item clears when the pair publishes, even on a
// scan that breaks on another branch after judging green, and when the running
// commit reaches green.
func TestDeployPairStuckClears(t *testing.T) {
	t.Parallel()
	t.Run("when the pair is published", func(t *testing.T) {
		t.Parallel()
		fx, green, _, greenAt := stuckFixture(t)
		fx.run(greenAt.Add(91 * time.Minute))
		require.Len(t, fx.check.observed(), 1)

		fx.publish(green)
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
		fx, green, _, greenAt := stuckFixture(t)
		fx.run(greenAt.Add(91 * time.Minute))
		require.Len(t, fx.check.observed(), 1)

		fx.writeRunning(t, green)
		fx.run(greenAt.Add(91*time.Minute + pairReadyInterval))

		assert.Empty(t, fx.check.observed(), "a green commit the running one reached left its stuck item standing")
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

// TestDeployPairStuckSurvivesAFailedRead: a read that did not answer cannot say
// the pair published, so the item stands beside the unreadable item.
func TestDeployPairStuckSurvivesAFailedRead(t *testing.T) {
	t.Parallel()
	fx, _, _, greenAt := stuckFixture(t)
	fx.run(greenAt.Add(91 * time.Minute))
	require.Len(t, fx.check.observed(), 1)

	fx.api.branchErr = errors.New("forgejo answered 500")
	fx.run(greenAt.Add(91*time.Minute + pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 2)
	assert.Equal(t, attention.KindDeployPairStuck, items[0].Kind)
	assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
}

// TestDeployPairStuckHasItsOwnCollector: the item is a kind of its own from the
// pair check's read, so it needs a collector of its own to reach the queue.
func TestDeployPairStuckHasItsOwnCollector(t *testing.T) {
	t.Parallel()
	stuck := attention.Item{Key: "deploy-pair-stuck:gastown:" + pairGreen12, Kind: attention.KindDeployPairStuck}
	s := &attentionSources{pairReady: func() []attention.Item { return []attention.Item{stuck} }}

	for _, c := range s.collectors() {
		if c.kind != attention.KindDeployPairStuck {
			continue
		}
		items, err := c.collect(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []attention.Item{stuck}, items)
		assert.Equal(t, []attention.Item{stuck}, c.kept(attention.State{Items: []attention.Item{stuck}}))
		return
	}
	t.Fatal("no collector raises the deploy-pair-stuck kind, so its items never reach the queue")
}
