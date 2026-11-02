package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sling"
)

// dockedBeads answers the rig identity bead reads a docked check makes, and
// counts them, so a test can hold a tick to the reads that reached the store.
// An id in docked answers with the docked label; every other id is ErrNotFound.
type dockedBeads struct {
	beads.Client
	fail   bool
	docked map[string]bool
	shows  map[string]int
}

func newDockedBeads(fail bool) *dockedBeads {
	return &dockedBeads{fail: fail, shows: map[string]int{}}
}

func (d *dockedBeads) Show(id string) (*beads.Issue, error) {
	d.shows[id]++
	if d.fail {
		return nil, errors.New("dolt down")
	}
	if d.docked[id] {
		return &beads.Issue{Labels: []string{RigDockedLabel}}, nil
	}
	return nil, beads.ErrNotFound
}

// tickDockedTown is a town whose two rigs are registered and whose directories
// exist, so a check for either rig reaches its identity-bead read.
func tickDockedTown(t *testing.T) string {
	t.Helper()
	town := parkTown(t)
	for _, name := range []string{"testrig", "otherrig"} {
		if err := os.MkdirAll(filepath.Join(town, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return town
}

// TestTickReadsRigDockedStateOncePerRig: the tick checks each rig's docked
// state during its board scan, then checks the same rigs again behind the
// slings, and the memo serves the scan's read to those later checks, so the
// identity bead is read once per rig (gt-ze7z5). Mutation: drop the memo and
// every check reads again.
func TestTickReadsRigDockedStateOncePerRig(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)
	board := func(string) ([]*beads.Issue, error) { return nil, nil }

	// The board scan checks each rig.
	specCandidatesRetrying(town, 2, board, docked.parkedOrDocked, nil)
	// These stand in for the repeated checks a sling makes after the scan (the
	// sling's own guard and its spawn's guard): each must be answered by the
	// scan's read, not by another one.
	rigs := []string{"testrig", "otherrig"}
	for i := 0; i < 2; i++ {
		for _, name := range rigs {
			if blocked, reason := docked.parkedOrDocked(town, name); blocked || reason != "" {
				t.Errorf("docked.parkedOrDocked(%s) = %v, %q; want not blocked", name, blocked, reason)
			}
		}
	}

	want := map[string]int{"tr-rig-testrig": 1, "or-rig-otherrig": 1}
	for id, n := range want {
		if got := store.shows[id]; got != n {
			t.Errorf("identity bead %s read %d time(s) in one tick, want %d", id, got, n)
		}
	}
	for id, n := range store.shows {
		if _, known := want[id]; !known {
			t.Errorf("unexpected read of %s (%d times)", id, n)
		}
	}
}

// TestTickDockedMemoIsSafeUnderConcurrentChecks: the tick slings in parallel,
// and every sling checks the same rig's docked state. The memo answers them
// all from the one read: no second read, and no unsynchronized map access.
func TestTickDockedMemoIsSafeUnderConcurrentChecks(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if blocked, reason := docked.parkedOrDocked(town, "testrig"); blocked || reason != "" {
				t.Errorf("concurrent check = %v, %q; want not blocked", blocked, reason)
			}
		}()
	}
	wg.Wait()

	if got := store.shows["tr-rig-testrig"]; got != 1 {
		t.Errorf("identity bead read %d time(s) by %d concurrent checks, want 1", got, n)
	}
}

// TestSpecSlingSpawnCarriesTheTicksDockedMemo: the spawn behind a tick's sling
// answers the rig guard from the tick's memo, not a read of its own
// (gt-ze7z5). Mutation: drop the assignment and the spawn asks bd again.
func TestSpecSlingSpawnCarriesTheTicksDockedMemo(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)
	var got SlingSpawnOptions
	spawn := func(_ string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		got = opts
		return &SpawnedPolecatInfo{}, nil
	}

	if _, err := specSlingSpawn(spawn, nil, nil, docked)("testrig", SlingSpawnOptions{}); err != nil {
		t.Fatalf("spawn error = %v", err)
	}
	if got.rigParked == nil {
		t.Fatal("the spawn carries no docked check, so its guard would read the rig's identity bead again")
	}
	for i := 0; i < 2; i++ {
		if blocked, reason := got.rigParked(town, "testrig"); blocked || reason != "" {
			t.Errorf("spawn's docked check = %v, %q; want not blocked", blocked, reason)
		}
	}
	if n := store.shows["tr-rig-testrig"]; n != 1 {
		t.Errorf("the spawn's checks read the identity bead %d time(s), want the tick's 1", n)
	}
}

// TestTickDockedReadFailureStaysBlocked: a docked read that fails blocks the
// rig as unverified, and the failure is not remembered as an answer: the next
// check in the same tick asks the store again, so a bd that recovers inside
// the tick is seen (gt-ze7z5).
func TestTickDockedReadFailureStaysBlocked(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(true)
	docked := newRigDockedMemo(store)

	for i := 1; i <= 2; i++ {
		blocked, reason := docked.parkedOrDocked(town, "testrig")
		if !blocked || reason != sling.RigStatusUnverified {
			t.Fatalf("check %d = %v, %q; want blocked as %q", i, blocked, reason, sling.RigStatusUnverified)
		}
		if got := store.shows["tr-rig-testrig"]; got != i {
			t.Errorf("after check %d the store was read %d time(s), want %d: a failed read must not be cached", i, got, i)
		}
	}
}

// TestTickDockedMemoSeesADockAfterTheFirstRead: gt rig dock sets the label and
// then stamps the rig (rig.DockStamp), and the memo drops a verdict older than
// the stamp, so a dock that lands mid-tick stops the tick's later slings
// (gt-ze7z5). Mutation: drop the stamp comparison and the second check serves
// the pre-dock answer from the memo.
func TestTickDockedMemoSeesADockAfterTheFirstRead(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)

	if blocked, reason := docked.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Fatalf("before the dock = %v, %q; want not blocked", blocked, reason)
	}

	// What gt rig dock does, in order: the label, then the stamp.
	store.docked = map[string]bool{"tr-rig-testrig": true}
	if err := rig.TouchDockStamp(town, "testrig", time.Now()); err != nil {
		t.Fatalf("touching the dock stamp: %v", err)
	}

	blocked, reason := docked.parkedOrDocked(town, "testrig")
	if !blocked || reason != "docked" {
		t.Errorf("after the dock = %v, %q; want blocked as %q", blocked, reason, "docked")
	}
	if n := store.shows["tr-rig-testrig"]; n != 2 {
		t.Errorf("identity bead read %d time(s), want 2: a newer dock stamp must drop the memo", n)
	}
}

// TestTickDockedMemoDoesNotEmptyOnAnUnreadableStamp: a dock stamp that cannot
// be read vouches for no cached verdict, so the check reads the label fresh
// instead of serving an answer from before a possible dock (gt-ze7z5).
// Mutation: serve the cached entry when the stamp read fails, and the store is
// read once rather than twice.
func TestTickDockedMemoDoesNotEmptyOnAnUnreadableStamp(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(false)
	docked := newRigDockedMemo(store)
	if blocked, reason := docked.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Fatalf("first check = %v, %q; want not blocked", blocked, reason)
	}

	docked.stamp = func(string, string) (time.Time, error) {
		return time.Time{}, errors.New("dock stamp unreadable")
	}
	store.docked = map[string]bool{"tr-rig-testrig": true}

	blocked, reason := docked.parkedOrDocked(town, "testrig")
	if !blocked || reason != "docked" {
		t.Errorf("check under an unreadable stamp = %v, %q; want the label read fresh as %q", blocked, reason, "docked")
	}
	if n := store.shows["tr-rig-testrig"]; n != 2 {
		t.Errorf("identity bead read %d time(s), want 2: an unreadable stamp must not serve the memo", n)
	}
}

// blockingDockedBeads holds testrig's identity-bead read open until the test
// releases it, so a check of another rig can be timed against it.
type blockingDockedBeads struct {
	beads.Client
	started chan struct{}
	release chan struct{}
}

func (b *blockingDockedBeads) Show(id string) (*beads.Issue, error) {
	if id == "tr-rig-testrig" {
		close(b.started)
		<-b.release
	}
	return nil, beads.ErrNotFound
}

// TestTickDockedMemoDoesNotHoldUpOtherRigs: the memo's lock covers one rig's
// entry, so a bd read held open for testrig does not stall otherrig's check
// (gt-ze7z5). Mutation: hold one lock across the read and otherrig waits for
// testrig's store call to return.
func TestTickDockedMemoDoesNotHoldUpOtherRigs(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := &blockingDockedBeads{started: make(chan struct{}), release: make(chan struct{})}
	docked := newRigDockedMemo(store)

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(store.release) }) }
	defer release()

	testrigDone := make(chan struct{})
	go func() {
		defer close(testrigDone)
		docked.parkedOrDocked(town, "testrig")
	}()
	select {
	case <-store.started:
	//testpolicy:allow no-sleep — hang guard only: a check that never reaches the store has no event to select on, and this fails it rather than hanging the suite
	case <-time.After(5 * time.Second):
		release()
		<-testrigDone
		t.Fatal("testrig's read never reached the store")
	}

	otherrigDone := make(chan struct{})
	go func() {
		defer close(otherrigDone)
		docked.parkedOrDocked(town, "otherrig")
	}()
	select {
	case <-otherrigDone:
	//testpolicy:allow no-sleep — hang guard only: a serialised check has no event to select on, and this fails it rather than hanging the suite
	case <-time.After(5 * time.Second):
		t.Error("otherrig's check waited on testrig's read: the memo holds a lock across the bd call")
	}

	release()
	<-testrigDone
	<-otherrigDone
}

// writeRigDockedRecord puts rec in testrig's verdict file.
func writeRigDockedRecord(t *testing.T, town string, rec rigDockedRecord) {
	t.Helper()
	path, err := rigDockedFilePath(town, "testrig")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeRigDockedFileAt answers one not-blocked check for testrig at at, so the
// verdict file a later tick reads exists.
func writeRigDockedFileAt(t *testing.T, town string, at time.Time) {
	t.Helper()
	memo := newRigDockedMemo(newDockedBeads(false))
	memo.now = func() time.Time { return at }
	if blocked, reason := memo.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Fatalf("seed check = %v, %q; want not blocked", blocked, reason)
	}
}

// TestRigDockedFileServesAcrossTicks: a tick leaves its docked verdict in a
// per-rig file under the runtime directory, and the next tick (a new process,
// a new memo) serves it while the dock stamp is unchanged and the read is
// younger than five minutes, with no identity-bead read at all (gt-ejxjh).
// Mutation: drop the file load and the second tick's failing store is read.
func TestRigDockedFileServesAcrossTicks(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	writeRigDockedFileAt(t, town, now)

	// The store cannot answer, so a verdict that did not come from the file
	// would be blocked as unverified.
	store := newDockedBeads(true)
	memo := newRigDockedMemo(store)
	memo.now = func() time.Time { return now.Add(4 * time.Minute) }

	blocked, reason := memo.parkedOrDocked(town, "testrig")
	if blocked || reason != "" {
		t.Errorf("second tick = %v, %q; want the file's not-blocked verdict", blocked, reason)
	}
	if n := store.shows["tr-rig-testrig"]; n != 0 {
		t.Errorf("second tick read the identity bead %d time(s), want 0: the verdict file answers", n)
	}
}

// TestRigDockedFileReadsParkStateEveryCheck: the verdict file holds only the
// docked label, and every check reads the park state from the registry first.
// A rig parked after a not-blocked file was written is blocked as parked, and
// once unparked the same file answers again (gt-ejxjh). Mutation: load and
// serve the file before the park check, and the parked check returns the
// file's stale not-blocked verdict.
func TestRigDockedFileReadsParkStateEveryCheck(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	writeRigDockedFileAt(t, town, now)

	// The store cannot answer, so a check that reads bd is blocked as
	// unverified: an answer proves the file served it.
	store := newDockedBeads(true)
	memo := newRigDockedMemo(store)
	memo.now = func() time.Time { return now.Add(time.Minute) }

	var out bytes.Buffer
	rec := config.RigParked{Since: now.Add(30 * time.Second), By: "test", Reason: "rework"}
	if err := parkRigs(&out, town, []string{"testrig"}, rec); err != nil {
		t.Fatalf("parkRigs = %v\n%s", err, out.String())
	}
	if blocked, reason := memo.parkedOrDocked(town, "testrig"); !blocked || reason != "parked" {
		t.Errorf("check after park = %v, %q; want blocked as %q despite the fresh not-blocked file", blocked, reason, "parked")
	}
	if n := store.shows["tr-rig-testrig"]; n != 0 {
		t.Errorf("the parked check read the identity bead %d time(s), want 0: park is the registry's", n)
	}

	journal := func(string) (bool, error) { return false, nil }
	if err := unparkRigs(&out, town, []string{"testrig"}, journal); err != nil {
		t.Fatalf("unparkRigs = %v\n%s", err, out.String())
	}
	if blocked, reason := memo.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Errorf("check after unpark = %v, %q; want the file's not-blocked verdict", blocked, reason)
	}
	if n := store.shows["tr-rig-testrig"]; n != 0 {
		t.Errorf("the unparked check read the identity bead %d time(s), want 0: the file still answers", n)
	}
}

// TestRigDockedFileDoesNotStoreAParkVerdict: the file records only the docked
// label. A park that lands between a check's registry read and its label read
// is not stored, so once the rig is unparked a later tick is not blocked by a
// stale "parked" verdict (gt-ejxjh). The dock-stamp read runs between the two,
// so parking there reproduces the race. Mutation: read the park state again
// behind the label read and store that answer, and the fresh tick after the
// unpark serves "parked" from the file.
func TestRigDockedFileDoesNotStoreAParkVerdict(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	rec := config.RigParked{Since: now, By: "test", Reason: "rework"}

	store := newDockedBeads(false)
	memo := newRigDockedMemo(store)
	memo.now = func() time.Time { return now }
	var out bytes.Buffer
	var parkErr error
	memo.stamp = func(string, string) (time.Time, error) {
		parkErr = parkRigs(&out, town, []string{"testrig"}, rec)
		return time.Time{}, nil
	}

	if blocked, reason := memo.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Fatalf("check under the park race = %v, %q; want the docked-label read's not-blocked verdict", blocked, reason)
	}
	if parkErr != nil {
		t.Fatalf("parking inside the race = %v\n%s", parkErr, out.String())
	}

	journal := func(string) (bool, error) { return false, nil }
	if err := unparkRigs(&out, town, []string{"testrig"}, journal); err != nil {
		t.Fatalf("unparkRigs = %v\n%s", err, out.String())
	}

	// A new tick: the park race's record is all the file holds.
	next := newRigDockedMemo(newDockedBeads(true))
	next.now = func() time.Time { return now.Add(time.Minute) }
	if blocked, reason := next.parkedOrDocked(town, "testrig"); blocked {
		t.Errorf("tick after unpark = %v, %q; want the file's not-blocked verdict, not the race's park", blocked, reason)
	}
}

// TestRigDockedFileDropsOnStampOrAge: a record may not answer a check it cannot
// vouch for. A newer dock stamp, a record older than five minutes, a corrupt
// file, a file naming another rig and an unreadable file each send the check to
// bd, which is what the store read count holds it to (gt-ejxjh).
func TestRigDockedFileDropsOnStampOrAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		break_ func(t *testing.T, town string, memo *rigDockedMemo)
	}{
		{
			name: "a newer dock stamp",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				t.Helper()
				if err := rig.TouchDockStamp(town, "testrig", now.Add(time.Minute)); err != nil {
					t.Fatalf("touching the dock stamp: %v", err)
				}
			},
		},
		{
			name: "a record older than five minutes",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				memo.now = func() time.Time { return now.Add(rigDockedFileTTL + time.Second) }
			},
		},
		{
			name: "a corrupt file",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				t.Helper()
				path, err := rigDockedFilePath(town, "testrig")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a file naming another rig",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				writeRigDockedRecord(t, town, rigDockedRecord{Rig: "otherrig", Stamp: now, ReadAt: now})
			},
		},
		{
			name: "a record with no read time",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				writeRigDockedRecord(t, town, rigDockedRecord{Rig: "testrig", Stamp: now})
			},
		},
		{
			name: "a record whose read time has not arrived",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				// The check runs at now+1m, so this record is still "future".
				writeRigDockedRecord(t, town, rigDockedRecord{Rig: "testrig", Stamp: now, ReadAt: now.Add(2 * time.Minute)})
			},
		},
		{
			name: "an unreadable file",
			break_: func(t *testing.T, town string, memo *rigDockedMemo) {
				memo.files.readFile = func(string) ([]byte, error) {
					return nil, errors.New("permission denied")
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tickDockedTown(t)
			writeRigDockedFileAt(t, town, now)

			store := newDockedBeads(true)
			memo := newRigDockedMemo(store)
			memo.now = func() time.Time { return now.Add(time.Minute) }
			tc.break_(t, town, memo)

			blocked, reason := memo.parkedOrDocked(town, "testrig")
			if !blocked || reason != sling.RigStatusUnverified {
				t.Errorf("check = %v, %q; want bd read and %q", blocked, reason, sling.RigStatusUnverified)
			}
			if n := store.shows["tr-rig-testrig"]; n != 1 {
				t.Errorf("identity bead read %d time(s), want 1: the record cannot answer this check", n)
			}
		})
	}
}

// TestRigDockedFileNeverStoresUnverified: a read that failed is not an answer,
// so it leaves no verdict file, the next check in the tick reads the store
// again, and a later tick finds nothing to serve (gt-ejxjh).
func TestRigDockedFileNeverStoresUnverified(t *testing.T) {
	t.Parallel()
	town := tickDockedTown(t)
	store := newDockedBeads(true)
	memo := newRigDockedMemo(store)

	for i := 1; i <= 2; i++ {
		blocked, reason := memo.parkedOrDocked(town, "testrig")
		if !blocked || reason != sling.RigStatusUnverified {
			t.Fatalf("check %d = %v, %q; want blocked as %q", i, blocked, reason, sling.RigStatusUnverified)
		}
		if got := store.shows["tr-rig-testrig"]; got != i {
			t.Errorf("after check %d the store was read %d time(s), want %d", i, got, i)
		}
	}

	path, err := rigDockedFilePath(town, "testrig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an unverified read left a verdict file at %s", path)
	}

	next := newDockedBeads(false)
	nextMemo := newRigDockedMemo(next)
	if blocked, reason := nextMemo.parkedOrDocked(town, "testrig"); blocked || reason != "" {
		t.Errorf("next tick = %v, %q; want its own read's not-blocked verdict", blocked, reason)
	}
	if n := next.shows["tr-rig-testrig"]; n != 1 {
		t.Errorf("next tick read the identity bead %d time(s), want 1", n)
	}
}

// TestRigDockedFileWriteFailureStillAnswers: a verdict file that cannot be
// written is logged and the check still answers with its read; the next tick
// simply reads bd again (gt-ejxjh).
func TestRigDockedFileWriteFailureStillAnswers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		break_ func(t *testing.T, memo *rigDockedMemo)
	}{
		{
			name: "the directory cannot be created",
			break_: func(t *testing.T, memo *rigDockedMemo) {
				memo.files.mkdirAll = func(string, os.FileMode) error {
					return errors.New("permission denied")
				}
			},
		},
		{
			name: "the file cannot be written",
			break_: func(t *testing.T, memo *rigDockedMemo) {
				memo.files.writeFile = func(string, []byte, os.FileMode) error {
					return errors.New("disk full")
				}
			},
		},
		{
			name: "the record cannot be encoded",
			break_: func(t *testing.T, memo *rigDockedMemo) {
				// time.Time marshals no year outside 0-9999, and the clock is
				// what produces the record's read time.
				memo.now = func() time.Time { return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tickDockedTown(t)
			store := newDockedBeads(false)
			memo := newRigDockedMemo(store)
			tc.break_(t, memo)

			blocked, reason := memo.parkedOrDocked(town, "testrig")
			if blocked || reason != "" {
				t.Errorf("check = %v, %q; want the read's not-blocked verdict", blocked, reason)
			}
			if n := store.shows["tr-rig-testrig"]; n != 1 {
				t.Errorf("identity bead read %d time(s), want 1", n)
			}

			next := newDockedBeads(true)
			nextMemo := newRigDockedMemo(next)
			if blocked, _ := nextMemo.parkedOrDocked(town, "testrig"); !blocked {
				t.Error("the next tick was served a verdict a failed write should not have left")
			}
		})
	}
}

// TestRigDockedFileRefusesARigNameThatLeavesTheDirectory: a rig name that could
// leave the runtime directory gets no verdict file. The check still answers with
// its read, and the write creates nothing (gt-ejxjh). The stamp is replaced
// because rig.DockStamp refuses the same name, which would decide the check
// before the file is reached.
func TestRigDockedFileRefusesARigNameThatLeavesTheDirectory(t *testing.T) {
	t.Parallel()
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json": `{"type":"town","version":2,"name":"t"}`,
		"mayor/rigs.json": `{"version":1,"rigs":{"../evil":{"git_url":"x","added_at":"2026-09-07T00:00:00Z","beads":{"repo":"","prefix":"ev"}}}}`,
	})
	store := newDockedBeads(false)
	memo := newRigDockedMemo(store)
	memo.stamp = func(string, string) (time.Time, error) { return time.Time{}, nil }

	if blocked, reason := memo.parkedOrDocked(town, "../evil"); blocked || reason != "" {
		t.Errorf("check = %v, %q; want the read's not-blocked verdict", blocked, reason)
	}
	if _, err := os.Stat(constants.TownRuntimePath(town)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a refused verdict path created the runtime directory")
	}
}
