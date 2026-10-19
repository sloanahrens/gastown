package attention

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var readBase = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

// writeEventFile writes events as JSON lines to path, creating the queue
// directory first.
func writeEventFile(t *testing.T, path string, events ...Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	var b strings.Builder
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event %s: %v", e.Key, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestReadOrdersTransitionsAcrossBothLogs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeEventFile(t, RotatedEventsPath(town),
		Event{TS: readBase.Add(3 * time.Minute), Key: "late", State: EventNew},
		Event{TS: readBase.Add(time.Minute), Key: "early", State: EventNew},
	)
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase.Add(2 * time.Minute), Key: "middle", State: EventNew},
	)

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var keys []string
	for _, e := range snap.Transitions {
		keys = append(keys, e.Key)
	}
	if want := []string{"early", "middle", "late"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("transition order = %v, want %v", keys, want)
	}
	keys = nil
	for _, r := range snap.Items {
		keys = append(keys, r.Key)
	}
	if want := []string{"early", "middle", "late"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("item order = %v, want %v", keys, want)
	}
	for _, r := range snap.Items {
		if len(r.Transitions) != 1 || r.Transitions[0].Key != r.Key {
			t.Errorf("%s transitions = %+v, want its own new line", r.Key, r.Transitions)
		}
	}
	if snap.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", snap.Skipped)
	}
}

func TestReadSkipsTruncatedLineWithCount(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := EventsPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	good := `{"ts":"2026-10-19T09:00:00Z","class":"red-main","severity":"high","text":"a","key":"a","state":"new"}`
	body := good + "\n\n" + good + "\n" + `{"ts":"2026-10-19T09:01:00Z","class":"red-ma`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Transitions) != 2 {
		t.Errorf("transitions = %d, want the 2 complete lines", len(snap.Transitions))
	}
	if snap.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the cut final line only, not the blank)", snap.Skipped)
	}
}

func TestReadReportsItemAckedBeforeItCleared(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: "acted", Class: KindEscalation, Severity: SeverityHigh, Text: "acted on", State: EventNew},
		Event{TS: readBase.Add(10 * time.Minute), Key: "acted", State: EventCleared},
		Event{TS: readBase, Key: "auto", Class: KindRedMain, Severity: SeverityHigh, Text: "cleared alone", State: EventNew},
		Event{TS: readBase.Add(10 * time.Minute), Key: "auto", State: EventCleared},
	)
	acks := Acks{Acks: []Ack{
		{Key: "acted", At: readBase.Add(5 * time.Minute)},
		{Key: "auto", At: readBase.Add(15 * time.Minute)},
	}}
	if err := WriteAcks(town, acks); err != nil {
		t.Fatalf("WriteAcks: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	byKey := make(map[string]Record, len(snap.Items))
	for _, r := range snap.Items {
		byKey[r.Key] = r
	}
	acted, ok := byKey["acted"]
	if !ok {
		t.Fatalf("records = %+v, want an acted record", snap.Items)
	}
	if !acted.Acked || acted.AckedAt == nil || !acted.AckedAt.Equal(readBase.Add(5*time.Minute)) {
		t.Errorf("acted = acked %v at %v, want acked at +5m", acted.Acked, acted.AckedAt)
	}
	if acted.ClearedAt == nil || !acted.ClearedAt.Equal(readBase.Add(10*time.Minute)) {
		t.Errorf("acted cleared = %v, want +10m", acted.ClearedAt)
	}
	auto, ok := byKey["auto"]
	if !ok {
		t.Fatalf("records = %+v, want an auto record", snap.Items)
	}
	if auto.Acked {
		t.Errorf("auto = acked at %v, want unacked: the ack came after the clear", auto.AckedAt)
	}
}

func TestReadReportsAckCurrentItemFromAcksFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	stateAcked := readBase.Add(-2 * time.Minute)
	state := State{
		Updated: readBase,
		Items: []Item{
			{Key: "from-state", Kind: KindHostDisk, Severity: SeverityHigh, Summary: "disk", FirstSeen: readBase.Add(-time.Hour), LastSeen: readBase, AckedAt: &stateAcked},
			{Key: "from-acks", Kind: KindBDSlow, Severity: SeverityLow, Summary: "bd slow", FirstSeen: readBase.Add(-time.Hour), LastSeen: readBase},
			{Key: "unacked", Kind: KindHostMem, Severity: SeverityLow, Summary: "mem", FirstSeen: readBase.Add(-time.Hour), LastSeen: readBase},
		},
	}
	if err := WriteState(town, state); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := WriteAcks(town, Acks{Acks: []Ack{{Key: "from-acks", At: readBase.Add(-time.Minute)}}}); err != nil {
		t.Fatalf("WriteAcks: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 3 {
		t.Fatalf("records = %d, want 3 from state", len(snap.Items))
	}
	for _, r := range snap.Items {
		want := r.Key != "unacked"
		if r.Acked != want {
			t.Errorf("%s acked = %v, want %v", r.Key, r.Acked, want)
		}
		if len(r.Transitions) != 0 {
			t.Errorf("%s transitions = %d, want none: no event line survived", r.Key, len(r.Transitions))
		}
	}
}

func TestReadSplitsRecurrenceIntoLives(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	newLine := func(ts time.Time) Event {
		return Event{TS: ts, Key: "red-main:gastown", Class: KindRedMain, Severity: SeverityHigh, Text: "main red", State: EventNew}
	}
	clearLine := func(ts time.Time) Event {
		return Event{TS: ts, Key: "red-main:gastown", State: EventCleared}
	}
	writeEventFile(t, EventsPath(town),
		newLine(readBase),
		clearLine(readBase.Add(10*time.Minute)),
		newLine(readBase.Add(20*time.Minute)),
	)
	if err := WriteState(town, State{
		Updated: readBase.Add(25 * time.Minute),
		Items: []Item{{
			Key: "red-main:gastown", Kind: KindRedMain, Severity: SeverityHigh,
			Summary: "main red", FirstSeen: readBase.Add(20 * time.Minute),
			LastSeen: readBase.Add(25 * time.Minute),
		}},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := WriteAcks(town, Acks{Acks: []Ack{{Key: "red-main:gastown", At: readBase.Add(5 * time.Minute)}}}); err != nil {
		t.Fatalf("WriteAcks: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("records = %d, want one per life", len(snap.Items))
	}
	first, second := snap.Items[0], snap.Items[1]
	if first.ClearedAt == nil || !first.Acked {
		t.Errorf("first life = %+v, want cleared and acked", first)
	}
	if second.ClearedAt != nil || second.Acked {
		t.Errorf("second life = %+v, want standing and unacked: the ack died with the first", second)
	}
	if !second.LastSeen.Equal(readBase.Add(25 * time.Minute)) {
		t.Errorf("second life last_seen = %v, want state's tick", second.LastSeen)
	}
}

func TestReadDoesNotResurrectItemClearedAfterState(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	key := "red-main:gastown"
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: key, Class: KindRedMain, Severity: SeverityHigh, Text: "main red", State: EventNew},
		Event{TS: readBase.Add(10 * time.Minute), Key: key, State: EventCleared},
	)
	// state.json is one tick behind the clear in events.jsonl.
	if err := WriteState(town, State{
		Updated: readBase.Add(5 * time.Minute),
		Items: []Item{{
			Key: key, Kind: KindRedMain, Severity: SeverityHigh, Summary: "main red",
			FirstSeen: readBase, LastSeen: readBase.Add(5 * time.Minute),
		}},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("records = %d, want only the cleared life", len(snap.Items))
	}
	if snap.Items[0].ClearedAt == nil {
		t.Errorf("item = %+v, want the clear to win over the stale state", snap.Items[0])
	}
}

func TestReadNeverWrites(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeEventFile(t, RotatedEventsPath(town),
		Event{TS: readBase, Key: "old", State: EventNew},
	)
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase.Add(time.Minute), Key: "old", State: EventCleared},
	)
	if err := WriteState(town, State{Updated: readBase.Add(time.Minute), Items: []Item{}}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := WriteAcks(town, Acks{Acks: []Ack{{Key: "old", At: readBase}}}); err != nil {
		t.Fatalf("WriteAcks: %v", err)
	}

	dir := Dir(town)
	// Root ignores the read-only bits below, so they cannot be the check. Age
	// every file and the directory instead: a rewrite, even of identical
	// bytes, then shows as a fresh mtime.
	aged := readBase.Add(-24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), aged, aged); err != nil {
			t.Fatalf("age %s: %v", e.Name(), err)
		}
	}
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatalf("age queue dir: %v", err)
	}
	before := readDirBytes(t, dir)
	beforeDir := dirModTime(t, dir)
	if err := makeReadOnly(t, dir); err != nil {
		t.Fatalf("make queue read-only: %v", err)
	}
	t.Cleanup(func() { makeWritable(t, dir) })

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Transitions) != 2 {
		t.Errorf("transitions = %d, want both lines", len(snap.Transitions))
	}
	after := readDirBytes(t, dir)
	if len(after) != len(before) {
		t.Fatalf("queue entries changed: before %v, after %v", sortedNames(before), sortedNames(after))
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok || string(got.data) != string(want.data) {
			t.Errorf("%s changed during a read; the reader wrote to the queue", name)
		} else if !got.modTime.Equal(want.modTime) {
			t.Errorf("%s was rewritten during a read (mtime %v, was %v)", name, got.modTime, want.modTime)
		}
	}
	if got := dirModTime(t, dir); !got.Equal(beforeDir) {
		t.Errorf("queue directory modified during a read (mtime %v, was %v)", got, beforeDir)
	}
}

func TestReadEndsEarlierLifeOfDoubleRaisedKey(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	raise := func(ts time.Time, text string) Event {
		return Event{TS: ts, Key: "red-main:gastown", Class: KindRedMain, Severity: SeverityHigh, Text: text, State: EventNew}
	}
	writeEventFile(t, EventsPath(town),
		raise(readBase, "first raise"),
		raise(readBase.Add(10*time.Minute), "second raise"),
	)
	if err := WriteState(town, State{
		Updated: readBase.Add(15 * time.Minute),
		Items: []Item{{
			Key: "red-main:gastown", Kind: KindRedMain, Severity: SeverityHigh,
			Summary: "second raise", FirstSeen: readBase.Add(10 * time.Minute),
			LastSeen: readBase.Add(15 * time.Minute),
		}},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("records = %d, want a life per raise", len(snap.Items))
	}
	first, second := snap.Items[0], snap.Items[1]
	if first.ClearedAt == nil || !first.ClearedAt.Equal(readBase.Add(10*time.Minute)) {
		t.Errorf("first life cleared = %v, want it ended at the second raise (+10m)", first.ClearedAt)
	}
	if !first.LastSeen.Equal(readBase.Add(10 * time.Minute)) {
		t.Errorf("first life last_seen = %v, want +10m", first.LastSeen)
	}
	if len(first.Transitions) != 1 || first.Summary != "first raise" {
		t.Errorf("first life = %+v, want only its own raising line", first)
	}
	if second.ClearedAt != nil || second.Summary != "second raise" {
		t.Errorf("second life = %+v, want the one standing life", second)
	}
}

func TestReadFillsKindSeverityAndRigFromTheKeysOtherRecords(t *testing.T) {
	t.Parallel()
	const key = "red-main:gastown"
	base := Event{Key: key, State: EventCleared}
	tests := []struct {
		name   string
		events []Event
		state  []Item
		want   Record // Kind, Severity, Rig of the first (cleared) life
	}{
		{
			name:   "standing state describes the key",
			events: []Event{{TS: readBase, Key: key, State: EventCleared}},
			state: []Item{{
				Key: key, Kind: KindRedMain, Severity: SeverityHigh, Rig: "gastown",
				FirstSeen: readBase.Add(time.Hour), LastSeen: readBase.Add(time.Hour),
			}},
			want: Record{Kind: KindRedMain, Severity: SeverityHigh, Rig: "gastown"},
		},
		{
			name: "a later life of the key supplies what a bare clear lacks",
			events: []Event{
				{TS: readBase, Key: key, State: EventCleared},
				{TS: readBase.Add(time.Hour), Key: key, Class: KindRedMain, Severity: SeverityLow, Text: "again", State: EventNew},
			},
			want: Record{Kind: KindRedMain, Severity: SeverityLow},
		},
		{
			name:   "nothing else knows the key",
			events: []Event{base},
			want:   Record{},
		},
		{
			name: "a line's own values are kept",
			events: []Event{
				{TS: readBase, Key: key, Class: KindHostDisk, Severity: SeverityLow, State: EventCleared},
				{TS: readBase.Add(time.Hour), Key: key, Class: KindRedMain, Severity: SeverityHigh, Text: "again", State: EventNew},
			},
			want: Record{Kind: KindHostDisk, Severity: SeverityLow},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeEventFile(t, EventsPath(town), tt.events...)
			if len(tt.state) > 0 {
				if err := WriteState(town, State{Updated: readBase.Add(2 * time.Hour), Items: tt.state}); err != nil {
					t.Fatalf("WriteState: %v", err)
				}
			}
			snap, err := Read(town)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if len(snap.Items) == 0 {
				t.Fatalf("records = none, want the cleared life first")
			}
			got := snap.Items[0]
			if got.ClearedAt == nil {
				t.Fatalf("first record = %+v, want the cleared life", got)
			}
			if got.Kind != tt.want.Kind || got.Severity != tt.want.Severity || got.Rig != tt.want.Rig {
				t.Errorf("kind/severity/rig = %q/%q/%q, want %q/%q/%q",
					got.Kind, got.Severity, got.Rig, tt.want.Kind, tt.want.Severity, tt.want.Rig)
			}
		})
	}
}

func TestReadFoldsStateWithoutTimes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: "no-times", Class: KindHostDisk, Severity: SeverityLow, Text: "disk", State: EventNew},
	)
	// The state item carries no first_seen or last_seen: the new line's time
	// must survive the fold rather than be zeroed.
	if err := WriteState(town, State{
		Updated: readBase.Add(time.Minute),
		Items:   []Item{{Key: "no-times", Kind: KindHostDisk, Severity: SeverityLow, Summary: "disk"}},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("records = %d, want 1", len(snap.Items))
	}
	if r := snap.Items[0]; !r.FirstSeen.Equal(readBase) || !r.LastSeen.Equal(readBase) {
		t.Errorf("first/last seen = %v/%v, want the new line's %v", r.FirstSeen, r.LastSeen, readBase)
	}
}

func TestReadStateOnlyRecurrenceAfterClear(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	key := "red-main:gastown"
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: key, Class: KindRedMain, Severity: SeverityHigh, Text: "main red", State: EventNew},
		Event{TS: readBase.Add(10 * time.Minute), Key: key, State: EventCleared},
	)
	// The recurrence's new line is not in the retained log, but state.json
	// holds it with a first_seen past the clear.
	if err := WriteState(town, State{
		Updated: readBase.Add(30 * time.Minute),
		Items: []Item{{
			Key: key, Kind: KindRedMain, Severity: SeverityHigh, Rig: "gastown", Summary: "main red",
			FirstSeen: readBase.Add(20 * time.Minute), LastSeen: readBase.Add(30 * time.Minute),
		}},
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("records = %d, want the cleared life and the state-only recurrence", len(snap.Items))
	}
	if first := snap.Items[0]; first.ClearedAt == nil {
		t.Errorf("first life = %+v, want cleared", first)
	}
	if second := snap.Items[1]; second.ClearedAt != nil || second.Rig != "gastown" || len(second.Transitions) != 0 {
		t.Errorf("second life = %+v, want standing from state alone", second)
	}
}

func TestReadUsesLatestAckInForce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: "k", Class: KindEscalation, Severity: SeverityHigh, Text: "x", State: EventNew},
		Event{TS: readBase.Add(30 * time.Minute), Key: "k", State: EventCleared},
	)
	// Acks out of order: the latest one at or before the clear wins, and the
	// one after the clear is ignored.
	if err := WriteAcks(town, Acks{Acks: []Ack{
		{Key: "k", At: readBase.Add(20 * time.Minute)},
		{Key: "k", At: readBase.Add(5 * time.Minute)},
		{Key: "k", At: readBase.Add(45 * time.Minute)},
		{Key: "other", At: readBase.Add(25 * time.Minute)},
	}}); err != nil {
		t.Fatalf("WriteAcks: %v", err)
	}

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 1 {
		t.Fatalf("records = %d, want 1", len(snap.Items))
	}
	if r := snap.Items[0]; !r.Acked || r.AckedAt == nil || !r.AckedAt.Equal(readBase.Add(20*time.Minute)) {
		t.Errorf("record = acked %v at %v, want the +20m ack", r.Acked, r.AckedAt)
	}
}

func TestReadRanksLivesThatStartTogether(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// Two keys raised in the same instant sort by key; one key whose cleared
	// life and standing life share an instant puts the cleared one first.
	writeEventFile(t, EventsPath(town),
		Event{TS: readBase, Key: "b", Class: KindHostMem, Severity: SeverityLow, Text: "b", State: EventNew},
		Event{TS: readBase, Key: "a", Class: KindHostMem, Severity: SeverityLow, Text: "a", State: EventNew},
		Event{TS: readBase, Key: "c", State: EventCleared},
		Event{TS: readBase, Key: "c", Class: KindHostMem, Severity: SeverityLow, Text: "c", State: EventNew},
	)

	snap, err := Read(town)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var got []string
	for _, r := range snap.Items {
		suffix := ""
		if r.ClearedAt != nil {
			suffix = "(cleared)"
		}
		got = append(got, r.Key+suffix)
	}
	if want := "a,b,c(cleared),c"; strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %s", got, want)
	}
}

func TestReadSurfacesUnreadableFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, town string)
	}{
		{"corrupt state.json", func(t *testing.T, town string) {
			writeRaw(t, StatePath(town), "{not json")
		}},
		{"corrupt acks.json", func(t *testing.T, town string) {
			writeRaw(t, AcksPath(town), "{not json")
		}},
		{"rotated events path is a directory", func(t *testing.T, town string) {
			if err := os.MkdirAll(RotatedEventsPath(town), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
		}},
		{"events path is a directory", func(t *testing.T, town string) {
			if err := os.MkdirAll(EventsPath(town), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tt.setup(t, town)
			if _, err := Read(town); err == nil {
				t.Error("Read succeeded, want the file's error")
			}
		})
	}
}

// writeRaw writes body to path, creating the queue directory first.
func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestReadMissingQueueReadsEmpty(t *testing.T) {
	t.Parallel()
	snap, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(snap.Items) != 0 || len(snap.Transitions) != 0 || snap.Skipped != 0 {
		t.Errorf("snapshot = %+v, want empty", snap)
	}
}

// fileState is a queue file as the read-only check compares it.
type fileState struct {
	data    []byte
	modTime time.Time
}

// readDirBytes returns every regular file in dir with its contents and mtime.
func readDirBytes(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := make(map[string]fileState, len(entries))
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat %s: %v", e.Name(), err)
		}
		out[e.Name()] = fileState{data: data, modTime: info.ModTime()}
	}
	return out
}

func dirModTime(t *testing.T, dir string) time.Time {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat %s: %v", dir, err)
	}
	return info.ModTime()
}

func sortedNames(m map[string]fileState) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// makeReadOnly strips every write bit from the queue, so any write the reader
// attempts fails. Root ignores the bits, so the caller also compares contents
// and mtimes, which holds either way.
func makeReadOnly(t *testing.T, dir string) error {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(dir, e.Name()), 0o444); err != nil {
			return err
		}
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	return os.Chmod(dir, 0o555)
}

func makeWritable(t *testing.T, dir string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
	if err := os.Chmod(dir, 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("restore queue dir permissions: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if err := os.Chmod(filepath.Join(dir, e.Name()), 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("restore %s permissions: %v", e.Name(), err)
		}
	}
}
