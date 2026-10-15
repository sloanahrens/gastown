package attention

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreStateRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	acked := reconcileNow.Add(-time.Minute)
	want := State{
		Updated: reconcileNow,
		Items: []Item{{
			Key: "red-main:gastown", Kind: KindRedMain, Severity: SeverityHigh,
			Rig: "gastown", Bead: "gt-abc", SHA: "6cf8b456",
			Summary: "main is red", FirstSeen: reconcileNow.Add(-time.Hour),
			LastSeen: reconcileNow, AckedAt: &acked,
		}},
	}
	if err := WriteState(town, want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	got, err := ReadState(town)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if !got.Updated.Equal(want.Updated) || len(got.Items) != 1 {
		t.Fatalf("state = %+v, want %+v", got, want)
	}
	g, w := got.Items[0], want.Items[0]
	if g.Key != w.Key || g.Kind != w.Kind || g.Severity != w.Severity || g.Rig != w.Rig ||
		g.Bead != w.Bead || g.SHA != w.SHA || g.Summary != w.Summary {
		t.Errorf("item = %+v, want %+v", g, w)
	}
	if !g.FirstSeen.Equal(w.FirstSeen) || !g.LastSeen.Equal(w.LastSeen) {
		t.Errorf("item times = %v..%v, want %v..%v", g.FirstSeen, g.LastSeen, w.FirstSeen, w.LastSeen)
	}
	if g.AckedAt == nil || !g.AckedAt.Equal(*w.AckedAt) {
		t.Errorf("acked_at = %v, want %v", g.AckedAt, w.AckedAt)
	}
}

func TestStoreMissingDirReadsEmpty(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	s, err := ReadState(town)
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if !s.Updated.IsZero() || len(s.Items) != 0 {
		t.Errorf("state = %+v, want empty", s)
	}
	events, err := ReadEvents(town)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("events = %d, want 0", len(events))
	}
	acks, err := ReadAcks(town)
	if err != nil {
		t.Fatalf("ReadAcks: %v", err)
	}
	if len(acks.Acks) != 0 {
		t.Errorf("acks = %d, want 0", len(acks.Acks))
	}
}

func TestStoreAppendEventsAndRotate(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	first := Event{TS: reconcileNow, Class: KindEscalation, Severity: SeverityHigh, Text: "one", Key: "esc:hq-1", State: EventNew}
	if err := AppendEvents(town, []Event{first}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	got, err := ReadEvents(town)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != 1 || got[0].Key != "esc:hq-1" || got[0].State != EventNew {
		t.Fatalf("events = %+v, want the one appended", got)
	}

	// Fill events.jsonl past the cap; the append that crosses it rotates the
	// file to events.jsonl.1 and starts a fresh one.
	big := strings.Repeat("x", 1024)
	var batch []Event
	for i := 0; i < 1100; i++ {
		batch = append(batch, Event{
			TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow,
			Text: big, Key: "k", State: EventNew,
		})
	}
	if err := AppendEvents(town, batch); err != nil {
		t.Fatalf("AppendEvents batch: %v", err)
	}
	if _, err := os.Stat(RotatedEventsPath(town)); err != nil {
		t.Fatalf("rotated file: %v", err)
	}
	fi, err := os.Stat(EventsPath(town))
	if err != nil {
		t.Fatalf("events file: %v", err)
	}
	if fi.Size() >= EventsMaxBytes {
		t.Errorf("events.jsonl = %d bytes, want under the %d cap", fi.Size(), EventsMaxBytes)
	}
	rotated, err := os.Stat(RotatedEventsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Size() < EventsMaxBytes {
		t.Errorf("rotated = %d bytes, want at least a full cap", rotated.Size())
	}
}

func TestStoreConcurrentAckLosesNoAck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	keys := []string{"esc:hq-1", "esc:hq-2"}
	var wg sync.WaitGroup
	errs := make([]error, len(keys))
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			errs[i] = Acknowledge(town, key, reconcileNow)
		}(i, key)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ack %s: %v", keys[i], err)
		}
	}
	acks, err := ReadAcks(town)
	if err != nil {
		t.Fatalf("ReadAcks: %v", err)
	}
	if len(acks.Acks) != len(keys) {
		t.Fatalf("acks = %d, want %d: %+v", len(acks.Acks), len(keys), acks.Acks)
	}
	for _, key := range keys {
		if !AckedKey(acks, key) {
			t.Errorf("ack %q was lost", key)
		}
	}
}

// gt-cuzjj: the daemon's prune re-reads acks.json inside the lock, so an ack
// that lands after the daemon's own read is kept when its item is still in the
// state. Pruning the acks that read produced would drop it.
func TestAttentionPruneAcksKeepsAConcurrentAck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// The daemon's read: one ack, for an item that has since cleared.
	if err := Acknowledge(town, "red-main:gastown:internal/cmd", reconcileNow); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	// The human acks a live item while the daemon's tick is running.
	if err := Acknowledge(town, "esc:hq-1", reconcileNow); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	state := State{Updated: reconcileNow, Items: []Item{{Key: "esc:hq-1"}}}
	if err := PruneAcks(town, state); err != nil {
		t.Fatalf("PruneAcks: %v", err)
	}
	acks, err := ReadAcks(town)
	if err != nil {
		t.Fatalf("ReadAcks: %v", err)
	}
	if !AckedKey(acks, "esc:hq-1") {
		t.Errorf("acks = %+v, want the ack made mid-tick kept", acks.Acks)
	}
	if AckedKey(acks, "red-main:gastown:internal/cmd") {
		t.Errorf("acks = %+v, want the cleared item's ack dropped", acks.Acks)
	}
}

// A tick with nothing to drop leaves acks.json alone: the prune does not
// create the file, and does not rewrite one whose acks all still hold.
func TestAttentionPruneAcksWritesNothingWhenNothingIsDropped(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := PruneAcks(town, State{}); err != nil {
		t.Fatalf("PruneAcks on an empty town: %v", err)
	}
	if _, err := os.Stat(AcksPath(town)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PruneAcks wrote acks.json with nothing to prune: %v", err)
	}

	if err := Acknowledge(town, "esc:hq-1", reconcileNow); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	before, err := os.Stat(AcksPath(town))
	if err != nil {
		t.Fatal(err)
	}
	state := State{Updated: reconcileNow, Items: []Item{{Key: "esc:hq-1"}}}
	if err := PruneAcks(town, state); err != nil {
		t.Fatalf("PruneAcks: %v", err)
	}
	after, err := os.Stat(AcksPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("acks.json was rewritten at %v, want the %v it was written at", after.ModTime(), before.ModTime())
	}
}

func TestStoreStale(t *testing.T) {
	t.Parallel()
	if Stale(State{}, reconcileNow, StaleAfter) {
		t.Error("a state with no tick time is empty, not stale")
	}
	old := State{Updated: reconcileNow.Add(-StaleAfter)}
	if !Stale(old, reconcileNow, StaleAfter) {
		t.Error("a state at the bound is stale")
	}
	fresh := State{Updated: reconcileNow.Add(-time.Minute)}
	if Stale(fresh, reconcileNow, StaleAfter) {
		t.Error("a one-minute-old state is not stale")
	}
}

// halfWrite writes half the data and then fails, the way a full disk does.
func halfWrite(f *os.File, b []byte) (int, error) {
	n, _ := f.Write(b[:len(b)/2])
	return n, errors.New("disk full")
}

func TestStoreAppendEventsFailedBatchLeavesFileUnchanged(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	first := Event{TS: reconcileNow, Class: KindEscalation, Severity: SeverityHigh, Text: "one", Key: "esc:hq-1", State: EventNew}
	if err := AppendEvents(town, []Event{first}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	before, err := os.ReadFile(EventsPath(town))
	if err != nil {
		t.Fatal(err)
	}

	batch := []Event{
		{TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow, Text: "a", Key: "k1", State: EventNew},
		{TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow, Text: "b", Key: "k2", State: EventNew},
	}
	if err := appendEvents(town, batch, halfWrite); err == nil {
		t.Fatal("AppendEvents succeeded, want the write error")
	}
	after, err := os.ReadFile(EventsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("events.jsonl changed by a failed batch:\nbefore %q\nafter  %q", before, after)
	}
}

func TestStoreAppendEventsRetryWritesEachEventOnce(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	batch := []Event{
		{TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow, Text: "a", Key: "k1", State: EventNew},
		{TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow, Text: "b", Key: "k2", State: EventNew},
		{TS: reconcileNow, Class: KindRedMain, Severity: SeverityLow, Text: "c", Key: "k3", State: EventNew},
	}

	if err := appendEvents(town, batch, halfWrite); err == nil {
		t.Fatal("AppendEvents succeeded, want the write error")
	}

	if err := AppendEvents(town, batch); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, err := ReadEvents(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(batch) {
		t.Fatalf("events = %d after a failed batch and a retry, want %d: %+v", len(got), len(batch), got)
	}
	for i, e := range got {
		if e.Key != batch[i].Key {
			t.Errorf("event %d = %s, want %s", i, e.Key, batch[i].Key)
		}
	}
}

// ParseEventLine is the one decoder of an events.jsonl line: a blank or broken
// line is not an event.
func TestParseEventLine(t *testing.T) {
	t.Parallel()
	e, ok := ParseEventLine(`  {"ts":"2026-10-14T10:00:00Z","class":"red-main","severity":"high","text":"t","key":"k","state":"new","rule":"main.red","handled":"operator_action","wake":true}` + "\n")
	if !ok || e.Key != "k" || e.Rule != RuleRedMain || !e.Wake || e.State != EventNew {
		t.Fatalf("ParseEventLine = %+v, %v", e, ok)
	}
	for _, bad := range []string{"", "   ", `{"key":`, "not json"} {
		if _, ok := ParseEventLine(bad); ok {
			t.Errorf("ParseEventLine(%q) parsed", bad)
		}
	}
}

// ReadRetainedEvents reads the rotated log and then the current one, so the
// line that raised a condition survives a rotation; a town with neither file
// reads as no events.
func TestReadRetainedEventsSpansTheRotation(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if evs, err := ReadRetainedEvents(town); err != nil || len(evs) != 0 {
		t.Fatalf("no files -> %v, %v", evs, err)
	}
	if err := AppendEvents(town, []Event{{Key: "old", State: EventNew}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(EventsPath(town), RotatedEventsPath(town)); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvents(town, []Event{{Key: "new", State: EventNew}}); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadRetainedEvents(town)
	if err != nil || len(evs) != 2 || evs[0].Key != "old" || evs[1].Key != "new" {
		t.Fatalf("ReadRetainedEvents = %+v, %v, want old then new", evs, err)
	}
}
