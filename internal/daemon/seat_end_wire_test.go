package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/seatship"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

// TestSeatEndRecordWire: a seat that shipped a SEAT-END comment has the record
// stored and its outbox and transcript pulls skipped; a seat with no readable
// record runs the pulls the end always ran; a record that cannot be stored
// keeps the seat, the only copy of what it shipped (gt-g73e2q).
func TestSeatEndRecordWire(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	rec := usageRunRecord(endBead, "s-1", start, start.Add(time.Hour), 100)
	seat := seatship.Payload{Rig: endRig, Polecat: endPolecat, Bead: endBead, Submitted: true,
		Usage: &rec, SessionIDs: []string{"s-1"}}
	tests := []struct {
		name string
		// seedBead creates the landing bead whose comments carry the record.
		seedBead                 string
		comment, exists, noStore bool // the record, a held sandbox, no usage store
		wantOutbox, wantPull     bool
		wantDeleted, wantGone    bool
	}{
		{name: "a record is stored and the pulls are not run", seedBead: endBead, comment: true,
			exists: true, wantDeleted: true, wantGone: true},
		{name: "no record runs the pulls", seedBead: endBead,
			exists: true, wantOutbox: true, wantPull: true, wantDeleted: true, wantGone: true},
		{name: "an unreadable bead runs the pull", wantPull: true, wantGone: true},
		{name: "a record that cannot be stored keeps the seat", seedBead: endBead, comment: true, noStore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			f.seatDirs()
			f.writeSeatRecordAt(start)
			if tt.seedBead != "" {
				f.agentDB.Seed(beads.Issue{ID: tt.seedBead, Title: "seat"})
			}
			if tt.comment {
				if err := f.agentDB.AddComment(endBead, seatEndComment(t, seat)); err != nil {
					t.Fatalf("AddComment: %v", err)
				}
			}
			if tt.exists {
				f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
				f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
				if err := session.WriteSandboxRecord(f.town, session.SandboxRecord{Session: f.host.sessionName(endRig, endPolecat),
					Rig: endRig, Polecat: endPolecat, Gateway: "gw1"}); err != nil {
					t.Fatal(err)
				}
			}
			var outboxPulled, pulled bool
			f.host.sandboxOutboxCopy = func(string, session.SandboxRecord) error { outboxPulled = true; return nil }
			f.host.seatUsage = seatUsageHook{
				list: func(_, _ string) ([]string, error) {
					pulled = true
					path := filepath.Join(t.TempDir(), "s-1.jsonl")
					writeUsageTranscript(t, path, start.Add(30*time.Minute), seatCallLine("new", 100, 50, 20))
					return []string{path}, nil
				},
				store: f.agentDB,
				write: func(string, dashboard.UsageRecord, []string) error { return nil },
			}
			if tt.noStore {
				f.host.seatUsage.store = nil
			}

			got := f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
			if deleted := len(f.deleted) > 0; got != tt.wantGone || pulled != tt.wantPull || outboxPulled != tt.wantOutbox || deleted != tt.wantDeleted {
				t.Fatalf("end = %v, transcript pull = %v, outbox pull = %v, deleted = %v; want %v, %v, %v, %v (%v)",
					got, pulled, outboxPulled, deleted, tt.wantGone, tt.wantPull, tt.wantOutbox, tt.wantDeleted, f.deleted)
			}
			if !tt.wantGone || !tt.comment {
				return
			}
			stored, err := usageattrib.ReadWork(filepath.Join(constants.TownRuntimePath(f.town), "usage"), endBead)
			if err != nil || stored.Record.Workspace != workspaceid.CustomerZero {
				t.Errorf("stored record = %+v, %v; want the comment's usage filed", stored.Record, err)
			}
		})
	}
}
