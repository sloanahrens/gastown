package daemon

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/seatship"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspaceid"
)

func seatEndComment(t *testing.T, p seatship.Payload) string {
	t.Helper()
	c, err := seatship.Encode(p)
	if err != nil {
		t.Fatalf("Encode(%+v): %v", p, err)
	}
	return c
}

func TestSeatEndRecordRead(t *testing.T) {
	t.Parallel()
	seat := seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Submitted: true}
	tests := []struct {
		name     string
		comments []string
		bead     string
		want     seatship.Payload
		found    bool
		wantErr  bool
	}{
		{"found", []string{seatEndComment(t, seat)}, "gt-a", seat, true, false},
		{"newest wins", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a"}), seatEndComment(t, seat)}, "gt-a", seat, true, false},
		{"foreign comment skipped", []string{seatEndComment(t, seat), "SP ESTIMATE v1 {}"}, "gt-a", seat, true, false},
		{"malformed record skipped", []string{seatEndComment(t, seat), "SEAT-END v1 {}"}, "gt-a", seat, true, false},
		{"other rig skipped", []string{seatEndComment(t, seatship.Payload{Rig: "other", Polecat: "toast", Bead: "gt-a"})}, "gt-a", seatship.Payload{}, false, false},
		{"other polecat skipped", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "nux", Bead: "gt-a"})}, "gt-a", seatship.Payload{}, false, false},
		{"other bead skipped", []string{seatEndComment(t, seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-b"})}, "gt-a", seatship.Payload{}, false, false},
		{"none", nil, "gt-a", seatship.Payload{}, false, false},
		{"missing bead", nil, "gt-missing", seatship.Payload{}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd := beadsfake.New()
			bd.Seed(beads.Issue{ID: "gt-a", Title: "seat"})
			for _, text := range tt.comments {
				if err := bd.AddComment("gt-a", text); err != nil {
					t.Fatalf("AddComment: %v", err)
				}
			}
			got, found, err := readSeatEndRecord(bd, "gastown", "toast", tt.bead)
			if (err != nil) != tt.wantErr || found != tt.found || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("readSeatEndRecord = %+v (found %v, err %v); want %+v (found %v, err %v)", got, found, err, tt.want, tt.found, tt.wantErr)
			}
		})
	}
}

// TestSeatEndRecordStore: the store pass end to end, and the payloads it refuses.
func TestSeatEndRecordStore(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	rec := usageRunRecord("gt-a", "s-1", start, start.Add(time.Hour), 100)
	seat := seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Usage: &rec, SessionIDs: []string{"s-1"}}
	badKind := rec
	badKind.Kind = ""

	filed := seat
	filed.Outbox = map[string]string{seatOutboxResultFile: "result", seatOutboxNotesFile: "notes"}
	path, err := storeSeatEndRecord(t.Context(), beadsfake.New(), town, filed)
	if err != nil {
		t.Fatalf("storeSeatEndRecord: %v", err)
	}
	usageDir := filepath.Join(constants.TownRuntimePath(town), "usage")
	if want := usageattrib.WorkRecordPath(usageDir, "gt-a"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	stored, err := usageattrib.ReadWork(usageDir, "gt-a")
	if err != nil || stored.Record.Workspace != workspaceid.CustomerZero {
		t.Errorf("stored record = %+v, %v; want workspace %q", stored.Record, err, workspaceid.CustomerZero)
	}
	dir := session.SandboxOutboxCopyDir(town, "gastown", "toast")
	for name, want := range filed.Outbox {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("outbox %s = %q, %v; want %q", name, got, err, want)
		}
	}

	tests := []struct {
		name, mapBody string
		payload       seatship.Payload
	}{
		{"no usage", "", seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a"}},
		{"usage of another bead", "", seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-b", Usage: &rec, SessionIDs: []string{"s-1"}}},
		{"polecat that steps out", "", seatship.Payload{Rig: "gastown", Polecat: "../toast", Bead: "gt-a", Usage: &rec, SessionIDs: []string{"s-1"}}},
		{"outbox file the host does not apply", "", seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Usage: &rec, SessionIDs: []string{"s-1"}, Outbox: map[string]string{"evil.sh": "x"}}},
		{"usage the record file refuses", "", seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Usage: &badKind, SessionIDs: []string{"s-1"}}},
		{"unreadable workspace map", "{not json", seat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			town := t.TempDir()
			if tt.mapBody != "" {
				writeTownFile(t, town, workspaceid.File, tt.mapBody)
			}
			if path, err := storeSeatEndRecord(t.Context(), beadsfake.New(), town, tt.payload); err == nil || path != "" {
				t.Fatalf("storeSeatEndRecord = %q, %v; want no path and an error", path, err)
			}
		})
	}
}

// TestSeatEndRecordStoreOutboxFailures: an outbox write that fails is returned
// beside the record path the caller can still report.
func TestSeatEndRecordStoreOutboxFailures(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	rec := usageRunRecord("gt-a", "s-1", start, start.Add(time.Hour), 100)
	p := seatship.Payload{Rig: "gastown", Polecat: "toast", Bead: "gt-a", Usage: &rec, SessionIDs: []string{"s-1"},
		Outbox: map[string]string{seatOutboxResultFile: "result"}}
	fail := errors.New("disk full")
	tests := []struct {
		name string
		ops  seatEndOutboxOps
	}{
		{"clearing the copy", seatEndOutboxOps{func(string) error { return fail }, os.MkdirAll, os.WriteFile}},
		{"creating the copy", seatEndOutboxOps{os.RemoveAll, func(string, os.FileMode) error { return fail }, os.WriteFile}},
		{"writing a file", seatEndOutboxOps{os.RemoveAll, os.MkdirAll, func(string, []byte, os.FileMode) error { return fail }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if path, err := storeSeatEndRecordWith(t.Context(), beadsfake.New(), t.TempDir(), p, tt.ops); !errors.Is(err, fail) || path == "" {
				t.Errorf("storeSeatEndRecordWith = %q, %v; want the record path and the %q failure", path, err, tt.name)
			}
		})
	}
}

// TestSeatEndNoRecordMarker: a seat end with neither a SEAT-END comment nor a
// transcript copy leaves one marker naming the seat and its bead; a second end
// of the same seat replaces that one file rather than adding another, and a
// marker the host cannot write is logged without failing the end (gt-gnxclz).
func TestSeatEndNoRecordMarker(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ends int
		// plant, when set, runs before the first end: a stale marker, or a
		// path the marker cannot be written under.
		plant func(t *testing.T, town string)
		// wantRefused says the marker could not be written and the end said so.
		wantRefused bool
	}{
		{name: "one marker for the seat and bead", ends: 1},
		{name: "a second end of the same seat adds no file", ends: 2},
		{name: "a stale marker is replaced, not added to", ends: 1,
			plant: func(t *testing.T, town string) {
				writeSeatEndNoRecordFile(t, town, endPolecat+".json", `{"bead":"gt-stale"}`)
			}},
		{name: "a marker the host cannot write is logged", ends: 1, wantRefused: true,
			plant: func(t *testing.T, town string) {
				if err := os.MkdirAll(seatEndNoRecordsDir(town), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(seatEndNoRecordsDir(town), endRig), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			f.agentDB.Seed(beads.Issue{ID: endBead, Title: "seat"})
			// No transcripts on the host: the pull finds nothing to copy.
			f.host.seatUsage = seatUsageHook{list: func(_, _ string) ([]string, error) { return nil, nil }}
			var logs strings.Builder
			f.host.d.logger = log.New(&logs, "", 0)
			if tt.plant != nil {
				tt.plant(t, f.town)
			}
			for i := 0; i < tt.ends; i++ {
				f.seatDirs()
				f.writeSeatRecordAt(start)
				if !f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
					t.Fatalf("end %d did not finish the seat", i)
				}
			}
			if refused := strings.Contains(logs.String(), "cannot record that its end found no record"); refused != tt.wantRefused {
				t.Fatalf("log %q says the marker was refused = %v, want %v", logs.String(), refused, tt.wantRefused)
			}
			if tt.wantRefused {
				return // the write was refused; the end still finished, checked above
			}
			rigDir := filepath.Join(seatEndNoRecordsDir(f.town), endRig)
			entries, err := os.ReadDir(rigDir)
			if err != nil || len(entries) != 1 || entries[0].Name() != endPolecat+".json" {
				t.Fatalf("markers = %v, %v; want one %s.json", entries, err, endPolecat)
			}
			data, err := os.ReadFile(filepath.Join(rigDir, endPolecat+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var marker struct{ Bead string }
			if err := json.Unmarshal(data, &marker); err != nil || marker.Bead != endBead {
				t.Errorf("marker = %q, %v; want bead %s", data, err, endBead)
			}
		})
	}
}

// writeSeatEndNoRecordFile plants one marker file under the town's rig.
func writeSeatEndNoRecordFile(t *testing.T, town, name, body string) {
	t.Helper()
	path := filepath.Join(seatEndNoRecordsDir(town), endRig, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSeatEndNoRecordMarkerRefused: a marker with no bead to clear it by, or a
// rig or polecat name no path holds, is refused rather than left out of the
// queue (gt-gnxclz).
func TestSeatEndNoRecordMarkerRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, rig, polecat, bead string
	}{
		{name: "no bead", rig: endRig, polecat: endPolecat},
		{name: "a polecat that steps out", rig: endRig, polecat: "../" + endPolecat, bead: endBead},
		{name: "a rig that steps out", rig: "../" + endRig, polecat: endPolecat, bead: endBead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := recordSeatEndNoRecord(t.TempDir(), tt.rig, tt.polecat, tt.bead); err == nil {
				t.Error("recordSeatEndNoRecord wrote a marker it should have refused")
			}
		})
	}
}
