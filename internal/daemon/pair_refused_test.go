package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/update"
)

// pairRefusedReason is the pre-swap check's recorded failure, with the check and
// the message the ledger keeps.
const pairRefusedReason = "preflight pair: no digest for the daemon image at green/" + pairGreen12

// pairRefusedLedger is the ledger seam that answers rec, or err when it is set.
func pairRefusedLedger(rec update.ImageRecord, err error) pairReadyLedgerRead {
	return func(string) (update.ImageRecord, error) { return rec, err }
}

// pairRefusedRecord is a ledger that records pairGreen's pair as refused.
func pairRefusedRecord() update.ImageRecord {
	return update.ImageRecord{
		Refused:       update.ImagePair{Commit: pairGreen},
		RefusedReason: pairRefusedReason,
	}
}

// pairItemKinds is the kinds the check holds, for an assertion that names one
// condition per item.
func pairItemKinds(items []attention.Item) []attention.Kind {
	var out []attention.Kind
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

// TestPreRollGuardRefusedNewerPairRaisesItsItem: a green commit newer than the
// running one whose pair the update ledger records as refused raises one
// deploy-pair-refused item naming the rig, the commit and the recorded reason,
// and no deploy-pair-ready item, although both images are published.
func TestPreRollGuardRefusedNewerPairRaisesItsItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1, "a refused commit must raise its own item and no pair-ready item beside it: %v", pairItemKinds(items))
	it := items[0]
	assert.Equal(t, attention.KindDeployPairRefused, it.Kind)
	assert.Equal(t, "deploy-pair-refused:gastown:"+pairGreen12, it.Key)
	assert.Equal(t, "gastown", it.Rig)
	assert.Equal(t, pairGreen, it.SHA)
	assert.Contains(t, it.Summary, "green/"+pairGreen12)
	assert.Contains(t, it.Summary, pairRefusedReason, "the item does not name the reason the ledger recorded")
}

// TestPreRollGuardADifferentCommitIsReadyAsBefore: the ledger refusing one
// commit does not stop another green commit newer than the running one from
// raising its pair-ready item, so a later green rolls as it did.
func TestPreRollGuardADifferentCommitIsReadyAsBefore(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(update.ImageRecord{
		Refused:       update.ImagePair{Commit: pairOther},
		RefusedReason: pairRefusedReason,
	}, nil)

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1)
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind, "a refusal of another commit must not touch this commit's ready item")
}

// TestPreRollGuardTheRefusedItemHasItsOwnCollector: the refused pair is a kind
// of its own from the pair check's read, so the attention queue needs a
// collector registered for it; the item the check holds is otherwise never
// read.
func TestPreRollGuardTheRefusedItemHasItsOwnCollector(t *testing.T) {
	t.Parallel()
	item := attention.Item{Key: "deploy-pair-refused:gastown:" + pairGreen12, Kind: attention.KindDeployPairRefused}
	s := &attentionSources{pairReady: func() []attention.Item { return []attention.Item{item} }}

	var collector attentionCollector
	registered := false
	for _, c := range s.collectors() {
		if c.kind == attention.KindDeployPairRefused {
			collector, registered = c, true
			break
		}
	}
	require.True(t, registered, "no collector is registered for %q, so the queue never reads the item", attention.KindDeployPairRefused)

	items, err := collector.collect(context.Background())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, attention.KindDeployPairRefused, items[0].Kind)
	assert.Equal(t, item.Key, items[0].Key)
}

// TestPreRollGuardARefusedItemClears: the item raised for a refused pair is
// gone from the next read when the ledger clears the refusal, and when the
// running commit reaches the refused commit.
func TestPreRollGuardARefusedItemClears(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// clear makes the second read's ledger, and the running commit when it
		// changes.
		clear   func(t *testing.T, fx *pairFixture)
		want    []attention.Kind
		explain string
	}{
		{
			name: "the ledger clears the refusal",
			clear: func(t *testing.T, fx *pairFixture) {
				t.Helper()
				fx.check.ledger = pairRefusedLedger(update.ImageRecord{}, nil)
			},
			want:    []attention.Kind{attention.KindDeployPairReady},
			explain: "with the refusal cleared the published pair is ready again",
		},
		{
			name: "the running commit reaches the refused commit",
			clear: func(t *testing.T, fx *pairFixture) {
				t.Helper()
				fx.writeRunning(t, pairGreen)
			},
			want:    nil,
			explain: "a commit the daemon runs is not ahead of it, so neither item is raised",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused,
				"the first read did not raise the refused item the second read must clear")

			tt.clear(t, fx)
			fx.run(pairT0.Add(pairReadyInterval))

			assert.Equal(t, tt.want, pairItemKinds(fx.check.observed()), tt.explain)
		})
	}
}

// TestPreRollGuardAnUnreadableLedgerRaisesNoPairReadyItem: a ledger the check
// cannot read is a pair answer it could not make. The published pair raises no
// pair-ready item, since it may be the one the pre-swap check refused, and the
// check raises its unmeasured item naming the ledger once per outage.
func TestPreRollGuardAnUnreadableLedgerRaisesNoPairReadyItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(update.ImageRecord{}, errors.New("the daemon image record images.json is damaged"))

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1, "an unreadable ledger must raise no pair item: %v", pairItemKinds(items))
	assert.Equal(t, attention.KindDeployPairUnreadable, items[0].Kind)
	assert.Equal(t, "deploy-pair-unreadable:town:update-ledger", items[0].Key)
	assert.Contains(t, items[0].Summary, "images.json is damaged",
		"the unmeasured item does not name the ledger the check could not read")
	require.Len(t, fx.logs, 1, "the unreadable ledger is logged once")
	assert.Contains(t, fx.logs[0], "could not be read")

	// A second pass with the same damaged ledger does not log again: the outage
	// is reported once, not on every read.
	fx.run(pairT0.Add(pairReadyInterval))
	assert.Len(t, fx.logs, 1, "the same outage must not be logged on every pass")

	// A later readable ledger names the refusal, the refused item is raised, and
	// the unmeasured item is gone.
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
	fx.run(pairT0.Add(2 * pairReadyInterval))
	assert.Equal(t, []attention.Kind{attention.KindDeployPairRefused}, pairItemKinds(fx.check.observed()))
}

// TestPreRollGuardARefusedItemSurvivesAnUnreadableLedger: an item the check
// raised for a refused pair stays when the next pass cannot read the ledger,
// even though the rig read itself answered: the ledger could not say the refusal
// is gone, and the pair-ready item is not raised in its place.
func TestPreRollGuardARefusedItemSurvivesAnUnreadableLedger(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
	fx.run(pairT0)
	require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

	fx.check.ledger = pairRefusedLedger(update.ImageRecord{}, errors.New("the daemon image record images.json is damaged"))
	fx.run(pairT0.Add(pairReadyInterval))

	got := pairItemKinds(fx.check.observed())
	assert.Contains(t, got, attention.KindDeployPairRefused,
		"a ledger that could not be read must not drop the refused item")
	assert.NotContains(t, got, attention.KindDeployPairReady,
		"a ledger that could not be read must not raise a pair-ready item")
	assert.Contains(t, got, attention.KindDeployPairUnreadable,
		"a ledger that could not be read must say the check is unmeasured")
}

// TestPreRollGuardARefusedItemOutlivesAFailedRigRead: a refused item the check
// holds is kept by a rig read that did not answer when the ledger still names
// the commit, and dropped when the ledger answers that nothing is refused or the
// running commit reaches the refused one.
func TestPreRollGuardARefusedItemOutlivesAFailedRigRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rec         update.ImageRecord
		ledgerErr   error
		reach       bool
		wantRefused bool
	}{
		{name: "the ledger stays unreadable", ledgerErr: errors.New("the daemon image record images.json is damaged"), wantRefused: true},
		{name: "the ledger answers that nothing is refused", wantRefused: false},
		{name: "the ledger still refuses the commit", rec: pairRefusedRecord(), wantRefused: true},
		{name: "the running commit reaches the refused commit", rec: pairRefusedRecord(), reach: true, wantRefused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

			if tt.reach {
				fx.writeRunning(t, pairGreen)
			}
			fx.check.ledger = pairRefusedLedger(tt.rec, tt.ledgerErr)
			fx.rigErr = errors.New("the rig's Forgejo settings could not be read")
			fx.run(pairT0.Add(pairReadyInterval))

			got := pairItemKinds(fx.check.observed())
			if tt.wantRefused {
				assert.Contains(t, got, attention.KindDeployPairRefused,
					"a ledger that still names the commit must not lose its refused item to a failed rig read")
				return
			}
			assert.NotContains(t, got, attention.KindDeployPairRefused,
				"a refusal the read can no longer confirm must be dropped")
		})
	}
}

// TestPreRollGuardAnIncompleteScanKeepsARefusedItem: a listing that ended early
// cannot say the refused commit's pair is gone, and the ledger still names it,
// so the item stays.
func TestPreRollGuardAnIncompleteScanKeepsARefusedItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
	fx.run(pairT0)
	require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

	fx.api.branchErr = errors.New("forgejo answered 500 on that page")
	fx.run(pairT0.Add(pairReadyInterval))

	got := pairItemKinds(fx.check.observed())
	assert.Contains(t, got, attention.KindDeployPairRefused,
		"an incomplete scan must not drop a refusal the ledger still records")
	assert.NotContains(t, got, attention.KindDeployPairReady)
}

// TestPreRollGuardAFailedJudgeKeepsARefusedItemItDidNotReach: the per-green
// judge fails and the scan breaks before it judges the refused commit, whether
// another green was listed before it or it is the green that fails. The ledger
// still names the refusal, so the item stays and no pair-ready item is raised.
func TestPreRollGuardAFailedJudgeKeepsARefusedItemItDidNotReach(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// stops makes the next pass's judge fail before it judges the refused
		// commit.
		stops func(fx *pairFixture)
	}{
		{
			name: "another green listed before it fails its comparison",
			stops: func(fx *pairFixture) {
				fx.api.branches = []forgejo.Branch{pairBranch(pairOther), pairBranch(pairGreen)}
			},
		},
		{
			name: "the refused commit itself fails its comparison",
			stops: func(fx *pairFixture) {
				delete(fx.api.compares, pairRunning+"..."+pairGreen)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused,
				"the first read did not raise the refused item")

			tt.stops(fx)
			fx.run(pairT0.Add(pairReadyInterval))
			require.Len(t, fx.logs, 1, "the second pass did not fail its comparison, so the test does not reach the break path")
			require.Contains(t, fx.logs[0], "no comparison fixture",
				"the second pass failed for another reason than the judge, so the test does not reach the break path")

			got := pairItemKinds(fx.check.observed())
			assert.Contains(t, got, attention.KindDeployPairRefused,
				"a judge failure that never reached the refused commit must not drop its item")
			assert.NotContains(t, got, attention.KindDeployPairReady)
		})
	}
}

// TestPreRollGuardARefusedItemJudgedBeforeTheBreakStaysOne: the scan judges the
// refused commit and raises its item, then a later green's comparison fails. The
// item the pass raised is not added a second time from what the rig held.
func TestPreRollGuardARefusedItemJudgedBeforeTheBreakStaysOne(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
	fx.run(pairT0)
	require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

	fx.api.branches = []forgejo.Branch{pairBranch(pairGreen), pairBranch(pairOther)}
	fx.run(pairT0.Add(pairReadyInterval))
	require.Len(t, fx.logs, 1, "the second pass did not fail its comparison, so the test does not reach the break path")

	refused := 0
	for _, it := range fx.check.observed() {
		if it.Kind == attention.KindDeployPairRefused {
			refused++
		}
	}
	assert.Equal(t, 1, refused, "the item the pass raised and the item the rig held are the same refusal, so they are one item")
}

// TestPreRollGuardARefusedCommitBehindTheRunningOneGoesEvenIfTheScanBreaks: a
// green commit the ledger still refuses but that the running commit has passed
// is not newer, so its item is dropped, whether the scan completes or a later
// green's comparison fails.
func TestPreRollGuardARefusedCommitBehindTheRunningOneGoesEvenIfTheScanBreaks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		breaks bool
	}{
		{name: "the scan completes"},
		{name: "a later green fails its comparison", breaks: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

			// The running commit is ahead of the refused one, so it is not newer.
			fx.api.compares[pairGreen+"..."+pairRunning] = 2
			fx.api.compares[pairRunning+"..."+pairGreen] = 0
			if tt.breaks {
				fx.api.branches = []forgejo.Branch{pairBranch(pairGreen), pairBranch(pairOther)}
			}
			fx.run(pairT0.Add(pairReadyInterval))

			got := pairItemKinds(fx.check.observed())
			assert.NotContains(t, got, attention.KindDeployPairRefused,
				"a refused commit the running commit has passed is not newer, so its item is gone")
			assert.NotContains(t, got, attention.KindDeployPairReady)
		})
	}
}

// TestPreRollGuardTheLedgerSettlesARefusedItemWhenTheScanBreaks: the scan breaks
// before it judges the refused commit, so only the ledger's answer on it can
// settle the item: a readable ledger that does not name it clears it, and a
// ledger that cannot be read leaves it.
func TestPreRollGuardTheLedgerSettlesARefusedItemWhenTheScanBreaks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		rec         update.ImageRecord
		ledgerErr   error
		wantRefused bool
	}{
		{name: "the ledger answers that nothing is refused", wantRefused: false},
		{
			name:        "the ledger refuses another commit",
			rec:         update.ImageRecord{Refused: update.ImagePair{Commit: pairOther}, RefusedReason: pairRefusedReason},
			wantRefused: false,
		},
		{
			name:        "the ledger cannot be read",
			ledgerErr:   errors.New("the daemon image record images.json is damaged"),
			wantRefused: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused,
				"the first read did not raise the refused item")

			// A green listed before the refused commit fails its comparison, so
			// the scan breaks before judging the refused commit.
			fx.api.branches = []forgejo.Branch{pairBranch(pairOther), pairBranch(pairGreen)}
			fx.check.ledger = pairRefusedLedger(tt.rec, tt.ledgerErr)
			fx.run(pairT0.Add(pairReadyInterval))

			got := pairItemKinds(fx.check.observed())
			assert.NotContains(t, got, attention.KindDeployPairReady)
			if tt.wantRefused {
				assert.Contains(t, got, attention.KindDeployPairRefused,
					"a ledger that could not say the refusal is gone must not lose its item to a scan that broke")
				return
			}
			assert.NotContains(t, got, attention.KindDeployPairRefused,
				"a readable ledger that does not name the commit settles the refusal")
		})
	}
}
