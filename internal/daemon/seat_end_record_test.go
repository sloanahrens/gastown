package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
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

// writeSeatEndNoRecordAt writes body at rel under the marker directory and
// returns that path.
func writeSeatEndNoRecordAt(t *testing.T, town, rel, body string) string {
	t.Helper()
	path := filepath.Join(seatEndNoRecordsDir(town), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSeatEndNoRecordUnreadableMarkerIsAnError: a marker the read cannot use
// is an error naming its file, never a seat quietly dropped from the read
// (gt-wssxhd).
func TestSeatEndNoRecordUnreadableMarkerIsAnError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// plant writes the bad marker and returns its path.
		plant func(t *testing.T, town string) string
	}{
		{name: "malformed json", plant: func(t *testing.T, town string) string {
			return writeSeatEndNoRecordAt(t, town, filepath.Join(endRig, endPolecat+".json"), "{")
		}},
		{name: "a marker naming no bead", plant: func(t *testing.T, town string) string {
			return writeSeatEndNoRecordAt(t, town, filepath.Join(endRig, endPolecat+".json"), "{}")
		}},
		{name: "a name that is not a seat name", plant: func(t *testing.T, town string) string {
			return writeSeatEndNoRecordAt(t, town, filepath.Join(endRig, "bad name.json"), `{"bead":"`+endBead+`"}`)
		}},
		{name: "a directory named like a marker", plant: func(t *testing.T, town string) string {
			path := filepath.Join(seatEndNoRecordsDir(town), endRig, endPolecat+".json")
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "a file outside a rig directory", plant: func(t *testing.T, town string) string {
			return writeSeatEndNoRecordAt(t, town, "stray", "x")
		}},
		{name: "a marker directory that is a file", plant: func(t *testing.T, town string) string {
			return writeSeatEndNoRecordAt(t, town, "", "x")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			want := tt.plant(t, town)
			markers, err := readSeatEndNoRecords(town)
			if err == nil {
				t.Fatalf("readSeatEndNoRecords read %s without an error, markers %+v", want, markers)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		})
	}
}

// TestSeatEndNoRecordIgnoresATempFile: an atomic write's temp file beside a
// marker is not a marker, so the read returns the real one alone (gt-wssxhd).
func TestSeatEndNoRecordIgnoresATempFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := recordSeatEndNoRecord(town, endRig, endPolecat, endBead); err != nil {
		t.Fatalf("recordSeatEndNoRecord: %v", err)
	}
	// An atomic write names its temp file <marker>.json.tmp.<random>, so it
	// does not end in .json and the read skips it rather than refusing on it.
	writeSeatEndNoRecordFile(t, town, endPolecat+".json.tmp.12345", `{"bead":"gt-stale"}`)
	markers, err := readSeatEndNoRecords(town)
	if err != nil {
		t.Fatalf("readSeatEndNoRecords: %v", err)
	}
	if len(markers) != 1 {
		t.Fatalf("markers = %+v; want the one real marker", markers)
	}
	got := markers[0]
	if got.Rig != endRig || got.Polecat != endPolecat || got.Bead != endBead {
		t.Errorf("marker = %+v; want %s/%s bead %s", got, endRig, endPolecat, endBead)
	}
}

// seatEndNoRecordComment is how the fixture's SEAT-END comment read answers one
// seat: its record stands on the bead, or the read failed. A seat absent from
// the fixture's map answers that no record stands.
type seatEndNoRecordComment int

const (
	seatEndHasComment seatEndNoRecordComment = iota + 1
	seatEndCommentUnreadable
)

// seatEndNoRecordSource is the fixture's sources with the SEAT-END comment read
// answering each seat as comments says, keyed "rig/polecat/bead".
func seatEndNoRecordSource(f *attentionFixture, comments map[string]seatEndNoRecordComment) *attentionSources {
	f.src.seatEndRecord = func(rig, name, bead string) (bool, error) {
		switch comments[rig+"/"+name+"/"+bead] {
		case seatEndHasComment:
			return true, nil
		case seatEndCommentUnreadable:
			return false, errors.New("bd did not answer the comment read")
		}
		return false, nil
	}
	return f.src
}

// TestSeatEndNoRecordRaised: a seat whose marker says its end found no record
// raises one high seat-end-no-record item naming the seat and bead; the item
// clears when its SEAT-END comment appears, and a comment read that does not
// answer raises it rather than passing as cleared (gt-oa16rk).
func TestSeatEndNoRecordRaised(t *testing.T) {
	t.Parallel()
	seat := endRig + "/" + endPolecat + "/" + endBead
	other := endRig + "/basalt/gt-b2c3d"
	tests := []struct {
		name string
		// markers are the seats whose end left a marker, "rig/polecat/bead".
		markers []string
		// comments is how the SEAT-END comment read answers, by the same key.
		comments map[string]seatEndNoRecordComment
		// productionRead leaves the seam nil, so the collector makes its own read
		// through the daemon's recovery client: the fake holds no bead, so the
		// read fails.
		productionRead bool
		// plant, when set, runs before the collect: a marker the store cannot use.
		plant func(t *testing.T, town string)
		// wantLog says the failed comment read is logged by seat.
		wantLog bool
		want    int
		wantErr bool
	}{
		{name: "a marked seat with no comment raises one item naming the seat and bead",
			markers: []string{seat}, want: 1},
		{name: "the SEAT-END comment clears the item",
			markers: []string{seat}, comments: map[string]seatEndNoRecordComment{seat: seatEndHasComment}},
		{name: "an unreadable comment read raises the item",
			markers: []string{seat}, comments: map[string]seatEndNoRecordComment{seat: seatEndCommentUnreadable}, want: 1, wantLog: true},
		{name: "the collector's own read fails on a bead the client cannot answer",
			markers: []string{seat}, productionRead: true, want: 1, wantLog: true},
		{name: "two seats raise two items",
			markers: []string{seat, other}, want: 2},
		{name: "a marker the store cannot use fails the collect",
			plant: func(t *testing.T, town string) {
				writeSeatEndNoRecordFile(t, town, endPolecat+".json", "{")
			}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			town := f.d.config.TownRoot
			for _, marker := range tt.markers {
				rig, rest, _ := strings.Cut(marker, "/")
				name, bead, _ := strings.Cut(rest, "/")
				if err := recordSeatEndNoRecord(town, rig, name, bead); err != nil {
					t.Fatalf("recordSeatEndNoRecord(%q): %v", marker, err)
				}
			}
			if tt.plant != nil {
				tt.plant(t, town)
			}
			s := f.src
			if tt.productionRead {
				f.d.openRecoveryBeads = func([]string) beads.Client { return beadsfake.New() }
			} else {
				s = seatEndNoRecordSource(f, tt.comments)
			}
			c := collectorFor(t, s, attention.KindSeatEndNoRecord)

			items, err := c.collect(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("collect err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if items != nil {
					t.Errorf("items = %+v beside an unusable marker store, want none", items)
				}
				return
			}
			if len(items) != tt.want {
				t.Fatalf("items = %+v, want %d", items, tt.want)
			}
			if tt.want != 1 {
				return
			}
			it := items[0]
			if it.Key != "seat-end-no-record:"+endRig+"/"+endPolecat || it.Kind != attention.KindSeatEndNoRecord ||
				it.Severity != attention.SeverityHigh || it.Rig != endRig || it.Bead != endBead {
				t.Errorf("item = %+v, want a high seat-end-no-record item on %s/%s and %s", it, endRig, endPolecat, endBead)
			}
			if !strings.Contains(it.Summary, endRig+"/"+endPolecat) || !strings.Contains(it.Summary, endBead) {
				t.Errorf("summary %q does not name the seat and its bead", it.Summary)
			}
			if tt.wantLog && !strings.Contains(f.logs.String(), "seat-end-no-record "+endRig+"/"+endPolecat) {
				t.Errorf("log %q does not name the seat whose comment read failed", f.logs.String())
			}
		})
	}
}

// seatEndNoRecordKey is the key one seat's marker raises.
func seatEndNoRecordKey(rig, name string) string { return "seat-end-no-record:" + rig + "/" + name }

// TestSeatEndNoRecordClears: a marked seat's item clears on the tick its bead
// carries a SEAT-END comment naming the seat, read through the real decode.
func TestSeatEndNoRecordClears(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	key := seatEndNoRecordKey(endRig, endPolecat)
	bd := beadsfake.New()
	bd.Seed(beads.Issue{ID: endBead, Title: "seat"})
	f.d.openRecoveryBeads = func([]string) beads.Client { return bd }
	if err := recordSeatEndNoRecord(f.d.config.TownRoot, endRig, endPolecat, endBead); err != nil {
		t.Fatalf("recordSeatEndNoRecord: %v", err)
	}
	if _, ok := attention.Find(f.tick(t, keptSeatAttentionNow), key); !ok {
		t.Fatalf("first tick holds no %s item, want the marked seat's", key)
	}
	if err := bd.AddComment(endBead, seatEndComment(t, seatship.Payload{Rig: endRig, Polecat: endPolecat, Bead: endBead})); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if it, ok := attention.Find(f.tick(t, keptSeatAttentionNow.Add(time.Minute)), key); ok {
		t.Fatalf("item %+v stands after its SEAT-END comment appeared, want cleared", it)
	}
	for _, e := range f.events(t) {
		if e.Key == key && e.State == attention.EventCleared {
			return
		}
	}
	t.Errorf("events hold no cleared transition for %s", key)
}

// TestSeatEndNoRecordOneItemPerSeat: a seat holds one item however many ticks
// its marker stands, keyed by the seat, so another bead replaces that item.
func TestSeatEndNoRecordOneItemPerSeat(t *testing.T) {
	t.Parallel()
	seat := seatEndNoRecord{Rig: endRig, Polecat: endPolecat, Bead: endBead}
	tests := []struct {
		name    string
		markers []seatEndNoRecord
		rewrite *seatEndNoRecord  // replaces the marker between the ticks
		want    map[string]string // key -> bead after the second tick
	}{
		{name: "one seat over two ticks", markers: []seatEndNoRecord{seat},
			want: map[string]string{seatEndNoRecordKey(endRig, endPolecat): endBead}},
		{name: "a marker rewritten with another bead", markers: []seatEndNoRecord{seat},
			rewrite: &seatEndNoRecord{Rig: endRig, Polecat: endPolecat, Bead: "gt-other"},
			want:    map[string]string{seatEndNoRecordKey(endRig, endPolecat): "gt-other"}},
		{name: "two seats", markers: []seatEndNoRecord{seat, {Rig: endRig, Polecat: "basalt", Bead: "gt-b2c3d"}},
			want: map[string]string{seatEndNoRecordKey(endRig, endPolecat): endBead, seatEndNoRecordKey(endRig, "basalt"): "gt-b2c3d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, keptSeatAttentionNow)
			for _, m := range tt.markers {
				if err := recordSeatEndNoRecord(f.d.config.TownRoot, m.Rig, m.Polecat, m.Bead); err != nil {
					t.Fatalf("recordSeatEndNoRecord: %v", err)
				}
			}
			seatEndNoRecordSource(f, nil) // no seat carries a SEAT-END comment
			f.tick(t, keptSeatAttentionNow)
			if tt.rewrite != nil {
				if err := recordSeatEndNoRecord(f.d.config.TownRoot, tt.rewrite.Rig, tt.rewrite.Polecat, tt.rewrite.Bead); err != nil {
					t.Fatalf("rewriting the marker: %v", err)
				}
			}
			st := f.tick(t, keptSeatAttentionNow.Add(time.Minute))
			got := map[string]string{}
			for _, it := range st.Items {
				if !strings.HasPrefix(it.Key, "seat-end-no-record:") {
					continue
				}
				got[it.Key] = it.Bead
				if it.FirstSeen.After(keptSeatAttentionNow) {
					t.Errorf("%s FirstSeen = %v, want the first tick's time", it.Key, it.FirstSeen)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("items = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSeatEndNoRecordNoReaderIsAnError: a clear check the bead reader cannot
// answer is an error, never a record that cleared, so the item stands.
func TestSeatEndNoRecordNoReaderIsAnError(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	key := seatEndNoRecordKey(endRig, endPolecat)
	if err := recordSeatEndNoRecord(f.d.config.TownRoot, endRig, endPolecat, endBead); err != nil {
		t.Fatalf("recordSeatEndNoRecord: %v", err)
	}
	// A bead reader holding no bead answers the clear check with an error.
	f.d.openRecoveryBeads = func([]string) beads.Client { return beadsfake.New() }
	h := &patrolScanHost{d: f.d, openRecoveryBeads: f.d.openRecoveryBeads}
	if _, found, err := h.seatEndRecord(endRig, endPolecat, endBead); err == nil || found {
		t.Fatalf("seatEndRecord = found %v, err %v; want the no-answer error", found, err)
	}
	items, err := collectorFor(t, f.src, attention.KindSeatEndNoRecord).collect(context.Background())
	if err != nil || len(items) != 1 || items[0].Key != key {
		t.Fatalf("collect = %+v, %v; want the seat's one item standing through the failed read", items, err)
	}
	if !strings.Contains(f.logs.String(), "seat-end-no-record "+endRig+"/"+endPolecat) {
		t.Errorf("log %q does not name the seat whose clear check failed", f.logs.String())
	}
}

// TestSeatEndNoRecordFailedCollectKeepsOnlyItsItems: a failed collector keeps
// the previous items of its kind and key alone, and the log names it.
func TestSeatEndNoRecordFailedCollectKeepsOnlyItsItems(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	writeSeatEndNoRecordFile(t, f.d.config.TownRoot, endPolecat+".json", "{") // fails the collect
	prev := attention.State{Items: []attention.Item{
		{Kind: attention.KindSeatEndNoRecord, Key: seatEndNoRecordKey(endRig, endPolecat)},
		{Kind: attention.KindSeatEndNoRecord, Key: "imported:gt-x"},
		{Kind: attention.KindRedMain, Key: seatEndNoRecordKey(endRig, "other")},
	}}
	items := collectAttention(context.Background(), prev,
		[]attentionCollector{collectorFor(t, f.src, attention.KindSeatEndNoRecord)}, f.d.logger.Printf)
	if len(items) != 1 || items[0].Key != seatEndNoRecordKey(endRig, endPolecat) ||
		!strings.Contains(f.logs.String(), "attention: collector seat-end-no-record: ") {
		t.Errorf("items = %+v beside log %q, want only the seat's item kept and the failure named", items, f.logs.String())
	}
}
