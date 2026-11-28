package gocacheprog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// evictAt is n minutes after the store's first put.
func evictAt(n int) time.Time { return evictNow.Add(-2*time.Hour + time.Duration(n)*time.Minute) }

func evictUse(action string, at time.Time) string {
	return `{"op":"use","action":"` + action + `","time":"` + at.Format(time.RFC3339) + `"}` + "\n"
}

// evictRunCache returns a store of 4-byte bodies aa, bb and cc, last used at minutes 0, 1 and 2 in an
// hour-old snapshot, with a dead journal using aa at minute 3 and an own journal using cc at minute 4.
func evictRunCache(t *testing.T) string {
	t.Helper()
	d, dir := newDisk(t)
	idx := Index{}
	for i, a := range []string{"aa", "bb", "cc"} {
		_, err := d.Put([]byte(a), []byte(a[:1]+"1"), []byte("body"))
		must(t, err)
		idx[a] = IndexEntry{OutputID: a[:1] + "1", Size: 4, PutTime: evictAt(0), LastUse: evictAt(i)}
	}
	index := filepath.Join(dir, "index.jsonl")
	must(t, WriteSnapshot(index, idx))
	must(t, os.Chtimes(index, evictNow.Add(-time.Hour), evictNow.Add(-time.Hour)))
	evictJournal(t, dir, "dead.log", evictAt(0), evictUse("aa", evictAt(3)))
	evictJournal(t, dir, "own.log", evictAt(0), evictUse("cc", evictAt(4)))
	return dir
}

// evictSnapshotAAandCC fails t unless dir's snapshot holds exactly aa and cc.
func evictSnapshotAAandCC(t *testing.T, dir string) {
	t.Helper()
	snap, err := ReadSnapshot(filepath.Join(dir, "index.jsonl"))
	must(t, err)
	if _, ok := snap["bb"]; ok || len(snap) != 2 {
		t.Errorf("snapshot %v, want aa and cc without bb", snap)
	}
}

// evictUsable fails t unless every entry of the store is a hit or a miss.
func evictUsable(t *testing.T, dir string) {
	t.Helper()
	d := &Disk{dir: dir}
	for _, a := range []string{"aa", "bb", "cc"} {
		if _, err := d.Get([]byte(a)); err != nil && !errors.Is(err, ErrMiss) {
			t.Errorf("Get %s after a failed eviction: %v, want a hit or a miss", a, err)
		}
	}
}

// evictBBUsedLater journals a use of bb in a journal the first read did not see.
func evictBBUsedLater(t *testing.T, dir string) func() {
	return func() { evictJournal(t, dir, "late.log", evictAt(5), evictUse("bb", evictAt(5))) }
}

func TestEvictRunBringsStoreUnderLimitThenRefuses(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	own := filepath.Join(dir, "journal", "own.log")
	sum, ran, err := Evict(dir, 8, own, evictNow)
	must(t, err)
	if !ran || sum.EvictedEntries != 1 || sum.EvictedBytes != 4 {
		t.Fatalf("ran %v, evicted %d entries and %d bytes; want true, 1 and 4", ran, sum.EvictedEntries, sum.EvictedBytes)
	}
	if exists(t, filepath.Join(dir, "bb", "bb-a")) || exists(t, filepath.Join(dir, "b1", "b1-d")) {
		t.Error("bb was not evicted")
	}
	if !exists(t, filepath.Join(dir, "aa", "aa-a")) || !exists(t, filepath.Join(dir, "c1", "c1-d")) {
		t.Error("aa or cc was evicted")
	}
	snap, err := ReadSnapshot(filepath.Join(dir, "index.jsonl"))
	must(t, err)
	if len(snap) != 2 || !snap["aa"].LastUse.Equal(evictAt(3)) || !snap["cc"].LastUse.Equal(evictAt(4)) {
		t.Errorf("snapshot %v, want aa and cc with their journal uses", snap)
	}
	if exists(t, own) || exists(t, filepath.Join(dir, "journal", "dead.log")) {
		t.Error("the dead or own journal was not removed")
	}
	// The snapshot was written at evictNow, so a minute later the gate refuses.
	must(t, os.Chtimes(filepath.Join(dir, "index.jsonl"), evictNow, evictNow))
	if sum, ran, err := Evict(dir, 8, own, evictNow.Add(time.Minute)); err != nil || ran || sum != (Summary{}) {
		t.Errorf("second call ran %v with %+v and err %v, want refused", ran, sum, err)
	}
}

func TestEvictRunKeepsLiveUse(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	must(t, os.Remove(filepath.Join(dir, "journal", "dead.log")))
	// The live journal started at minute 1 and used bb then: bb is protected, aa is not.
	live := evictJournal(t, dir, "live.log", evictAt(1), evictUse("bb", evictAt(1)))
	t.Cleanup(holdJournal(t, live))
	sum, ran, err := Evict(dir, 4, filepath.Join(dir, "journal", "own.log"), evictNow)
	must(t, err)
	if !ran || sum.EvictedEntries != 1 {
		t.Fatalf("ran %v, evicted %d entries; want true and 1", ran, sum.EvictedEntries)
	}
	if !exists(t, filepath.Join(dir, "bb", "bb-a")) || !exists(t, filepath.Join(dir, "b1", "b1-d")) {
		t.Error("bb, used by the live journal, was evicted")
	}
	if exists(t, filepath.Join(dir, "aa", "aa-a")) || !exists(t, live) {
		t.Error("aa was not evicted, or the live journal was removed")
	}
}

func TestEvictRunKeepsEntryUsedDuringEviction(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	sum, ran, err := evict(dir, 8, filepath.Join(dir, "journal", "own.log"), evictNow, evictBBUsedLater(t, dir), os.Remove)
	must(t, err)
	if !ran || sum.EvictedEntries != 0 || sum.EvictedBytes != 0 {
		t.Fatalf("ran %v, evicted %d entries and %d bytes; want true, 0 and 0", ran, sum.EvictedEntries, sum.EvictedBytes)
	}
	if !exists(t, filepath.Join(dir, "bb", "bb-a")) || !exists(t, filepath.Join(dir, "b1", "b1-d")) {
		t.Error("bb, used during the eviction, was evicted")
	}
}

// TestEvictRunEntryWithoutFiles: bb has no files, as an earlier failed run leaves it. The plan
// chooses it: a chosen entry with no files leaves the snapshot, unless the reread names it.
func TestEvictRunEntryWithoutFiles(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		during func(t *testing.T, dir string) func()
		kept   bool
	}{
		{"dropped when not named", func(*testing.T, string) func() { return nil }, false},
		{"kept when the reread names it", evictBBUsedLater, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := evictRunCache(t)
			must(t, os.Remove(filepath.Join(dir, "bb", "bb-a")))
			must(t, os.Remove(filepath.Join(dir, "b1", "b1-d")))
			sum, ran, err := evict(dir, 8, filepath.Join(dir, "journal", "own.log"), evictNow, c.during(t, dir), os.Remove)
			must(t, err)
			if !ran || sum.EvictedEntries != 0 || sum.EvictedBytes != 0 {
				t.Fatalf("ran %v, evicted %d entries and %d bytes; want true, 0 and 0", ran, sum.EvictedEntries, sum.EvictedBytes)
			}
			snap, err := ReadSnapshot(filepath.Join(dir, "index.jsonl"))
			must(t, err)
			if _, ok := snap["bb"]; ok != c.kept {
				t.Errorf("snapshot %v, want bb kept %v", snap, c.kept)
			}
		})
	}
}

func TestEvictRunFailureKeepsJournals(t *testing.T) {
	t.Parallel()
	cut := func(t *testing.T, dir string) {
		writeDiskFile(t, filepath.Join(dir, "journal", "bad.log"), []byte("junk\n"))
	}
	cases := []struct {
		name     string
		before   func(t *testing.T, dir string)
		during   func(t *testing.T, dir string)
		maxBytes int64
	}{
		{name: "gate lock fails", before: func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "evict.lock"), 0o755))
		}, maxBytes: 8},
		{name: "snapshot cut off", before: func(t *testing.T, dir string) {
			writeDiskFile(t, filepath.Join(dir, "index.jsonl"), []byte("{"))
			must(t, os.Chtimes(filepath.Join(dir, "index.jsonl"), evictNow.Add(-time.Hour), evictNow.Add(-time.Hour)))
		}, maxBytes: 8},
		{name: "journal cut off", before: cut, maxBytes: 8},
		{name: "plan refused", maxBytes: 0},
		{name: "rename blocked", before: func(t *testing.T, dir string) {
			must(t, os.Mkdir(filepath.Join(dir, "bb", "bb-a.evicting"), 0o755))
		}, maxBytes: 8},
		{name: "reread cut off", during: cut, maxBytes: 8},
		{name: "snapshot blocked", during: func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, "index.jsonl")))
			must(t, os.Mkdir(filepath.Join(dir, "index.jsonl"), 0o755))
		}, maxBytes: 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := evictRunCache(t)
			if c.before != nil {
				c.before(t, dir)
			}
			var during func()
			if c.during != nil {
				during = func() { c.during(t, dir) }
			}
			_, ran, err := evict(dir, c.maxBytes, filepath.Join(dir, "journal", "own.log"), evictNow, during, os.Remove)
			if err == nil || ran {
				t.Errorf("ran %v with err %v, want ran false and an error", ran, err)
			}
			for _, rel := range []string{"journal/dead.log", "journal/own.log"} {
				if !exists(t, filepath.Join(dir, rel)) {
					t.Errorf("%s was removed by a failed eviction", rel)
				}
			}
			evictUsable(t, dir)
		})
	}
}

func TestEvictRunLaterEvictionDropsEntryWithoutFiles(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	own := filepath.Join(dir, "journal", "own.log")
	// The reread fails after bb's files were moved aside, so bb is left with no files.
	bad := func() { writeDiskFile(t, filepath.Join(dir, "journal", "bad.log"), []byte("junk\n")) }
	if _, ran, err := evict(dir, 8, own, evictNow, bad, os.Remove); err == nil || ran {
		t.Fatalf("failing eviction ran %v with err %v, want ran false and an error", ran, err)
	}
	must(t, os.Remove(filepath.Join(dir, "journal", "bad.log")))
	sum, ran, err := Evict(dir, 8, own, evictNow)
	must(t, err)
	if !ran || sum.EvictedEntries != 0 || sum.EvictedBytes != 0 {
		t.Fatalf("later eviction ran %v, evicted %d entries and %d bytes; want true, 0 and 0", ran, sum.EvictedEntries, sum.EvictedBytes)
	}
	evictSnapshotAAandCC(t, dir)
}

func TestEvictRunDropFailureWritesNoSnapshot(t *testing.T) {
	t.Parallel()
	d, dir := newDisk(t)
	// aa and ee name one body, 1d; cc has its own. ee's index file is gone.
	for _, p := range [][2]string{{"aa", "1d"}, {"ee", "1d"}, {"cc", "c1"}} {
		_, err := d.Put([]byte(p[0]), []byte(p[1]), []byte("body"))
		must(t, err)
	}
	must(t, os.Remove(filepath.Join(dir, "ee", "ee-a")))
	idx := Index{
		"aa": {OutputID: "1d", Size: 4, PutTime: evictAt(0), LastUse: evictAt(2)},
		"ee": {OutputID: "1d", Size: 4, PutTime: evictAt(0), LastUse: evictAt(0)},
		"cc": {OutputID: "c1", Size: 4, PutTime: evictAt(0), LastUse: evictAt(1)},
	}
	index := filepath.Join(dir, "index.jsonl")
	must(t, WriteSnapshot(index, idx))
	must(t, os.Chtimes(index, evictNow.Add(-time.Hour), evictNow.Add(-time.Hour)))
	// With 1d a file in place of its shard, stat of ee's body fails with a non-absence error.
	must(t, os.RemoveAll(filepath.Join(dir, "1d")))
	writeDiskFile(t, filepath.Join(dir, "1d"), []byte("x"))

	_, ran, err := Evict(dir, 4, filepath.Join(dir, "journal", "own.log"), evictNow)
	if err == nil || ran {
		t.Fatalf("ran %v with err %v, want ran false and an error", ran, err)
	}
	snap, err := ReadSnapshot(index)
	must(t, err)
	if len(snap) != 3 {
		t.Errorf("snapshot %v, want the three entries unchanged", snap)
	}
}

func TestEvictRunJournalRemovalErrorKeepsSnapshot(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	own := filepath.Join(dir, "journal", "own.log")
	// A non-empty directory cannot be removed as the own journal.
	must(t, os.Remove(own))
	writeDiskFile(t, filepath.Join(own, "x"), []byte("x"))
	sum, ran, err := Evict(dir, 8, own, evictNow)
	if err == nil || !ran || sum.EvictedEntries != 1 || sum.EvictedBytes != 4 {
		t.Fatalf("ran %v, evicted %d entries and %d bytes, err %v; want ran true, 1 entry, 4 bytes and an error", ran, sum.EvictedEntries, sum.EvictedBytes, err)
	}
	evictSnapshotAAandCC(t, dir)
}

var errLeftover = errors.New("leftover test failure")

func TestEvictLeftoverRemovesLeftoversInShards(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	leftovers := []string{"aa/aa-a.evicting", "bb/bb-d.evicting"}
	for _, rel := range leftovers {
		writeDiskFile(t, filepath.Join(dir, rel), []byte("left"))
	}
	writeDiskFile(t, filepath.Join(dir, "aa", "aa-x"), []byte("x"))
	_, _, err := Evict(dir, 8, filepath.Join(dir, "journal", "own.log"), evictNow)
	must(t, err)
	for _, rel := range leftovers {
		if exists(t, filepath.Join(dir, rel)) {
			t.Errorf("leftover %s was not removed", rel)
		}
	}
	if !exists(t, filepath.Join(dir, "aa", "aa-x")) {
		t.Error("aa-x, which is not a leftover, was removed")
	}
}

func TestEvictLeftoverRefusedGateKeepsFiles(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	leftover := filepath.Join(dir, "aa", "aa-a.evicting")
	writeDiskFile(t, leftover, []byte("left"))
	// A snapshot written at evictNow refuses an eviction at evictNow.
	must(t, os.Chtimes(filepath.Join(dir, "index.jsonl"), evictNow, evictNow))
	_, ran, err := Evict(dir, 8, filepath.Join(dir, "journal", "own.log"), evictNow)
	if err != nil || ran || !exists(t, leftover) {
		t.Errorf("refused eviction ran %v with err %v, or removed the leftover", ran, err)
	}
}

func TestEvictLeftoverSelectsRegularFilesInShards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := map[string]bool{"aa/aa-a.evicting": true, "bb/bb-d.evicting": true, "aa/aa-x": false, "cc/cc-a": false, "abc/abc-a.evicting": false, "index.jsonl.evicting": false}
	for rel := range want {
		writeDiskFile(t, filepath.Join(dir, rel), []byte("x"))
	}
	// An empty directory: os.Remove would delete it, so only the regular-file check keeps it.
	must(t, os.MkdirAll(filepath.Join(dir, "dd", "dd-a.evicting"), 0o755))
	want["dd/dd-a.evicting"] = false
	must(t, removeLeftovers(dir, os.ReadDir, os.Remove))
	for rel, removed := range want {
		if exists(t, filepath.Join(dir, rel)) == removed {
			t.Errorf("%s: removed %v, want %v", rel, !removed, removed)
		}
	}
}

func TestEvictLeftoverGoneIsNoError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeDiskFile(t, filepath.Join(dir, "aa", "aa-a.evicting"), []byte("x"))
	gone := func(path string) error {
		must(t, os.Remove(path))
		return os.Remove(path)
	}
	if err := removeLeftovers(dir, os.ReadDir, gone); err != nil {
		t.Errorf("removeLeftovers with a leftover already gone: %v, want nil", err)
	}
}

func TestEvictLeftoverListingFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leftover := filepath.Join(dir, "aa", "aa-a.evicting")
	writeDiskFile(t, leftover, []byte("x"))
	for _, failAt := range []string{dir, filepath.Join(dir, "aa")} {
		read := func(path string) ([]os.DirEntry, error) {
			if path == failAt {
				return nil, errLeftover
			}
			return os.ReadDir(path)
		}
		if err := removeLeftovers(dir, read, os.Remove); !errors.Is(err, errLeftover) || !exists(t, leftover) {
			t.Errorf("listing %s fails: err %v, want the listing error and the leftover kept", failAt, err)
		}
	}
}

func TestEvictLeftoverRemoveFailureKeepsJournals(t *testing.T) {
	t.Parallel()
	dir := evictRunCache(t)
	writeDiskFile(t, filepath.Join(dir, "aa", "aa-a.evicting"), []byte("x"))
	failing := func(string) error { return errLeftover }
	_, ran, err := evict(dir, 8, filepath.Join(dir, "journal", "own.log"), evictNow, nil, failing)
	if !errors.Is(err, errLeftover) || ran {
		t.Fatalf("ran %v with err %v, want ran false and the removal error", ran, err)
	}
	for _, rel := range []string{"journal/dead.log", "journal/own.log"} {
		if !exists(t, filepath.Join(dir, rel)) {
			t.Errorf("%s was removed by a failed eviction", rel)
		}
	}
	evictUsable(t, dir)
}
