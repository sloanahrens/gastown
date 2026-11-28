package gocacheprog

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func indexAt(n int) time.Time { return journalT0.Add(time.Duration(n) * time.Second) }

func indexPut(action, output string, size int64, n int) Record {
	return Record{Op: opPut, ActionID: action, OutputID: output, Size: size, Time: indexAt(n)}
}

func indexUse(action string, n int) Record {
	return Record{Op: opUse, ActionID: action, Time: indexAt(n)}
}

func indexEntry(output string, size int64, put, use int) IndexEntry {
	return IndexEntry{OutputID: output, Size: size, PutTime: indexAt(put), LastUse: indexAt(use)}
}

// sameIndex reports whether a and b hold the same actions with equal fields.
func sameIndex(a, b Index) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || x.OutputID != y.OutputID || x.Size != y.Size || !x.PutTime.Equal(y.PutTime) || !x.LastUse.Equal(y.LastUse) {
			return false
		}
	}
	return true
}

func TestIndexMergeIsOrderFreeAndIdempotent(t *testing.T) {
	t.Parallel()
	records := []Record{
		indexPut("a", "x", 5, 1),
		indexPut("a", "y", 7, 2),
		indexPut("a", "z", 9, 2), // same time as y: the greater output id wins
		indexUse("a", 3),
		indexPut("b", "b", 1, 0),
		indexPut("c", "p", 3, 4),
		indexPut("c", "p", 4, 4), // same time and output: the greater size wins
	}
	want := Index{"a": indexEntry("z", 9, 2, 3), "b": indexEntry("b", 1, 0, 0), "c": indexEntry("p", 4, 4, 4)}
	reversed := slices.Clone(records)
	slices.Reverse(reversed)
	orders := map[string][]Record{
		"forward":  records,
		"reversed": reversed,
		"doubled":  append(append([]Record{}, records...), records...),
	}
	for name, recs := range orders {
		got := Merge(nil, recs)
		if !sameIndex(got, want) {
			t.Errorf("%s: index = %+v, want %+v", name, got, want)
		}
		if again := Merge(got, recs); !sameIndex(again, want) {
			t.Errorf("%s: merging again = %+v, want %+v", name, again, want)
		}
	}
	if split := Merge(Merge(nil, append(slices.Clone(records[:3]), records[4:]...)), records[3:4]); !sameIndex(split, want) {
		t.Errorf("puts then use: index = %+v, want %+v", split, want)
	}

	// A use merged in an earlier call than its put is dropped, since an entry never
	// exists without a size. The same two records in one call keep the use as last use.
	useBeforePut := []Record{indexUse("d", 5), indexPut("d", "w", 2, 1)}
	splitWant := Index{"d": indexEntry("w", 2, 1, 1)}
	if got := Merge(Merge(nil, useBeforePut[:1]), useBeforePut[1:]); !sameIndex(got, splitWant) {
		t.Errorf("use merged before its put: index = %+v, want %+v", got, splitWant)
	}
	oneCallWant := Index{"d": indexEntry("w", 2, 1, 5)}
	if got := Merge(nil, useBeforePut); !sameIndex(got, oneCallWant) {
		t.Errorf("use and put in one call: index = %+v, want %+v", got, oneCallWant)
	}
}

func TestIndexUseRaisesLastUseWithoutLoweringIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		first, second []Record
		want          IndexEntry
	}{
		{"a later use raises last use", []Record{indexPut("a", "x", 5, 1)}, []Record{indexUse("a", 5)}, indexEntry("x", 5, 1, 5)},
		{"an earlier use does not lower it", []Record{indexPut("a", "x", 5, 1), indexUse("a", 5)}, []Record{indexUse("a", 2)}, indexEntry("x", 5, 1, 5)},
		{"an earlier put does not lower it", []Record{indexPut("a", "x", 5, 1), indexUse("a", 5)}, []Record{indexPut("a", "x", 5, 0)}, indexEntry("x", 5, 1, 5)},
		{"a later put sets output and size and raises it", []Record{indexPut("a", "x", 5, 1), indexUse("a", 2)}, []Record{indexPut("a", "q", 6, 3)}, indexEntry("q", 6, 3, 3)},
	}
	for _, c := range cases {
		base := Merge(nil, c.first)
		got := Merge(base, c.second)
		if !sameIndex(got, Index{"a": c.want}) {
			t.Errorf("%s: index = %+v, want %+v", c.name, got, c.want)
		}
		if !sameIndex(base, Merge(nil, c.first)) {
			t.Errorf("%s: Merge changed its input index", c.name)
		}
	}
}

func TestIndexUseOfUnknownActionAddsNothing(t *testing.T) {
	t.Parallel()
	known := Index{"a": indexEntry("x", 5, 1, 1)}
	cases := []struct {
		name    string
		start   Index
		records []Record
		want    Index
	}{
		{"a use alone adds no entry", nil, []Record{indexUse("z", 3)}, Index{}},
		{"a use of an unknown action leaves known entries alone", known, []Record{indexUse("z", 3)}, known},
		{"a use of an action put in the same call is kept", nil, []Record{indexUse("z", 3), indexPut("z", "q", 2, 1)}, Index{"z": indexEntry("q", 2, 1, 3)}},
	}
	for _, c := range cases {
		if got := Merge(c.start, c.records); !sameIndex(got, c.want) {
			t.Errorf("%s: index = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// snapshotPath is a snapshot file path under a fresh temp dir.
func snapshotPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "index.jsonl")
}

func TestIndexMissingSnapshotIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := ReadSnapshot(snapshotPath(t))
	must(t, err)
	if len(got) != 0 {
		t.Errorf("index = %+v, want empty", got)
	}
}

func TestIndexEmptySnapshotReadsAsEmpty(t *testing.T) {
	t.Parallel()
	path := snapshotPath(t)
	must(t, WriteSnapshot(path, Index{}))
	got, err := ReadSnapshot(path)
	must(t, err)
	if len(got) != 0 {
		t.Errorf("index = %+v, want empty", got)
	}
}

func TestIndexSnapshotRoundTripKeepsPutTimes(t *testing.T) {
	t.Parallel()
	// A put time with nanoseconds in a non-UTC zone must come back as the same instant.
	precise := time.Date(2026, 1, 2, 12, 0, 0, 123456789, time.FixedZone("east", 9*3600))
	want := Index{
		"aa": {OutputID: "11", Size: 5, PutTime: precise, LastUse: indexAt(9)},
		"bb": indexEntry("22", 0, 1, 1),
	}
	path := snapshotPath(t)
	must(t, WriteSnapshot(path, want))
	got, err := ReadSnapshot(path)
	must(t, err)
	if !sameIndex(got, want) {
		t.Fatalf("read = %+v, want %+v", got, want)
	}
	// After the reload an older put of the same action does not replace the entry.
	older := []Record{{Op: opPut, ActionID: "aa", OutputID: "ee", Size: 9, Time: precise.Add(-time.Nanosecond)}}
	if merged := Merge(got, older); !sameIndex(merged, want) {
		t.Errorf("older put after reload: index = %+v, want %+v", merged, want)
	}
}

func TestIndexSnapshotWritesOneLinePerEntryInActionOrder(t *testing.T) {
	t.Parallel()
	idx := Index{}
	var want strings.Builder
	for i := range 8 {
		action := fmt.Sprintf("%02x", i)
		at := indexAt(i + 1).Format(time.RFC3339Nano)
		idx[action] = indexEntry("f"+action, int64(i+1), i+1, i+1)
		fmt.Fprintf(&want, `{"action":%q,"output":%q,"size":%d,"put":%q,"use":%q}`+"\n", action, "f"+action, i+1, at, at)
	}
	path := snapshotPath(t)
	must(t, WriteSnapshot(path, idx))
	data, err := os.ReadFile(path)
	must(t, err)
	if string(data) != want.String() {
		t.Errorf("snapshot file = %q, want %q", data, want.String())
	}
}

func TestIndexReadRefusesBadSnapshot(t *testing.T) {
	t.Parallel()
	const good = `{"action":"aa","output":"bb","size":5,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}` + "\n"
	cases := []struct {
		name string
		body string
	}{
		{"a cut last line", good + `{"action":"cc","output":"dd"`},
		{"a complete last line with no newline", good + `{"action":"cc","output":"bb","size":5,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}`},
		{"a line that is not JSON", good + "not json\n"},
		{"a blank line", good + "\n"},
		{"an action that is not hex", good + `{"action":"zz","output":"bb","size":5,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}` + "\n"},
		{"an empty action", good + `{"action":"","output":"bb","size":5,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}` + "\n"},
		{"an output that is not hex", good + `{"action":"cc","output":"xy","size":5,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}` + "\n"},
		{"a negative size", good + `{"action":"cc","output":"bb","size":-1,"put":"2026-01-02T03:04:06Z","use":"2026-01-02T03:04:07Z"}` + "\n"},
		{"a line with no put time", good + `{"action":"cc","output":"bb","size":5,"use":"2026-01-02T03:04:07Z"}` + "\n"},
		{"a line with no last use", good + `{"action":"cc","output":"bb","size":5,"put":"2026-01-02T03:04:06Z"}` + "\n"},
		{"a repeated action", good + good},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := snapshotPath(t)
			must(t, os.WriteFile(path, []byte(c.body), 0o644))
			if _, err := ReadSnapshot(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("error = %v, want an error naming %s", err, path)
			}
		})
	}
}

func TestIndexReadRefusesUnreadableSnapshot(t *testing.T) {
	t.Parallel()
	path := snapshotPath(t)
	must(t, os.Mkdir(path, 0o755))
	if _, err := ReadSnapshot(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want an error naming %s", err, path)
	}
}

func TestIndexWriteReturnsItsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		idx  Index
	}{
		{"a write into a missing directory", filepath.Join(t.TempDir(), "missing", "index.jsonl"), Index{"aa": indexEntry("bb", 1, 1, 1)}},
		// Year 10000 has no JSON encoding, so the entry cannot be written at all.
		{"an entry that cannot be encoded", snapshotPath(t), Index{"aa": {OutputID: "bb", Size: 1, PutTime: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), LastUse: indexAt(1)}}},
	}
	for _, c := range cases {
		if err := WriteSnapshot(c.path, c.idx); err == nil || !strings.Contains(err.Error(), c.path) {
			t.Errorf("%s: error = %v, want an error naming %s", c.name, err, c.path)
		}
	}
}
