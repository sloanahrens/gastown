package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
)

// collectClusterDriftOnce runs the collector for one heartbeat and fails on a
// collector error, which this collector never returns: a failed answer is an
// item.
func collectClusterDriftOnce(t *testing.T, s *attentionSources) []attention.Item {
	t.Helper()
	items, err := s.collectClusterDrift(context.Background())
	if err != nil {
		t.Fatalf("collectClusterDrift: %v", err)
	}
	return items
}

// assertClusterDriftItem fails unless items is exactly one item with key and
// kind.
func assertClusterDriftItem(t *testing.T, items []attention.Item, key string, kind attention.Kind) {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly one %s item", items, key)
	}
	if items[0].Key != key || items[0].Kind != kind {
		t.Errorf("item = key %q kind %q, want key %q kind %q", items[0].Key, items[0].Kind, key, kind)
	}
}

// TestClusterDriftPatrolRaisesDriftThenClears is the bead's first box: an
// answer with a difference raises cluster.drift, and the clean answer that
// follows returns nothing.
func TestClusterDriftPatrolRaisesDriftThenClears(t *testing.T) {
	t.Parallel()
	answers := []clusterDriftResult{
		{Differing: []string{"Deployment/gastown-daemon: spec.replicas 2 -> 3", "Service/forgejo: spec.ports"}},
		{},
	}
	calls := 0
	s := &attentionSources{
		clusterDriftEnabled: func() bool { return true },
		clusterDriftAnswer: func(context.Context) (clusterDriftResult, error) {
			res := answers[calls]
			calls++
			return res, nil
		},
	}
	assertClusterDriftItem(t, collectClusterDriftOnce(t, s), attention.RuleClusterDrift, attention.KindClusterDrift)
	if items := collectClusterDriftOnce(t, s); len(items) != 0 {
		t.Errorf("the clean answer raised %+v, want nothing so the drift item clears", items)
	}
}

// TestClusterDriftPatrolUnreadable is the bead's second box: a failed answer
// raises cluster.drift_unreadable on the same heartbeat, the next clean answer
// returns nothing, and an answer nobody wired raises the unreadable item.
func TestClusterDriftPatrolUnreadable(t *testing.T) {
	t.Parallel()

	t.Run("a failed answer then a clean one", func(t *testing.T) {
		t.Parallel()
		failed := true
		s := &attentionSources{
			clusterDriftEnabled: func() bool { return true },
			clusterDriftAnswer: func(context.Context) (clusterDriftResult, error) {
				if failed {
					failed = false
					return clusterDriftResult{}, errors.New("the cluster read was refused")
				}
				return clusterDriftResult{}, nil
			},
		}
		assertClusterDriftItem(t, collectClusterDriftOnce(t, s),
			attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)
		if items := collectClusterDriftOnce(t, s); len(items) != 0 {
			t.Errorf("a clean answer after a failed one raised %+v, want nothing", items)
		}
	})

	t.Run("no answer wired", func(t *testing.T) {
		t.Parallel()
		s := &attentionSources{clusterDriftEnabled: func() bool { return true }}
		assertClusterDriftItem(t, collectClusterDriftOnce(t, s),
			attention.RuleClusterDriftUnreadable, attention.KindClusterDriftUnreadable)
	})
}

// TestClusterDriftPatrolOff is the bead's third box: with the setting off the
// collector makes no answer call and raises nothing, and a sources value that
// carries no setting raises nothing either, which is the patrol's default.
func TestClusterDriftPatrolOff(t *testing.T) {
	t.Parallel()

	t.Run("the setting is off", func(t *testing.T) {
		t.Parallel()
		calls := 0
		s := &attentionSources{
			clusterDriftEnabled: func() bool { return false },
			clusterDriftAnswer: func(context.Context) (clusterDriftResult, error) {
				calls++
				return clusterDriftResult{Differing: []string{"Deployment/gastown-daemon: spec.replicas 2 -> 3"}}, nil
			},
		}
		items := collectClusterDriftOnce(t, s)
		if calls != 0 {
			t.Errorf("the collector called the answer %d times with the setting off", calls)
		}
		if len(items) != 0 {
			t.Errorf("items = %+v with the setting off, want nothing", items)
		}
	})

	t.Run("no setting carried", func(t *testing.T) {
		t.Parallel()
		s := &attentionSources{
			clusterDriftAnswer: func(context.Context) (clusterDriftResult, error) {
				t.Error("the collector called the answer with no setting carried")
				return clusterDriftResult{}, nil
			},
		}
		if items := collectClusterDriftOnce(t, s); len(items) != 0 {
			t.Errorf("items = %+v with no setting carried, want nothing", items)
		}
	})
}

// TestClusterDriftPatrolIsWiredAndRegistered: the production sources carry the
// setting, it answers PatrolsConfig.ClusterDriftEnabled for the patrol config
// in force (off by default, on when the block asks), and the collector is one
// of the queue's.
func TestClusterDriftPatrolIsWiredAndRegistered(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t)
	s := d.attentionSources(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC))
	if s.clusterDriftEnabled == nil {
		t.Fatal("the production sources carry no cluster_drift setting")
	}
	if s.clusterDriftEnabled() {
		t.Fatal("the patrol is on with no cluster_drift block in the settings")
	}
	if c := collectorFor(t, s, attention.KindClusterDrift); c.collect == nil {
		t.Error("the drift collector is registered with no collect")
	}
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		ClusterDrift: &config.ClusterDriftPatrolConfig{Enabled: true},
	}}
	if !s.clusterDriftEnabled() {
		t.Error("patrols.cluster_drift.enabled = true, but the production setting answers false")
	}
}
