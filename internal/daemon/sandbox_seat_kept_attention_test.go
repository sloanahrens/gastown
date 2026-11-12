package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// keptSeatAttentionNow is the tick every collection in these tests judges
// against, so no test reads the wall clock.
var keptSeatAttentionNow = time.Date(2026, 11, 12, 22, 0, 0, 0, time.UTC)

// keptSeatSince writes a kept record for gastown/name whose keep began age ago.
func keptSeatSince(t *testing.T, town, name, bead string, age time.Duration) {
	t.Helper()
	rec := keptSandboxSeatRecord{Bead: bead, Reason: keptSeatReason, Since: keptSeatAttentionNow.Add(-age)}
	if err := writeKeptSandboxSeat(town, attentionRig, name, rec); err != nil {
		t.Fatalf("writing the kept record for %s: %v", name, err)
	}
}

// A seat kept more than thirty minutes raises one high item that names the seat,
// its bead and the age, and carries the daemon's reason verbatim. A seat kept
// for just under thirty minutes raises nothing.
func TestKeptSeatAttentionRaisesAfterTheWindow(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "capable", "gt-abc12", 31*time.Minute)
	keptSeatSince(t, f.d.config.TownRoot, "nux", "gt-abc13", 29*time.Minute)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one (the 29m seat raises none)", items)
	}
	it := items[0]
	if it.Key != "sandbox-seat-kept:gastown/capable" || it.Kind != attention.KindSandboxSeatKept ||
		it.Severity != attention.SeverityHigh || it.Rig != attentionRig || it.Bead != "gt-abc12" {
		t.Errorf("item = %+v, want a high sandbox-seat-kept item on gastown/capable and gt-abc12", it)
	}
	want := "gastown/capable kept 31m on gt-abc12: " + keptSeatReason
	if it.Summary != want {
		t.Errorf("summary = %q, want %q", it.Summary, want)
	}
}

// The item's key is the seat, so two ticks over the same record give one item
// with the same key, and removing the record clears it.
func TestKeptSeatAttentionKeyIsTheSeat(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	town := f.d.config.TownRoot
	keptSeatSince(t, town, "capable", "gt-abc12", 3*time.Hour)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	first, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].Key != second[0].Key {
		t.Fatalf("ticks = %+v then %+v, want one item with the same key", first, second)
	}

	if err := removeKeptSandboxSeat(town, attentionRig, "capable"); err != nil {
		t.Fatalf("removing the kept record: %v", err)
	}
	after, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("items after the record is gone = %+v, want none", after)
	}
}

// A record the reader cannot decode fails the collection: it returns the error
// and no items, so the previous tick's items are kept rather than cleared.
func TestKeptSeatAttentionFailsOnAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "nux", "gt-abc13", 3*time.Hour)
	path, err := keptSandboxSeatPath(f.d.config.TownRoot, attentionRig, "capable")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err == nil {
		t.Fatalf("collector read a malformed record as clean: items = %+v", items)
	}
	if items != nil {
		t.Errorf("items = %+v beside an unreadable record, want none", items)
	}
	if !strings.Contains(err.Error(), "capable.json") {
		t.Errorf("error %q does not name the unreadable record", err)
	}
}

// A store that cannot be listed fails the collection with the listing error and
// no items, so the previous tick's items stand rather than clearing as an empty
// queue.
func TestKeptSeatAttentionFailsOnAStoreThatCannotBeListed(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	dir := keptSandboxSeatsDir(f.d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the store's directory should be: a listing of it fails.
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collector err = %v, want the listing error", err)
	}
	if items != nil {
		t.Errorf("items = %+v beside an unlistable store, want none", items)
	}
}

// A kept seat with no bead recorded says so in its summary, and its item has no
// bead.
func TestKeptSeatAttentionNamesAnUnrecordedBead(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	keptSeatSince(t, f.d.config.TownRoot, "capable", "", 3*time.Hour)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	if items[0].Bead != "" || !strings.Contains(items[0].Summary, "on no recorded bead: ") {
		t.Errorf("item = %+v, want no bead and the summary to say no recorded bead", items[0])
	}
}

// The collector is in the queue's set and owns its own items: a failed
// collection keeps the previous tick's kept-seat item and no other kind's.
func TestKeptSeatAttentionIsInTheCollectorSet(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, keptSeatAttentionNow)
	c := collectorFor(t, f.src, attention.KindSandboxSeatKept)
	kept := attention.Item{Key: "sandbox-seat-kept:gastown/capable", Kind: attention.KindSandboxSeatKept, Severity: attention.SeverityHigh}
	other := attention.Item{Key: "config-parse-failed", Kind: attention.KindConfigParseFailed, Severity: attention.SeverityHigh}
	prev := attention.State{Items: []attention.Item{kept, other}}

	got := c.kept(prev)
	if len(got) != 1 || got[0].Key != kept.Key {
		t.Errorf("kept(prev) = %+v, want only the kept-seat item", got)
	}
}
