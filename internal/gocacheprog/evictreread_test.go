package gocacheprog

import (
	"strings"
	"testing"
	"time"
)

// rereadUse is one use line of a journal, stamped at at.
func rereadUse(action string, at time.Time) string {
	return `{"op":"use","action":"` + action + `","time":"` + at.Format(time.RFC3339) + `"}` + "\n"
}

// rereadFirst is the first read Evict makes before planning, as a set.
func rereadFirst(t *testing.T, path string) map[Record]bool {
	t.Helper()
	c, err := ReadJournal(path)
	must(t, err)
	first := make(map[Record]bool, len(c.Records))
	for _, r := range c.Records {
		first[utcRecord(r)] = true
	}
	return first
}

func TestEvictRereadGainsIdsSinceFirstRead(t *testing.T) {
	t.Parallel()
	// Offset times make the second read parse a fresh fixed zone, so only a UTC compare matches.
	at := journalT0.In(time.FixedZone("A", 5*3600+30*60))
	read := evictPut("aa", "bb", 4, at) + rereadUse("aa", at.Add(time.Second))
	for _, c := range []struct {
		name, added, wantActions, wantOutputs string
		idx                                   Index
	}{
		{name: "a use and a put",
			added:       rereadUse("aa", at.Add(2*time.Second)) + evictPut("cc", "dd", 5, at.Add(3*time.Second)),
			idx:         Index{"aa": {OutputID: "bb"}},
			wantActions: "aa,cc", wantOutputs: "bb,dd"},
		{name: "a put the index names an earlier output for",
			added:       evictPut("cc", "dd", 5, at.Add(2*time.Second)),
			idx:         Index{"cc": {OutputID: "ee"}},
			wantActions: "cc", wantOutputs: "ee,dd"},
		{name: "records already read"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := evictJournal(t, dir, "own.log", at, read)
			first := rereadFirst(t, path)
			evictJournal(t, dir, "own.log", at, read+c.added)
			gainedActions, gainedOutputs, err := gainedSince(dir, path, first, c.idx)()
			must(t, err)
			if a, o := joined(gainedActions), joined(gainedOutputs); a != c.wantActions || o != c.wantOutputs {
				t.Errorf("gained %q and %q, want %q and %q", a, o, c.wantActions, c.wantOutputs)
			}
		})
	}
}

func TestEvictRereadReadFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := evictJournal(t, dir, "own.log", journalT0, evictPut("aa", "bb", 4, journalT0))
	writeDiskFile(t, path, []byte("not a journal\n"))
	_, _, err := gainedSince(dir, path, nil, Index{})()
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want an error naming %s", err, path)
	}
}
