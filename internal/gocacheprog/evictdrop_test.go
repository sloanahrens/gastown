package gocacheprog

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEvictDropRemovesMissing(t *testing.T) {
	t.Parallel()
	d, dir := newDisk(t)
	idx := Index{}
	put := func(action, output string) {
		_, err := d.Put([]byte(action), []byte(output), []byte("hello"))
		must(t, err)
		idx[action] = IndexEntry{OutputID: output, Size: 5}
	}
	put("aa", "bb") // chosen, index file removed
	put("cc", "dd") // chosen, body removed
	put("ee", "ff") // chosen, complete
	put("10", "11") // chosen and kept, body removed
	put("12", "13") // not chosen, body removed
	for _, p := range []string{"aa/aa-a", "dd/dd-d", "11/11-d", "13/13-d"} {
		must(t, os.Remove(filepath.Join(dir, p)))
	}
	must(t, dropMissing(dir, idx, idsOf("aa", "cc", "ee", "10", "14"), idsOf("10")))
	if got, want := slices.Sorted(maps.Keys(idx)), []string{"10", "12", "ee"}; !slices.Equal(got, want) {
		t.Errorf("idx keys %v, want %v", got, want)
	}
}

func TestEvictDropStatFailure(t *testing.T) {
	t.Parallel()
	d, dir := newDisk(t)
	_, err := d.Put([]byte("aa"), []byte("bb"), []byte("hello"))
	must(t, err)
	_, err = d.Put([]byte("cc"), []byte("dd"), []byte("hello"))
	must(t, err)
	idx := Index{"aa": {OutputID: "bb", Size: 5}, "cc": {OutputID: "dd", Size: 5}}
	must(t, os.Remove(filepath.Join(dir, "cc", "cc-a")))
	// A shard that is a regular file makes stat fail with ENOTDIR, which is not absence.
	must(t, os.RemoveAll(filepath.Join(dir, "aa")))
	writeDiskFile(t, filepath.Join(dir, "aa"), []byte("not a shard"))
	// cc would be dropped, but the failing stat on aa leaves idx unchanged.
	if err := dropMissing(dir, idx, idsOf("cc", "aa"), nil); err == nil {
		t.Fatal("dropMissing succeeded, want a stat error")
	}
	if got, want := slices.Sorted(maps.Keys(idx)), []string{"aa", "cc"}; !slices.Equal(got, want) {
		t.Errorf("idx keys %v, want %v unchanged", got, want)
	}
	if err := dropMissing(dir, Index{"zz": {OutputID: "bb"}}, idsOf("zz"), nil); err == nil {
		t.Error("dropMissing accepted a non-hex action id")
	}
}
