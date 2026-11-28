package gocacheprog

import (
	"slices"
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
