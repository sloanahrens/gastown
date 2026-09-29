package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/deacon"
)

// searchStorage is a minimal Storage stub for hasActiveWork tests.
// Embeds beadsdk.Storage to satisfy the full interface without implementing
// every method (unused methods panic if called, which surfaces accidental calls).
type searchStorage struct {
	beadsdk.Storage
	// results maps status string → issues to return
	results map[string][]*beadsdk.Issue
	err     error
}

func (s *searchStorage) SearchIssues(_ context.Context, _ string, filter beadsdk.IssueFilter) ([]*beadsdk.Issue, error) {
	if s.err != nil {
		return nil, s.err
	}
	status := ""
	if filter.Status != nil {
		status = string(*filter.Status)
	}
	return s.results[status], nil
}

// writeDeaconHeartbeat writes a fresh or stale deacon heartbeat file to townRoot.
func writeDeaconHeartbeat(t *testing.T, townRoot string, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age)
	hb := &deacon.Heartbeat{Timestamp: ts}
	if err := deacon.WriteHeartbeat(townRoot, hb); err != nil {
		t.Fatalf("writeDeaconHeartbeat: %v", err)
	}
}

func newTestDaemonWithStores(t *testing.T, townRoot string, stores map[string]beadsdk.Storage) *Daemon {
	t.Helper()
	return &Daemon{
		config:      &Config{TownRoot: townRoot},
		logger:      log.New(io.Discard, "", 0),
		tmux:        newFakeTmux(newFixedClock()),
		beadsStores: stores,
		ctx:         context.Background(),
	}
}

// TestHasActiveWork covers the hasActiveWork helper in isolation.
func TestHasActiveWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stores map[string]beadsdk.Storage
		want   bool
	}{
		{
			name:   "no stores — conservative true",
			stores: nil,
			want:   true,
		},
		{
			name:   "empty stores map — conservative true",
			stores: map[string]beadsdk.Storage{},
			want:   true,
		},
		{
			name: "store error — conservative true",
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{err: fmt.Errorf("db offline")},
			},
			want: true,
		},
		{
			name: "no in_progress or hooked beads",
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			want: false,
		},
		{
			name: "in_progress bead present",
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "sc-abc"}},
				}},
			},
			want: true,
		},
		{
			name: "hooked bead only — patrol wisps do not count as active work",
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"hooked": {{ID: "hq-wisp-34zi"}},
				}},
			},
			want: false,
		},
		{
			name: "active work in second store only",
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
				"rig": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "nw-xyz"}},
				}},
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{
				config:      &Config{TownRoot: t.TempDir()},
				logger:      log.New(io.Discard, "", 0),
				beadsStores: tc.stores,
				ctx:         context.Background(),
			}
			got := d.hasActiveWork()
			if got != tc.want {
				t.Errorf("hasActiveWork() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnsureBootRunning_IdleGuard verifies that Boot is not spawned when
// Deacon is healthy and no work is active (idle guard).
func TestEnsureBootRunning_IdleGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		heartbeatAge time.Duration // zero means no heartbeat file
		stores       map[string]beadsdk.Storage
		wantSpawns   int
		desc         string
	}{
		{
			name:         "idle: fresh heartbeat, no work — skip",
			heartbeatAge: 1 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantSpawns: 0,
			desc:       "Both idle conditions met: Boot must NOT spawn",
		},
		{
			name:         "stale heartbeat, no work — spawn",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantSpawns: 1,
			desc:       "Deacon may be stuck: Boot must spawn",
		},
		{
			name:         "fresh heartbeat, active work — spawn",
			heartbeatAge: 1 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "sc-abc"}},
				}},
			},
			wantSpawns: 1,
			desc:       "Work may be stuck: Boot must spawn",
		},
		{
			name:         "no heartbeat file — spawn",
			heartbeatAge: 0, // no heartbeat
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantSpawns: 1,
			desc:       "No heartbeat: Deacon never started or died: Boot must spawn",
		},
		{
			name:         "store error — spawn conservatively",
			heartbeatAge: 1 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{err: fmt.Errorf("db offline")},
			},
			wantSpawns: 1,
			desc:       "Cannot check work state: Boot must spawn conservatively",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, d, spawner := bootTestTown(t)
			useAgentBootMode(t, townRoot)

			if tc.heartbeatAge > 0 {
				writeDeaconHeartbeat(t, townRoot, tc.heartbeatAge)
			}

			d.beadsStores = tc.stores
			d.ctx = context.Background()
			d.ensureBootRunning()

			spawns := spawner.count()
			if spawns != tc.wantSpawns {
				t.Errorf("%s\ngot %d spawn(s), want %d", tc.desc, spawns, tc.wantSpawns)
			}
		})
	}
}
