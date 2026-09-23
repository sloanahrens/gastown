package slot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/lock"
)

const testMarkerName = "om-review-gt-mr-1"

// TestMarker_RefusesASecondLiveHolder: the refusal is what stops two reviews of
// one diff from both running and billing the backend, so it has to name the
// holder it refused against, and it has to be scoped to the one name — the
// batch's other members hold their own markers at the same time.
func TestMarker_RefusesASecondLiveHolder(t *testing.T) {
	town := t.TempDir()

	first, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}

	_, err = AcquireMarker(town, testMarkerName, "gastown/om-review")
	var held *MarkerHeldError
	if !errors.As(err, &held) {
		t.Fatalf("second AcquireMarker on a held name: err = %v, want *MarkerHeldError", err)
	}
	if held.Name != testMarkerName {
		t.Errorf("refusal names marker %q, want %q", held.Name, testMarkerName)
	}
	if held.Owner == nil || held.Owner.Role != "gastown/om-review" || held.Owner.PID != os.Getpid() {
		t.Errorf("refusal should name the holder, got %+v", held.Owner)
	}
	if !strings.Contains(held.Error(), "gastown/om-review") {
		t.Errorf("refusal message should name the holder's role: %q", held.Error())
	}

	other, err := AcquireMarker(town, "om-review-gt-mr-2", "gastown/om-review")
	if err != nil {
		t.Fatalf("a second review's own marker must not be refused: %v", err)
	}
	defer func() { _ = other.Release() }()

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	reacquired, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := reacquired.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestMarker_ReleaseRemovesTheOwnerFileAndIsIdempotent: the file is what a
// reader displays, so releasing has to take it with the lock, and a second
// Release (defer plus an explicit call on a failure path) must not error.
func TestMarker_ReleaseRemovesTheOwnerFileAndIsIdempotent(t *testing.T) {
	town := t.TempDir()

	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
	if _, err := os.Stat(MarkerOwnerPath(town, testMarkerName)); err != nil {
		t.Fatalf("a held marker should have an owner file: %v", err)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if _, err := os.Stat(MarkerOwnerPath(town, testMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("owner file should be gone after Release, stat err=%v", err)
	}
	if got := liveMarkers(town); len(got) != 0 {
		t.Fatalf("a released marker should not be reported: %+v", got)
	}
}

// TestMarker_DeadHoldersFileIsNotAHolder is the recovery test: a killed holder
// leaves its owner file behind with no lock (the kernel dropped that), and
// nothing may read it as a live review — the next review of the same diff has
// to be able to take the marker.
func TestMarker_DeadHoldersFileIsNotAHolder(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 1}

	// What kill -9 leaves: owner metadata written by a process that is gone,
	// and a lock file the kernel has already released.
	if err := atomicfile.EnsureDirAndWriteJSON(MarkerOwnerPath(town, testMarkerName), Owner{
		Role:       "gastown/om-review",
		PID:        999999,
		AcquiredAt: time.Now().Add(-time.Hour),
		Name:       testMarkerName,
	}); err != nil {
		t.Fatalf("write dead holder's owner file: %v", err)
	}

	rep, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if rep.HeldCount != 0 || rep.Held {
		t.Fatalf("a marker with no lock is nobody's, got %+v", rep)
	}
	if got := markerRows(rep); len(got) != 0 {
		t.Fatalf("a dead holder's file reported as a live review: %+v", got)
	}

	// Recovery: the next review of this diff takes the marker, and the file it
	// leaves describes that review rather than the dead one.
	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker should recover a dead holder's marker: %v", err)
	}
	defer func() { _ = h.Release() }()

	rep, err = StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	rows := markerRows(rep)
	if len(rows) != 1 {
		t.Fatalf("want one live marker row, got %+v", rows)
	}
	if rows[0].Owner == nil || rows[0].Owner.PID != os.Getpid() {
		t.Fatalf("marker row should describe the live holder, got %+v", rows[0].Owner)
	}
}

// TestMarker_StatusReportsItOutsideThePoolCount: a review holds no pool slot,
// so it must appear in the report — with the age its readers want — while
// leaving the pool's own count, saturation and busy verdict untouched, and
// leaving the slot itself free for a real suite.
func TestMarker_StatusReportsItOutsideThePoolCount(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 1}

	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
	defer func() { _ = h.Release() }()

	rep, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if rep.Total != 1 || rep.HeldCount != 0 || rep.Held || rep.AllHeld() || rep.Busy() {
		t.Fatalf("a marker must not count as the pool: %+v", rep)
	}
	if len(rep.Slots) != 2 {
		t.Fatalf("want the pool's slot plus the marker row, got %+v", rep.Slots)
	}
	if rep.Slots[0].Marker || rep.Slots[0].Index != 0 {
		t.Fatalf("slot 0 should still be a free pool slot: %+v", rep.Slots[0])
	}

	rows := markerRows(rep)
	if len(rows) != 1 {
		t.Fatalf("want one marker row, got %+v", rows)
	}
	m := rows[0]
	if m.Index == 0 {
		t.Errorf("marker row reused slot 0's index: %+v", m)
	}
	if m.Name != testMarkerName {
		t.Errorf("marker row name = %q, want %q", m.Name, testMarkerName)
	}
	if m.Owner == nil || m.Owner.Role != "gastown/om-review" || m.Owner.AcquiredAt.IsZero() {
		t.Fatalf("marker row carries no owner metadata to age: %+v", m.Owner)
	}
	if got := rep.HeldBy("gastown/om-review"); len(got) != 1 || !got[0].Marker {
		t.Fatalf("HeldBy should find the marker under its role: %+v", got)
	}

	// Outside the pool count means outside its admission too: the free slot is
	// still there for a container-backed suite to take.
	h2, err := AcquirePool(town, "gastown/polecat-7", shortWait, pool)
	if err != nil {
		t.Fatalf("AcquirePool beside a held marker: %v", err)
	}
	if h2.Index != 0 {
		t.Fatalf("suite took slot %d, want the free slot 0", h2.Index)
	}
	if err := h2.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestMarker_DoesNotStandInForTheContainerCheck: a marker says a review is
// running, not that the Docker VM is in use. The report must still look for an
// unwrapped suite, or a review in flight would hide one.
func TestMarker_DoesNotStandInForTheContainerCheck(t *testing.T) {
	town := t.TempDir()
	pool := Pool{Slots: 1}

	orig := runningGateContainers
	defer func() { runningGateContainers = orig }()
	runningGateContainers = func() ([]string, error) {
		return []string{dockerPSLine("live-id", "dolt/dolt-sql-server:2.2.0", "running-suite",
			time.Now().Add(-2*time.Minute), nil)}, nil
	}

	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
	defer func() { _ = h.Release() }()

	rep, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if len(rep.UnwrappedContainers) != 1 {
		t.Fatalf("the container cross-check did not run with a marker held: %+v", rep)
	}
	if !rep.Busy() {
		t.Fatal("an unwrapped suite must read busy even while a review is in flight")
	}
}

// TestMarker_KeepsItsNameInsideTheLockDirectory: the name reaches the lock
// directory's filenames, so a name carrying a path separator must not address a
// file outside it — and the report shows the name the caller spelled.
func TestMarker_KeepsItsNameInsideTheLockDirectory(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	name := "../../escape/om review"

	h, err := AcquireMarker(town, name, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
	defer func() { _ = h.Release() }()

	for _, path := range []string{MarkerLockPath(town, name), MarkerOwnerPath(town, name)} {
		if got := filepath.Dir(path); got != LockDir(town) {
			t.Errorf("marker file %q escaped the lock directory into %q", path, got)
		}
	}

	rep, err := StatusPool(town, Pool{Slots: 1})
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	rows := markerRows(rep)
	if len(rows) != 1 || rows[0].Name != name {
		t.Fatalf("marker row should report the caller's name %q: %+v", name, rows)
	}
}

// TestMarker_PoolSlotFilesAreNotMarkers: both kinds of lock live in one
// directory, so the marker sweep has to leave the pool's own slot and owner
// files alone (and the reports of them).
func TestMarker_PoolSlotFilesAreNotMarkers(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	h, err := AcquirePool(town, "gastown/refinery", shortWait, pool)
	if err != nil {
		t.Fatalf("AcquirePool: %v", err)
	}
	defer func() { _ = h.Release() }()

	rep, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if len(rep.Slots) != 2 {
		t.Fatalf("pool slot files must not be swept up as markers: %+v", rep.Slots)
	}
	if rep.HeldCount != 1 || rep.Total != 2 {
		t.Fatalf("the pool's own count changed: %+v", rep)
	}
}

// TestMarker_AcquireRidesOutTransientFlockContention: liveMarkers and
// StatusPoolLocksOnly test a marker by taking its flock and releasing it with
// no work in between — microseconds. A real reviewer's AcquireMarker landing
// in that window must not be refused as though another review held it
// (gt-97cm finding 1).
func TestMarker_AcquireRidesOutTransientFlockContention(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	unlock, ok, err := lock.FlockTryAcquire(MarkerLockPath(town, testMarkerName))
	if err != nil || !ok {
		t.Fatalf("simulated status probe could not take the flock: ok=%v err=%v", ok, err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		unlock()
	}()

	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker should ride out a transient probe hold: %v", err)
	}
	defer func() { _ = h.Release() }()
}

// TestMarker_ReleaseRemovesTheLockFile: Release must remove the lock file
// itself, not only the owner file, or one file per ever-reviewed MR piles up
// forever and every status read re-tests it (gt-97cm finding 3).
func TestMarker_ReleaseRemovesTheLockFile(t *testing.T) {
	town := t.TempDir()
	h, err := AcquireMarker(town, testMarkerName, "gastown/om-review")
	if err != nil {
		t.Fatalf("AcquireMarker: %v", err)
	}
	lockPath := MarkerLockPath(town, testMarkerName)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("a held marker should have a lock file: %v", err)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should be gone after Release, stat err=%v", err)
	}
}

// TestMarker_LiveMarkersSweepsAFreeOrphanedLockFile: a lock file nobody holds
// must not sit on disk forever costing every future status read a flock probe
// (gt-97cm finding 3) — this is the recovery path for a marker released by a
// killed process, which never runs MarkerHandle.Release at all.
func TestMarker_LiveMarkersSweepsAFreeOrphanedLockFile(t *testing.T) {
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lockPath := MarkerLockPath(town, testMarkerName)
	if err := os.WriteFile(lockPath, nil, 0644); err != nil {
		t.Fatalf("seeding an orphaned lock file: %v", err)
	}

	if got := liveMarkers(town); len(got) != 0 {
		t.Fatalf("an orphaned free lock names no holder: %+v", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("liveMarkers should have swept the free lock file, stat err=%v", err)
	}
}

// TestMarkerSlug_LongNameRoundTripsThroughMarkerLockPath: cutting a slug to
// length before trimming trailing dots keeps it idempotent — otherwise the
// cut can leave a trailing '.' that a second pass trims away, and liveMarkers
// feeding a name it read off the directory back into MarkerLockPath would
// then address a different file than the live holder's (gt-97cm finding 6).
func TestMarkerSlug_LongNameRoundTripsThroughMarkerLockPath(t *testing.T) {
	name := strings.Repeat("a", 99) + "." + strings.Repeat("b", 20)
	slug := markerSlug(name)
	if len(slug) > 100 {
		t.Fatalf("slug %q is %d chars, want at most 100", slug, len(slug))
	}
	if strings.HasSuffix(slug, ".") {
		t.Fatalf("slug %q ends in '.', which markerSlug would trim on a second pass", slug)
	}
	if again := markerSlug(slug); again != slug {
		t.Fatalf("markerSlug is not idempotent: markerSlug(%q) = %q, want %q", slug, again, slug)
	}
}

// markerRows returns the report's in-flight marker rows.
func markerRows(rep Report) []SlotState {
	var out []SlotState
	for _, st := range rep.Slots {
		if st.Marker {
			out = append(out, st)
		}
	}
	return out
}
