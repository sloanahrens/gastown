package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
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

// TestPreRollGuardAnUnreadableLedgerRaisesNoRefusedItem: a ledger the check
// cannot read refuses nothing it cannot see, raises no unreadable item of its
// own, and leaves the rest of the read answering as it did.
func TestPreRollGuardAnUnreadableLedgerRaisesNoRefusedItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.check.ledger = pairRefusedLedger(update.ImageRecord{}, errors.New("the daemon image record images.json is damaged"))

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1, "an unreadable ledger must not raise a refused item: %v", pairItemKinds(items))
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind,
		"the published pair is still ready, since the ledger could not say it was refused")
	require.Len(t, fx.logs, 1, "the unreadable ledger is logged once")
	assert.Contains(t, fx.logs[0], "could not be read")

	// A second pass with the same damaged ledger does not log again: the outage
	// is reported once, not on every read.
	fx.run(pairT0.Add(pairReadyInterval))
	assert.Len(t, fx.logs, 1, "the same outage must not be logged on every pass")

	// A later readable ledger names the refusal again, and the item comes back.
	fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
	fx.run(pairT0.Add(2 * pairReadyInterval))
	assert.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)
}

// TestPreRollGuardARefusedItemWaitsWhileTheLedgerIsUnreadable: a refused item
// the check already holds is kept by a read of a rig that did not answer, since
// the ledger could not say the refusal is gone; a readable ledger that clears
// the refusal drops it even when the branch read that same pass failed.
func TestPreRollGuardARefusedItemWaitsWhileTheLedgerIsUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		ledgerErr   error
		wantRefused bool
	}{
		{name: "the ledger stays unreadable", ledgerErr: errors.New("the daemon image record images.json is damaged"), wantRefused: true},
		{name: "the ledger answers that nothing is refused", wantRefused: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.check.ledger = pairRefusedLedger(pairRefusedRecord(), nil)
			fx.run(pairT0)
			require.Contains(t, pairItemKinds(fx.check.observed()), attention.KindDeployPairRefused)

			fx.check.ledger = pairRefusedLedger(update.ImageRecord{}, tt.ledgerErr)
			fx.rigErr = errors.New("the rig's Forgejo settings could not be read")
			fx.run(pairT0.Add(pairReadyInterval))

			got := pairItemKinds(fx.check.observed())
			if tt.wantRefused {
				assert.Contains(t, got, attention.KindDeployPairRefused,
					"a ledger that could not be read must not drop the refused item")
				return
			}
			assert.NotContains(t, got, attention.KindDeployPairRefused,
				"a readable ledger that clears the refusal drops it even when the rig read failed")
		})
	}
}
