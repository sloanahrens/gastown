package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// fakeDispatchMRLister stands in for the rig's beads database. It records
// every query so a test can assert not just the count but that the guard
// asked at all (knob off and --force must not pay for a queue read).
type fakeDispatchMRLister struct {
	mrs   []*beads.Issue
	err   error
	calls []beads.ListOptions
}

func (f *fakeDispatchMRLister) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	f.calls = append(f.calls, opts)
	return f.mrs, f.err
}

// backpressureTown writes a town root holding one rig whose settings carry
// max_ready_for_dispatch. This is the layout the guard reads: the rig
// directory is the town root plus the rig name, and merge_queue lives in
// <rig>/settings/config.json beside batch_max and the gate commands.
func backpressureTown(t *testing.T, maxReady int) (townRoot, rigName string) {
	t.Helper()
	townRoot = t.TempDir()
	rigName = "gastown"
	settingsDir := filepath.Join(townRoot, rigName, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	settings := fmt.Sprintf(`{"type":"rig-settings","version":1,"merge_queue":{"max_ready_for_dispatch":%d}}`, maxReady)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), []byte(settings), 0644); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return townRoot, rigName
}

// readyMRs builds n work beads waiting to land: open, labeled
// gt:ready-to-land (the fake lister already filters on the label).
func readyMRs(n int) []*beads.Issue {
	mrs := make([]*beads.Issue, 0, n)
	for i := 0; i < n; i++ {
		mrs = append(mrs, &beads.Issue{ID: fmt.Sprintf("gt-mr%d", i), Status: "open"})
	}
	return mrs
}

// backpressureGuard is the guard reading lister for the queue and labels for
// the hooked bead.
func backpressureGuard(lister *fakeDispatchMRLister, labels ...string) slingBackpressure {
	return slingBackpressure{
		lookupBead: func(_, beadID string) (poolBead, error) { return poolBead{ID: beadID, Labels: labels}, nil },
		lister:     func(string) dispatchMRLister { return lister },
	}
}

// TestCheckSlingBackpressure drives the refusal, every way through it, and the
// two cases where the guard must not even read the queue (gt-xidg, A3).
func TestCheckSlingBackpressure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		maxReady  int
		mrs       []*beads.Issue
		listerErr error
		hookBead  string
		labels    []string
		force     bool
		wantErr   bool
		// wantCalls is how many times the guard may query the queue. Zero is a
		// requirement, not an observation: a rig with the knob off must not pay
		// a Dolt round trip on every sling.
		wantCalls int
	}{
		{
			name:      "over the ceiling refuses",
			maxReady:  12,
			mrs:       readyMRs(13),
			hookBead:  "gt-abc",
			wantErr:   true,
			wantCalls: 1,
		},
		{
			name:      "at the ceiling dispatches",
			maxReady:  12,
			mrs:       readyMRs(12),
			hookBead:  "gt-abc",
			wantCalls: 1,
		},
		{
			name:      "rework label passes without reading the queue",
			maxReady:  12,
			mrs:       readyMRs(13),
			hookBead:  "gt-abc",
			labels:    []string{"rework"},
			wantCalls: 0,
		},
		{
			name:      "force passes without reading the queue",
			maxReady:  12,
			mrs:       readyMRs(13),
			hookBead:  "gt-abc",
			force:     true,
			wantCalls: 0,
		},
		{
			name:      "knob off passes without reading the queue",
			maxReady:  0,
			mrs:       readyMRs(13),
			hookBead:  "gt-abc",
			wantCalls: 0,
		},
		{
			name:      "landed beads do not count",
			maxReady:  12,
			mrs:       append(readyMRs(12), &beads.Issue{ID: "gt-landed", Status: "closed"}),
			hookBead:  "gt-abc",
			wantCalls: 1,
		},
		{
			name:      "an unreadable queue fails open",
			maxReady:  12,
			listerErr: errors.New("connection refused"),
			hookBead:  "gt-abc",
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot, rigName := backpressureTown(t, tt.maxReady)
			lister := &fakeDispatchMRLister{mrs: tt.mrs, err: tt.listerErr}

			err := backpressureGuard(lister, tt.labels...).check(townRoot, rigName, SlingSpawnOptions{
				TownRoot: townRoot,
				HookBead: tt.hookBead,
				Force:    tt.force,
			})

			if tt.wantErr {
				if !errors.Is(err, errQueueBackpressure) {
					t.Fatalf("checkSlingBackpressure() error = %v, want errQueueBackpressure", err)
				}
				want := "sling refused: gastown has 13 beads waiting to land (> 12); pass --force or label the bead rework"
				if got := err.Error(); got != want {
					t.Errorf("refusal message = %q, want %q", got, want)
				}
			} else if err != nil {
				t.Fatalf("checkSlingBackpressure() error = %v, want nil", err)
			}

			if len(lister.calls) != tt.wantCalls {
				t.Fatalf("queue queries = %d, want %d (calls: %+v)", len(lister.calls), tt.wantCalls, lister.calls)
			}
			if tt.wantCalls == 0 {
				return
			}
			// The count comes from one bulk query on the ready-to-land label,
			// never a per-id read.
			call := lister.calls[0]
			if call.Label != land.LabelReadyToLand || call.Priority != -1 {
				t.Errorf("queue query = %+v, want the bulk ready-to-land query", call)
			}
		})
	}
}

// TestCheckSlingBackpressureUnreadableBeadStillCounts pins the choice made for
// a bead whose labels cannot be read: the guard protects the queue, so an
// unreadable bead is treated as unlabeled rather than assumed to be rework.
// The refusal names both ways through, so the operator is never stuck.
func TestCheckSlingBackpressureUnreadableBeadStillCounts(t *testing.T) {
	t.Parallel()
	townRoot, rigName := backpressureTown(t, 12)
	guard := backpressureGuard(&fakeDispatchMRLister{mrs: readyMRs(13)})
	guard.lookupBead = func(_, _ string) (poolBead, error) {
		return poolBead{}, errors.New("no such bead")
	}

	if err := guard.check(townRoot, rigName, SlingSpawnOptions{HookBead: "gt-abc"}); !errors.Is(err, errQueueBackpressure) {
		t.Fatalf("checkSlingBackpressure() error = %v, want errQueueBackpressure", err)
	}
}

// TestCheckSlingBackpressureWithoutBead covers a spawn with no hooked bead
// (gt sling --hook-raw-bead and the dog paths): there is no label to read, so
// the count alone decides.
func TestCheckSlingBackpressureWithoutBead(t *testing.T) {
	t.Parallel()
	townRoot, rigName := backpressureTown(t, 12)
	guard := backpressureGuard(&fakeDispatchMRLister{mrs: readyMRs(13)})

	if err := guard.check(townRoot, rigName, SlingSpawnOptions{}); !errors.Is(err, errQueueBackpressure) {
		t.Fatalf("checkSlingBackpressure() error = %v, want errQueueBackpressure", err)
	}
}

// TestCheckSlingBackpressureNoSettingsIsOff covers a rig that never configured
// the knob: no settings file at all must not read as "everything is full".
func TestCheckSlingBackpressureNoSettingsIsOff(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	lister := &fakeDispatchMRLister{mrs: readyMRs(13)}

	if err := backpressureGuard(lister).check(townRoot, "newrig", SlingSpawnOptions{HookBead: "gt-abc"}); err != nil {
		t.Fatalf("checkSlingBackpressure() error = %v, want nil", err)
	}
	if len(lister.calls) != 0 {
		t.Errorf("queue queries = %d, want 0", len(lister.calls))
	}
}

// TestCountReadyToLand pins the count itself: open and in-flight beads carry
// the ready-to-land label until the landing worker lands them; a closed bead
// has already landed and is not queue depth.
func TestCountReadyToLand(t *testing.T) {
	t.Parallel()
	lister := &fakeDispatchMRLister{mrs: []*beads.Issue{
		{ID: "gt-a", Status: "open"},
		{ID: "gt-b", Status: "in_progress"},
		{ID: "gt-c", Status: "closed"},
		{ID: "gt-d", Status: "hooked"},
	}}

	got, err := countReadyToLand(lister, "gastown")
	if err != nil {
		t.Fatalf("countReadyToLand() error = %v", err)
	}
	if got != 3 {
		t.Errorf("countReadyToLand() = %d, want 3", got)
	}
	if len(lister.calls) != 1 {
		t.Errorf("queue queries = %d, want 1 (one bulk read, never per-id)", len(lister.calls))
	}
	if lister.calls[0].Label != land.LabelReadyToLand {
		t.Errorf("queried label %q, want %q", lister.calls[0].Label, land.LabelReadyToLand)
	}
}

// TestQueueBackpressureErrorIsIdentifiable keeps the sentinel contract: the
// daemon feeder recognizes a refusal in a sling subprocess's stderr, and
// in-process callers match with errors.Is, so the message may change but the
// identity must not.
func TestQueueBackpressureErrorIsIdentifiable(t *testing.T) {
	t.Parallel()
	err := &queueBackpressureError{Rig: "gastown", Ready: 13, Max: 12}
	if !errors.Is(err, errQueueBackpressure) {
		t.Error("errors.Is(queueBackpressureError, errQueueBackpressure) = false, want true")
	}
	if !strings.Contains(err.Error(), "sling refused:") {
		t.Errorf("refusal %q must carry the marker the daemon feeder keys on", err.Error())
	}
}
