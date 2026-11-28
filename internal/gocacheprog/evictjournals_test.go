package gocacheprog

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// evictJournal writes a journal named name under dir's journal directory,
// started at start, and returns its path.
func evictJournal(t *testing.T, dir, name string, start time.Time, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "journal", name)
	body := `{"op":"start","time":"` + start.Format(time.RFC3339) + `"}` + "\n" + strings.Join(lines, "")
	writeDiskFile(t, path, []byte(body))
	return path
}

func evictPut(action, output string, size int64, at time.Time) string {
	return `{"op":"put","action":"` + action + `","output":"` + output + `","size":` +
		strconv.FormatInt(size, 10) + `,"time":"` + at.Format(time.RFC3339) + `"}` + "\n"
}

// holdJournal locks a journal as its live writer does and returns the release.
func holdJournal(t *testing.T, path string) func() {
	t.Helper()
	release, took, err := lock.FlockTryAcquire(path)
	must(t, err)
	if !took {
		t.Fatalf("holding %s: want the lock taken", path)
	}
	return release
}

func TestEvictJournalsLiveDeadAndOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	own := evictJournal(t, dir, "own.log", journalT0, evictPut("01", "11", 1, journalT0))
	liveA := evictJournal(t, dir, "a-live.log", journalT0.Add(time.Minute), evictPut("02", "12", 2, journalT0.Add(time.Minute)))
	liveB := evictJournal(t, dir, "b-live.log", journalT0.Add(2*time.Minute), evictPut("03", "13", 3, journalT0.Add(2*time.Minute)))
	dead := evictJournal(t, dir, "c-dead.log", journalT0.Add(3*time.Minute), "not json\n", evictPut("04", "14", 4, journalT0.Add(3*time.Minute)))
	writeDiskFile(t, filepath.Join(dir, "journal", "notes.txt"), []byte("not a journal\n"))
	ownRelease := holdJournal(t, own)
	for _, path := range []string{liveA, liveB} {
		t.Cleanup(holdJournal(t, path))
	}
	want := []Record{
		{Op: opPut, ActionID: "02", OutputID: "12", Size: 2, Time: journalT0.Add(time.Minute)},
		{Op: opPut, ActionID: "03", OutputID: "13", Size: 3, Time: journalT0.Add(2 * time.Minute)},
		{Op: opPut, ActionID: "04", OutputID: "14", Size: 4, Time: journalT0.Add(3 * time.Minute)},
		{Op: opPut, ActionID: "01", OutputID: "11", Size: 1, Time: journalT0},
	}
	records, protectFrom, notLive, err := readJournals(dir, own)
	must(t, err)
	requireRecords(t, records, want)
	// own.log starts earliest, so a protectFrom naming it would mean own counted.
	if !protectFrom.Equal(journalT0.Add(time.Minute)) {
		t.Errorf("protectFrom = %v, want the earliest other live start %v", protectFrom, journalT0.Add(time.Minute))
	}
	if want := []string{dead}; !slices.Equal(notLive, want) {
		t.Errorf("notLive = %v, want %v", notLive, want)
	}
	// Released, own.log is dead too: still never protective and never listed.
	ownRelease()
	if _, _, notLive, err = readJournals(dir, own); err != nil || !slices.Equal(notLive, []string{dead}) {
		t.Fatalf("own released: notLive = %v (err %v), want %v", notLive, err, []string{dead})
	}
}

func TestEvictJournalsMissingDirIsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if records, protectFrom, notLive, err := readJournals(dir, filepath.Join(dir, "journal", "own.log")); err != nil || len(records) != 0 || !protectFrom.IsZero() || len(notLive) != 0 {
		t.Errorf("records=%v protectFrom=%v notLive=%v err=%v, want none", records, protectFrom, notLive, err)
	}
}

func TestEvictJournalsFailuresNameTheCulprit(t *testing.T) {
	t.Parallel()
	fileDir := t.TempDir()
	filePath := filepath.Join(fileDir, "journal")
	must(t, os.WriteFile(filePath, nil, 0o644))
	badDir := t.TempDir()
	badPath := filepath.Join(badDir, "journal", "bad.log")
	writeDiskFile(t, badPath, []byte("not a journal\n"))
	for _, c := range []struct{ name, dir, path string }{
		{"a journal path that is a file", fileDir, filePath},
		{"a journal without a start line", badDir, badPath},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, _, _, err := readJournals(c.dir, ""); err == nil || !strings.Contains(err.Error(), c.path) {
				t.Errorf("error = %v, want an error naming %s", err, c.path)
			}
		})
	}
}
