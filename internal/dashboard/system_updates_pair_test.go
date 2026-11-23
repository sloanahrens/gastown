package dashboard

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
)

// pairTestItem is one attention item of kind for commit sha, as the daemon
// raises these kinds with the commit's full sha.
func pairTestItem(kind attention.Kind, sha string) attention.Item {
	return attention.Item{Kind: kind, SHA: sha}
}

// pairTestRow is the section with one commit row, classified from items and the
// row's images runs by the hub's own polls.
func pairTestRow(t *testing.T, items []attention.Item, itemsErr error, runs []PipelineRun) string {
	t.Helper()

	sha := sysTestSHA(1)
	h := NewHub(Config{
		Attention: func() ([]attention.Event, attention.Acks) { return nil, attention.Acks{} },
		AttentionItems: func() ([]attention.Item, error) {
			return items, itemsErr
		},
		SystemUpdates: func() *SystemUpdates {
			sweeps := []TierSweepRow{{At: sysTestClock, Rig: "gastown", SHA: sha[:8]}}
			return NewSystemUpdates(sweeps, &GastownPipeline{Runs: runs}, nil, false)
		},
	})
	h.pollAttention()
	h.pollSystemUpdates()

	got := h.State().SystemUpdates
	require.NotNil(t, got, "the section is in the feed once its reader reports")
	require.Len(t, got.Rows, 1, "the section did not carry the commit row")
	return got.Rows[0].Pair
}

// A row's Pair names its commit's images pair: each attention kind maps to its
// state, the images run's running, queued and failed statuses map theirs, and a
// state both readings name is taken in the precedence refused, failed, stuck,
// ready, building, absent. An attention read that failed is unknown, never
// absent, and beats any item a failed read still returned.
func TestSystemUpdatesPair(t *testing.T) {
	t.Parallel()

	sha := sysTestSHA(1)
	images := func(status string) []PipelineRun {
		return []PipelineRun{{Workflow: imagesWorkflow, SHA: sha[:12], Status: status}}
	}
	item := func(kind attention.Kind) []attention.Item { return []attention.Item{pairTestItem(kind, sha)} }
	joined := func(kinds ...attention.Kind) []attention.Item {
		var out []attention.Item
		for _, kind := range kinds {
			out = append(out, pairTestItem(kind, sha))
		}
		return out
	}

	cases := []struct {
		name     string
		items    []attention.Item
		itemsErr error
		runs     []PipelineRun
		want     string
	}{
		{name: "no attention item and no images run", want: pairAbsent},
		{name: "pair ready", items: item(attention.KindDeployPairReady), want: pairReady},
		{name: "pair refused", items: item(attention.KindDeployPairRefused), want: pairRefused},
		{name: "pair stuck", items: item(attention.KindDeployPairStuck), want: pairStuck},
		{name: "images failed item", items: item(attention.KindImagesFailed), want: pairFailed},
		{name: "images run running", runs: images(pipelineRunning), want: pairBuilding},
		{name: "images run queued", runs: images(pipelineQueued), want: pairBuilding},
		{name: "images run failed", runs: images(pipelineFailure), want: pairFailed},
		{name: "images run succeeded", runs: images(pipelineSuccess), want: pairAbsent},
		{
			name: "the post-land run is not the images run",
			runs: []PipelineRun{{Workflow: postLandWorkflow, SHA: sha[:12], Status: pipelineRunning}},
			want: pairAbsent,
		},
		{
			name: "the newest images run names the state",
			runs: []PipelineRun{
				{Workflow: imagesWorkflow, SHA: sha[:12], Status: pipelineRunning},
				{Workflow: imagesWorkflow, SHA: sha[:12], Status: pipelineFailure},
			},
			want: pairBuilding,
		},
		{
			name: "a newer failed images run names the state",
			runs: []PipelineRun{
				{Workflow: imagesWorkflow, SHA: sha[:12], Status: pipelineFailure},
				{Workflow: imagesWorkflow, SHA: sha[:12], Status: pipelineRunning},
			},
			want: pairFailed,
		},
		{
			name: "refused before failed, stuck, ready and building",
			items: joined(attention.KindDeployPairReady, attention.KindDeployPairStuck,
				attention.KindImagesFailed, attention.KindDeployPairRefused),
			runs: images(pipelineRunning),
			want: pairRefused,
		},
		{
			name:  "failed before stuck, ready and building",
			items: joined(attention.KindDeployPairReady, attention.KindDeployPairStuck, attention.KindImagesFailed),
			runs:  images(pipelineRunning),
			want:  pairFailed,
		},
		{
			name:  "stuck before ready and building",
			items: joined(attention.KindDeployPairReady, attention.KindDeployPairStuck),
			runs:  images(pipelineRunning),
			want:  pairStuck,
		},
		{
			name:  "ready before building",
			items: joined(attention.KindDeployPairReady),
			runs:  images(pipelineRunning),
			want:  pairReady,
		},
		{
			name:     "an attention read that failed",
			itemsErr: errors.New("the queue's state could not be read"),
			want:     pairUnknown,
		},
		{
			name:     "an attention read that failed beats the items it returned",
			items:    item(attention.KindDeployPairReady),
			itemsErr: errors.New("the queue's state could not be read"),
			want:     pairUnknown,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pairTestRow(t, tt.items, tt.itemsErr, tt.runs))
		})
	}
}

// An attention item names the commit its sha names: an item for another commit,
// or for none at all, leaves this row absent while the item still classifies its
// own row.
func TestSystemUpdatesPairOtherSha(t *testing.T) {
	t.Parallel()

	all := sysTestSHA(1)
	other := sysTestSHA(2)
	items := []attention.Item{
		pairTestItem(attention.KindDeployPairReady, other),
		pairTestItem(attention.KindDeployPairRefused, other),
		pairTestItem(attention.KindDeployPairStuck, other),
		pairTestItem(attention.KindImagesFailed, other),
		{Kind: attention.KindDeployPairReady}, // no sha names no commit
	}
	h := NewHub(Config{
		Attention: func() ([]attention.Event, attention.Acks) { return nil, attention.Acks{} },
		AttentionItems: func() ([]attention.Item, error) {
			return items, nil
		},
		SystemUpdates: func() *SystemUpdates {
			sweeps := []TierSweepRow{
				{At: sysTestClock, Rig: "gastown", SHA: all[:8]},
				{At: sysTestClock.Add(-time.Minute), Rig: "gastown", SHA: other[:8]},
			}
			return NewSystemUpdates(sweeps, nil, nil, false)
		},
	})
	h.pollAttention()
	h.pollSystemUpdates()

	got := h.State().SystemUpdates
	require.NotNil(t, got, "the section is in the feed once its reader reports")
	require.Len(t, got.Rows, 2, "the section did not carry both commit rows")
	bySHA := map[string]string{}
	for _, row := range got.Rows {
		bySHA[row.SHA] = row.Pair
	}
	assert.Equal(t, pairAbsent, bySHA[all[:8]], "another commit's item set this row's pair")
	assert.Equal(t, pairRefused, bySHA[other[:8]], "the refused item did not reach its own row")
}

// Until the attention queue has been read, a row's Pair is unknown, never
// absent: a section poll that runs before the first attention poll, a hub with
// no attention reader, and a hub whose items reader is missing all leave the
// queue unknown. A read that fails after a good one forgets the items it
// returned, so they never classify a row.
func TestSystemUpdatesPairUnread(t *testing.T) {
	t.Parallel()

	sha := sysTestSHA(1)
	section := func() *SystemUpdates {
		sweeps := []TierSweepRow{{At: sysTestClock, Rig: "gastown", SHA: sha[:8]}}
		return NewSystemUpdates(sweeps, nil, nil, false)
	}
	pairOf := func(t *testing.T, h *Hub) string {
		t.Helper()
		got := h.State().SystemUpdates
		require.NotNil(t, got, "the section is in the feed once its reader reports")
		require.Len(t, got.Rows, 1, "the section did not carry the commit row")
		return got.Rows[0].Pair
	}
	readyItem := func() ([]attention.Item, error) {
		return []attention.Item{pairTestItem(attention.KindDeployPairReady, sha)}, nil
	}

	t.Run("the attention poll has not run", func(t *testing.T) {
		t.Parallel()
		h := NewHub(Config{
			Attention:      func() ([]attention.Event, attention.Acks) { return nil, attention.Acks{} },
			AttentionItems: readyItem,
			SystemUpdates:  section,
		})
		h.pollSystemUpdates()
		assert.Equal(t, pairUnknown, pairOf(t, h), "a row read as a commit with no pair before the queue was read")
	})

	t.Run("no attention reader", func(t *testing.T) {
		t.Parallel()
		h := NewHub(Config{SystemUpdates: section})
		h.pollSystemUpdates()
		assert.Equal(t, pairUnknown, pairOf(t, h), "a hub with no attention reader read a row as a commit with no pair")
	})

	t.Run("no items reader", func(t *testing.T) {
		t.Parallel()
		h := NewHub(Config{
			Attention:     func() ([]attention.Event, attention.Acks) { return nil, attention.Acks{} },
			SystemUpdates: section,
		})
		h.pollAttention()
		h.pollSystemUpdates()
		assert.Equal(t, pairUnknown, pairOf(t, h), "a queue with no items reader read as a commit with no pair")
	})

	t.Run("a failed read after a good one", func(t *testing.T) {
		t.Parallel()
		fail := false
		h := NewHub(Config{
			Attention: func() ([]attention.Event, attention.Acks) { return nil, attention.Acks{} },
			AttentionItems: func() ([]attention.Item, error) {
				if fail {
					return nil, errors.New("the queue's state could not be read")
				}
				return readyItem()
			},
			SystemUpdates: section,
		})
		h.pollAttention()
		h.pollSystemUpdates()
		assert.Equal(t, pairReady, pairOf(t, h), "a good read did not classify the row")
		fail = true
		h.pollAttention()
		h.pollSystemUpdates()
		assert.Equal(t, pairUnknown, pairOf(t, h), "the items of an earlier good read survived a failed read")
	})
}
