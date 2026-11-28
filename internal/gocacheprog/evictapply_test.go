package gocacheprog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// evictStore returns a store holding action aa with body bb.
func evictStore(t *testing.T) string {
	t.Helper()
	d, dir := newDisk(t)
	_, err := d.Put([]byte("aa"), []byte("bb"), []byte("hello"))
	must(t, err)
	return dir
}

func idsOf(names ...string) [][]byte {
	var out [][]byte
	for _, n := range names {
		out = append(out, []byte(n))
	}
	return out
}

// joined renders ids as "aa,bb"; no ids render as "".
func joined(ids [][]byte) string { return string(bytes.Join(ids, []byte(","))) }

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func TestEvictApplyDeletesChosen(t *testing.T) {
	t.Parallel()
	dir := evictStore(t)
	deletedA, deletedO, err := applyEviction(dir, idsOf("aa"), idsOf("bb"), func() ([][]byte, [][]byte, error) {
		// reread runs once the index has left its name, before anything is deleted.
		if exists(t, filepath.Join(dir, "aa", "aa-a")) || !exists(t, filepath.Join(dir, "aa", "aa-a.evicting")) {
			t.Error("reread ran before the chosen index left its name")
		}
		return nil, nil, nil
	})
	must(t, err)
	if joined(deletedA) != "aa" || joined(deletedO) != "bb" {
		t.Errorf("deleted %q %q, want aa and bb", joined(deletedA), joined(deletedO))
	}
	for _, rel := range []string{"aa/aa-a", "aa/aa-a.evicting", "bb/bb-d", "bb/bb-d.evicting"} {
		if exists(t, filepath.Join(dir, rel)) {
			t.Errorf("%s still exists", rel)
		}
	}
}

func TestEvictApplyKeepsGained(t *testing.T) {
	t.Parallel()
	dir := evictStore(t)
	const newIndex = `{"OutputID":"YmI=","Size":5}`
	deletedA, deletedO, err := applyEviction(dir, idsOf("aa"), idsOf("bb"), func() ([][]byte, [][]byte, error) {
		// A writer takes aa's freed name; bb's name stays free.
		writeDiskFile(t, filepath.Join(dir, "aa", "aa-a"), []byte(newIndex))
		return idsOf("aa"), idsOf("bb"), nil
	})
	must(t, err)
	if joined(deletedA)+joined(deletedO) != "" {
		t.Errorf("deleted %q %q, want none", joined(deletedA), joined(deletedO))
	}
	got, err := os.ReadFile(filepath.Join(dir, "aa", "aa-a"))
	must(t, err)
	if string(got) != newIndex {
		t.Errorf("aa-a = %q, want the newer index kept", got)
	}
	if !exists(t, filepath.Join(dir, "bb", "bb-d")) {
		t.Error("bb was not restored")
	}
}

func TestEvictApplyMissingFile(t *testing.T) {
	t.Parallel()
	dir := evictStore(t)
	// ee and ff were never stored; by reread, aa and bb have gone too.
	deletedA, deletedO, err := applyEviction(dir, idsOf("aa", "ee"), idsOf("bb", "ff"), func() ([][]byte, [][]byte, error) {
		must(t, os.Remove(filepath.Join(dir, "aa", "aa-a.evicting")))
		must(t, os.Remove(filepath.Join(dir, "bb", "bb-d.evicting")))
		return nil, idsOf("bb"), nil
	})
	if err != nil || joined(deletedA)+joined(deletedO) != "" {
		t.Errorf("err %v deleted %q %q, want no error and nothing reported", err, joined(deletedA), joined(deletedO))
	}
}

func TestEvictApplyFailures(t *testing.T) {
	t.Parallel()
	// nonEmptyDir makes path a directory: a rename onto it, a link from it and a remove of it all fail.
	nonEmptyDir := func(t *testing.T, path string) { must(t, os.MkdirAll(filepath.Join(path, "x"), 0o755)) }
	noReread := func(*testing.T, string) ([][]byte, [][]byte, error) { return nil, nil, nil }
	tests := []struct {
		name             string
		setup            func(*testing.T, string)
		actions, outputs [][]byte
		reread           func(*testing.T, string) ([][]byte, [][]byte, error)
		wantDeleted      string
		kept             []string
	}{
		{name: "rename fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"bb/bb-d"},
			setup: func(t *testing.T, dir string) { nonEmptyDir(t, filepath.Join(dir, "aa", "aa-a.evicting")) },
			reread: func(t *testing.T, _ string) ([][]byte, [][]byte, error) {
				t.Error("reread ran after a failed rename")
				return nil, nil, nil
			}},
		{name: "reread fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"aa/aa-a.evicting", "bb/bb-d.evicting"},
			reread: func(*testing.T, string) ([][]byte, [][]byte, error) {
				return nil, nil, errors.New("journal unreadable")
			}},
		{name: "restore fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"aa/aa-a.evicting", "bb/bb-d.evicting"},
			reread: func(t *testing.T, dir string) ([][]byte, [][]byte, error) {
				must(t, os.Remove(filepath.Join(dir, "aa", "aa-a.evicting")))
				nonEmptyDir(t, filepath.Join(dir, "aa", "aa-a.evicting"))
				return idsOf("aa"), nil, nil
			}},
		{name: "delete fails after the first", actions: idsOf("aa"), outputs: idsOf("bb"), wantDeleted: "aa", kept: []string{"bb/bb-d.evicting"},
			reread: func(t *testing.T, dir string) ([][]byte, [][]byte, error) {
				must(t, os.Remove(filepath.Join(dir, "bb", "bb-d.evicting")))
				nonEmptyDir(t, filepath.Join(dir, "bb", "bb-d.evicting"))
				return nil, nil, nil
			}},
		{name: "id is not hex", actions: idsOf("zz"), outputs: idsOf("bb"), kept: []string{"bb/bb-d"}},
		{name: "body rename fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"aa/aa-a.evicting"},
			setup: func(t *testing.T, dir string) { nonEmptyDir(t, filepath.Join(dir, "bb", "bb-d.evicting")) },
			reread: func(t *testing.T, _ string) ([][]byte, [][]byte, error) {
				t.Error("reread ran after a failed rename")
				return nil, nil, nil
			}},
		{name: "action delete fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"bb/bb-d.evicting"},
			reread: func(t *testing.T, dir string) ([][]byte, [][]byte, error) {
				must(t, os.Remove(filepath.Join(dir, "aa", "aa-a.evicting")))
				nonEmptyDir(t, filepath.Join(dir, "aa", "aa-a.evicting"))
				return nil, nil, nil
			}},
		{name: "body restore fails", actions: idsOf("aa"), outputs: idsOf("bb"), kept: []string{"aa/aa-a.evicting", "bb/bb-d.evicting"},
			reread: func(t *testing.T, dir string) ([][]byte, [][]byte, error) {
				must(t, os.Remove(filepath.Join(dir, "bb", "bb-d.evicting")))
				nonEmptyDir(t, filepath.Join(dir, "bb", "bb-d.evicting"))
				return nil, idsOf("bb"), nil
			}},
		{name: "evicted copy cannot be removed", actions: idsOf("aa"), kept: []string{"aa/aa-a.evicting"},
			reread: func(t *testing.T, dir string) ([][]byte, [][]byte, error) {
				// A newer index holds the name, so link refuses; the evicted copy is then a non-empty directory.
				must(t, os.Remove(filepath.Join(dir, "aa", "aa-a.evicting")))
				nonEmptyDir(t, filepath.Join(dir, "aa", "aa-a.evicting"))
				writeDiskFile(t, filepath.Join(dir, "aa", "aa-a"), []byte("newer"))
				return idsOf("aa"), nil, nil
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := evictStore(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			reread := tt.reread
			if reread == nil {
				reread = noReread
			}
			deletedA, _, err := applyEviction(dir, tt.actions, tt.outputs, func() ([][]byte, [][]byte, error) { return reread(t, dir) })
			if err == nil {
				t.Fatal("applyEviction succeeded, want an error")
			}
			if joined(deletedA) != tt.wantDeleted {
				t.Errorf("deleted %q, want %q", joined(deletedA), tt.wantDeleted)
			}
			for _, rel := range tt.kept {
				if !exists(t, filepath.Join(dir, rel)) {
					t.Errorf("%s was deleted after the failure", rel)
				}
			}
		})
	}
}
