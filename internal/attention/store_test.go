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
