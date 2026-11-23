package daemon

import (
	"os"
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
// record of its own, no comment, an unreadable bead or a predecessor's comment,
// runs the pulls the end always ran; a record that cannot be stored keeps the
// seat, the only copy of what it shipped (gt-g73e2q).
func TestSeatEndRecordWire(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	rec := usageRunRecord(endBead, "s-1", start, start.Add(time.Hour), 100)
	seat := seatship.Payload{Rig: endRig, Polecat: endPolecat, Bead: endBead, Submitted: true, Usage: &rec, SessionIDs: []string{"s-1"}}
	priorRec := usageRunRecord(endBead, "s-0", start.Add(-2*time.Hour), start.Add(-time.Hour), 50)
	prior := seatship.Payload{Rig: endRig, Polecat: endPolecat, Bead: endBead, Submitted: true, Usage: &priorRec, SessionIDs: []string{"s-0"}}
	tests := []struct {
		name string
		// seedBead creates the landing bead whose comments carry the record; an
		// empty value leaves no bead, so its comments cannot be read.
		seedBead string
		// payload is the comment on the bead: this seat's record, a predecessor's, or nil for none.
		payload               *seatship.Payload
		exists, noStore       bool // a held sandbox, no usage store
		brokenStart           bool // the seat's start cannot be read
		wantOutbox, wantPull  bool
		wantDeleted, wantGone bool
		wantRows              int
	}{
		{name: "a record is stored and the pulls are not run", seedBead: endBead, payload: &seat, exists: true, wantDeleted: true, wantGone: true, wantRows: 1},
		{name: "no record runs the pulls", seedBead: endBead, exists: true, wantOutbox: true, wantPull: true, wantDeleted: true, wantGone: true},
		{name: "an unreadable bead runs the pull", wantPull: true, wantGone: true},
		{name: "a prior seat's record runs the pulls", seedBead: endBead, payload: &prior, exists: true, wantOutbox: true, wantPull: true, wantDeleted: true, wantGone: true},
		{name: "an unknown start runs the pulls", seedBead: endBead, payload: &seat, brokenStart: true, wantPull: true, wantGone: true},
		{name: "a record that cannot be stored keeps the seat", seedBead: endBead, payload: &seat, noStore: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			f.seatDirs()
			f.writeSeatRecordAt(start)
			if tt.brokenStart {
				path := filepath.Join(session.SandboxRegistryDir(f.town), f.host.sessionName(endRig, endPolecat)+".json")
				if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.seedBead != "" {
				f.agentDB.Seed(beads.Issue{ID: tt.seedBead, Title: "seat"})
			}
			if tt.payload != nil {
				// A copy per row: a store stamps the payload's usage with its workspace.
				p, usage := *tt.payload, *tt.payload.Usage
				p.Usage = &usage
				if err := f.agentDB.AddComment(endBead, seatEndComment(t, p)); err != nil {
					t.Fatalf("AddComment: %v", err)
				}
			}
			if tt.exists {
				f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
				f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
				if err := session.WriteSandboxRecord(f.town, session.SandboxRecord{Session: f.host.sessionName(endRig, endPolecat), Rig: endRig, Polecat: endPolecat, Gateway: "gw1", CreatedAt: start}); err != nil {
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
			rows, err := f.agentDB.ReadUsageRecords(t.Context(), time.Time{}, 0)
			if err != nil || len(rows) != tt.wantRows {
				t.Fatalf("usage rows = %d (%v), want %d", len(rows), err, tt.wantRows)
			}
			if tt.wantRows == 0 {
				return
			}
			stored, err := usageattrib.ReadWork(filepath.Join(constants.TownRuntimePath(f.town), "usage"), endBead)
			if err != nil || stored.Record.Workspace != workspaceid.CustomerZero {
				t.Errorf("stored record = %+v, %v; want the comment's usage filed", stored.Record, err)
			}
		})
	}
}
