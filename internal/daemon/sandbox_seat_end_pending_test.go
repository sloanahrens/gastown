package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The exported reader hands the dashboard each kept seat's rig, name and
// submitted time, reports a record it cannot read by path, and reads a town
// with no store as no seats (gt-ddg96x).
func TestPendingSandboxSeatEndsReturnsEachWrittenRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	submitted := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	if err := writePendingSandboxSeatEnd(town, "gastown", "cheedo", sandboxSeatLanding{BeadID: "gt-1", Submitted: submitted}); err != nil {
		t.Fatalf("writing the record: %v", err)
	}

	seats, bad, err := PendingSandboxSeatEnds(town)
	if err != nil {
		t.Fatalf("reading the pending ends: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("unreadable records = %v, want none", bad)
	}
	if len(seats) != 1 {
		t.Fatalf("seats = %+v, want one", seats)
	}
	if seats[0].Rig != "gastown" || seats[0].Name != "cheedo" || !seats[0].Submitted.Equal(submitted) {
		t.Errorf("seat = %+v, want gastown/cheedo submitted %s", seats[0], submitted)
	}
}

// A record that cannot be decoded is reported by its path and is not a seat:
// the caller must see that a seat it cannot read exists (gt-ddg96x).
func TestPendingSandboxSeatEndsReportsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path, err := pendingSandboxSeatEndPath(town, "gastown", "cheedo")
	if err != nil {
		t.Fatalf("naming the record: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	seats, bad, err := PendingSandboxSeatEnds(town)
	if err != nil {
		t.Fatalf("reading the pending ends: %v", err)
	}
	if len(seats) != 0 {
		t.Errorf("seats = %+v, want none: the record cannot be read", seats)
	}
	if len(bad) != 1 || bad[0] != path {
		t.Errorf("unreadable records = %v, want just %s", bad, path)
	}
}

// A town that has never kept a seat has no store, which is no seats and no
// error, not a failed read.
func TestPendingSandboxSeatEndsIsEmptyWithoutAStore(t *testing.T) {
	t.Parallel()
	seats, bad, err := PendingSandboxSeatEnds(t.TempDir())
	if err != nil {
		t.Fatalf("a town with no store read as an error: %v", err)
	}
	if len(seats) != 0 || len(bad) != 0 {
		t.Errorf("seats = %+v, bad = %v, want neither", seats, bad)
	}
}

// A store that cannot be listed is an error, so the caller decides nothing on
// seats it could not read.
func TestPendingSandboxSeatEndsFailsOnAStoreThatCannotBeListed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := pendingSandboxSeatEndsDir(town)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PendingSandboxSeatEnds(town); err == nil {
		t.Error("a store that is a file was read as a town with no kept seats")
	}
}
