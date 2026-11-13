package polecat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/land/landtest"
)

// TestLandedSeatDoesNotCountTowardCapacityAtTheManager drives the Manager's own
// classifier input (workstateInputForPolecat), the production caller that reads
// the seat's intent record and landing log. A submitted seat whose landing is
// recorded frees its capacity; a submission with no record still holds it.
func TestLandedSeatDoesNotCountTowardCapacityAtTheManager(t *testing.T) {
	t.Parallel()
	submittedAt := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	landedAfter := submittedAt.Add(time.Hour)
	cases := []struct {
		name     string
		landedAt *time.Time // the landing record's time; nil means none
		want     bool       // CountsTowardCapacity
	}{
		{name: "a landing recorded after the submission frees the seat", landedAt: &landedAfter, want: false},
		{name: "a submission with no landing record holds the seat", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
			if mgr.townRoot == "" {
				t.Fatal("manager has no town root; the landing read would not reach the town's records")
			}
			bd.Seed(beads.Issue{ID: "gt-jhz3t0", Status: "hooked", Labels: []string{land.LabelReadyToLand}})
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.HookBead = "gt-jhz3t0" })
			seat := intent.Seat{Rig: "rig", Role: constants.RolePolecat, Name: "toast"}
			if err := intent.MarkSubmitted(mgr.townRoot, seat, "gt-jhz3t0", "test", submittedAt); err != nil {
				t.Fatal(err)
			}
			if c.landedAt != nil {
				landtest.WriteLandingLog(t, mgr.townRoot, "rig", land.LandingRecord{
					BeadID: "gt-jhz3t0", Rig: "rig", Head: "abc123", LandedAt: *c.landedAt,
				})
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateSubmitted, "")
			if d.CountsTowardCapacity != c.want {
				t.Fatalf("CountsTowardCapacity = %v, want %v (verdict %q, reason %q)",
					d.CountsTowardCapacity, c.want, d.Verdict, d.Reason)
			}
		})
	}
}

// TestUnreadableLandingReadHoldsTheSeatAndSaysSo: when a seat's intent record
// or the rig's landing log cannot be read, the Manager must not free the seat
// on an answer it never read — the seat keeps counting toward capacity — and
// the read failure is reported instead of swallowed (gt-ap2y66).
func TestUnreadableLandingReadHoldsTheSeatAndSaysSo(t *testing.T) {
	t.Parallel()
	submittedAt := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		broken string // "intent" or "landing": which record path becomes unreadable
		warn   string // the phrase the warning must carry to say which read failed
	}{
		{name: "an unreadable intent record holds the seat", broken: "intent", warn: "intent record"},
		{name: "an unreadable landing log holds the seat", broken: "landing", warn: "landing record"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
			var warned []string
			mgr.warn = func(format string, args ...any) {
				warned = append(warned, fmt.Sprintf(format, args...))
			}
			bd.Seed(beads.Issue{ID: "gt-jhz3t0", Status: "hooked", Labels: []string{land.LabelReadyToLand}})
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.HookBead = "gt-jhz3t0" })
			seat := intent.Seat{Rig: "rig", Role: constants.RolePolecat, Name: "toast"}
			switch c.broken {
			case "intent":
				// The record's path is a directory, so the read fails.
				if err := os.MkdirAll(seat.Path(mgr.townRoot), 0o700); err != nil {
					t.Fatal(err)
				}
			case "landing":
				if err := intent.MarkSubmitted(mgr.townRoot, seat, "gt-jhz3t0", "test", submittedAt); err != nil {
					t.Fatal(err)
				}
				// The landing log's path is a directory, so the read fails.
				logPath := filepath.Join(constants.TownRuntimePath(mgr.townRoot), "landings", "rig.jsonl")
				if err := os.MkdirAll(logPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateSubmitted, "")
			if !d.CountsTowardCapacity {
				t.Fatalf("CountsTowardCapacity = false, want the seat held on an unreadable %s (verdict %q, reason %q)",
					c.broken, d.Verdict, d.Reason)
			}
			if len(warned) != 1 || !strings.Contains(warned[0], c.warn) {
				t.Fatalf("warnings = %q, want one naming the unread %s", warned, c.warn)
			}
		})
	}
}
