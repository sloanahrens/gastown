package session

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type transcriptArchiveFile struct {
	age  time.Duration
	size int
}

// TestTranscriptArchivePrune checks the prune removes what is older than the
// keep, then the oldest until the rest fit under the cap, and removes the seat
// directory it empties, leaving a younger set alone.
func TestTranscriptArchivePrune(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	cases := []struct {
		name  string
		cap   int64
		files map[string]transcriptArchiveFile
		want  map[string]bool
	}{
		{
			name: "age and an emptied seat",
			cap:  1 << 20,
			files: map[string]transcriptArchiveFile{
				"rig/seat/old.jsonl":  {8 * 24 * hour, 1},
				"rig/seat/keep.jsonl": {hour, 1},
				"rig/gone/only.jsonl": {8 * 24 * hour, 1},
			},
			want: map[string]bool{
				"rig/seat/old.jsonl": false, "rig/seat/keep.jsonl": true, "rig/gone": false,
			},
		},
		{
			name: "the oldest until the rest fit the cap",
			cap:  60,
			files: map[string]transcriptArchiveFile{
				"rig/seat/a.jsonl": {2 * hour, 60},
				"rig/seat/b.jsonl": {hour, 20},
				"rig/new/c.jsonl":  {5 * time.Minute, 10},
			},
			want: map[string]bool{
				"rig/seat/a.jsonl": false, "rig/seat/b.jsonl": true, "rig/new/c.jsonl": true,
			},
		},
		{
			name: "a file exactly at the keep stays and one past it goes",
			cap:  1 << 20,
			files: map[string]transcriptArchiveFile{
				"rig/seat/at.jsonl":   {sandboxTranscriptKeep, 1},
				"rig/seat/past.jsonl": {sandboxTranscriptKeep + time.Second, 1},
			},
			want: map[string]bool{"rig/seat/at.jsonl": true, "rig/seat/past.jsonl": false},
		},
		{
			name: "a total exactly at the cap evicts nothing",
			cap:  60,
			files: map[string]transcriptArchiveFile{
				"rig/seat/a.jsonl": {2 * hour, 30},
				"rig/seat/b.jsonl": {hour, 30},
			},
			want: map[string]bool{"rig/seat/a.jsonl": true, "rig/seat/b.jsonl": true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, f := range c.files {
				p := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, make([]byte, f.size), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, now.Add(-f.age), now.Add(-f.age)); err != nil {
					t.Fatal(err)
				}
			}
			if err := pruneTranscriptArchive(root, now, sandboxTranscriptKeep, c.cap); err != nil {
				t.Fatalf("pruneTranscriptArchive: %v", err)
			}
			for rel, present := range c.want {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); (err == nil) != present {
					t.Errorf("%s present = %v, want %v", rel, err == nil, present)
				}
			}
		})
	}
}

// TestTranscriptArchivePruneErrors checks a missing root is not an error and a
// path that is not a directory where one belongs is an error naming it.
func TestTranscriptArchivePruneErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	if err := pruneTranscriptArchive(filepath.Join(t.TempDir(), "gone"), now, sandboxTranscriptKeep, 1<<20); err != nil {
		t.Errorf("prune of a missing root: %v", err)
	}
	for _, name := range []string{"root", "rig", "seat"} {
		root := filepath.Join(t.TempDir(), "archive")
		want := root
		switch name {
		case "rig":
			root = t.TempDir()
			want = filepath.Join(root, "rig")
		case "seat":
			root = t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "rig"), 0o755); err != nil {
				t.Fatal(err)
			}
			want = filepath.Join(root, "rig", "seat")
		}
		if err := os.WriteFile(want, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := pruneTranscriptArchive(root, now, sandboxTranscriptKeep, 1<<20)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("prune with the %s not a directory = %v, want an error naming %s", name, err, want)
		}
	}
}

// TestTranscriptArchivePruneLeavesLinksAlone: the archive's files are regular
// files; a link in a seat directory is neither followed nor removed nor counted,
// whatever its target's age or the cap, and what it points at is untouched.
func TestTranscriptArchivePruneLeavesLinksAlone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	root, outside := t.TempDir(), filepath.Join(t.TempDir(), "target.jsonl")
	if err := os.WriteFile(outside, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * sandboxTranscriptKeep)
	if err := os.Chtimes(outside, old, old); err != nil {
		t.Fatal(err)
	}
	seat := filepath.Join(root, "rig", "seat")
	if err := os.MkdirAll(seat, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(seat, "link.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// A cap smaller than the link's own size: counting the link would evict it.
	if err := pruneTranscriptArchive(root, now, sandboxTranscriptKeep, 1); err != nil {
		t.Fatalf("pruneTranscriptArchive: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("the link was removed: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "target" {
		t.Errorf("the link's target = %q, %v; want it untouched", got, err)
	}
}

// failingArchiveFS is the real filesystem with chosen calls failed: readDir of
// readFail on its nth call (1 is the first), info of the file named infoFail,
// remove of the paths in removeFail. Each failure is a named error, so a row
// can assert the report names the path.
type failingArchiveFS struct {
	readFail   string
	readOn     int
	infoFail   string
	removeFail map[string]bool
	reads      map[string]int
}

func (f *failingArchiveFS) fs() archiveFS {
	f.reads = map[string]int{}
	return archiveFS{
		readDir: func(name string) ([]os.DirEntry, error) {
			f.reads[name]++
			if name == f.readFail && f.reads[name] >= f.readOn {
				return nil, errors.New("read refused")
			}
			return os.ReadDir(name)
		},
		remove: func(name string) error {
			if f.removeFail[name] {
				return errors.New("remove refused")
			}
			return os.Remove(name)
		},
		info: func(e fs.DirEntry) (fs.FileInfo, error) {
			if e.Name() == f.infoFail {
				return nil, errors.New("info refused")
			}
			return e.Info()
		},
	}
}

// TestTranscriptArchivePruneReportsEachFailureOnce: every failing call of the
// prune is reported once, naming its path, and the prune goes on with the rest
// (gt-w8rs2l).
func TestTranscriptArchivePruneReportsEachFailureOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	build := func(t *testing.T) (root string, files map[string]string) {
		root = t.TempDir()
		files = map[string]string{
			"old":   filepath.Join(root, "rig", "seat", "old.jsonl"),
			"young": filepath.Join(root, "rig", "seat", "young.jsonl"),
			"other": filepath.Join(root, "rig2", "seat2", "x.jsonl"),
		}
		ages := map[string]time.Duration{"old": 2 * sandboxTranscriptKeep, "young": time.Hour, "other": 2 * time.Hour}
		for k, p := range files {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, make([]byte, 50), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, now.Add(-ages[k]), now.Add(-ages[k])); err != nil {
				t.Fatal(err)
			}
		}
		return root, files
	}
	cases := []struct {
		name string
		cap  int64
		set  func(f *failingArchiveFS, root string, files map[string]string)
		want func(root string, files map[string]string) string // the path the one error names
	}{
		{"the rig directory cannot be read", 1 << 20,
			func(f *failingArchiveFS, root string, _ map[string]string) {
				f.readFail, f.readOn = filepath.Join(root, "rig"), 1
			},
			func(root string, _ map[string]string) string { return filepath.Join(root, "rig") }},
		{"the seat directory cannot be read in the first pass, reported once", 1 << 20,
			func(f *failingArchiveFS, root string, _ map[string]string) {
				f.readFail, f.readOn = filepath.Join(root, "rig", "seat"), 1
			},
			func(root string, _ map[string]string) string { return filepath.Join(root, "rig", "seat") }},
		{"the seat directory cannot be read in the last pass", 1 << 20,
			func(f *failingArchiveFS, root string, _ map[string]string) {
				f.readFail, f.readOn = filepath.Join(root, "rig", "seat"), 2
			},
			func(root string, _ map[string]string) string { return filepath.Join(root, "rig", "seat") }},
		{"a file's info cannot be read", 1 << 20,
			func(f *failingArchiveFS, _ string, _ map[string]string) { f.infoFail = "young.jsonl" },
			func(_ string, files map[string]string) string { return files["young"] }},
		{"an old file cannot be removed", 1 << 20,
			func(f *failingArchiveFS, _ string, files map[string]string) {
				f.removeFail = map[string]bool{files["old"]: true}
			},
			func(_ string, files map[string]string) string { return files["old"] }},
		{"a file the cap evicts cannot be removed", 60,
			func(f *failingArchiveFS, _ string, files map[string]string) {
				f.removeFail = map[string]bool{files["other"]: true}
			},
			func(_ string, files map[string]string) string { return files["other"] }},
		{"an emptied seat directory cannot be removed", 1 << 20,
			func(f *failingArchiveFS, root string, files map[string]string) {
				f.removeFail = map[string]bool{files["old"]: false, filepath.Join(root, "rig3", "seat3"): true}
			},
			func(root string, _ map[string]string) string { return filepath.Join(root, "rig3", "seat3") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root, files := build(t)
			if err := os.MkdirAll(filepath.Join(root, "rig3", "seat3"), 0o755); err != nil {
				t.Fatal(err)
			}
			f := &failingArchiveFS{}
			c.set(f, root, files)
			err := pruneTranscriptArchiveWith(f.fs(), root, now, sandboxTranscriptKeep, c.cap)
			want := c.want(root, files)
			if err == nil || strings.Count(err.Error(), want) != 1 {
				t.Errorf("err = %v, want exactly one report naming %s", err, want)
			}
		})
	}
}

// TestTranscriptArchivePruneCountsAFileItCannotRemove: a file whose removal
// fails still exists, so the total the cap is held to still counts it (gt-w8rs2l).
func TestTranscriptArchivePruneCountsAFileItCannotRemove(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	seat := filepath.Join(root, "rig", "seat")
	if err := os.MkdirAll(seat, 0o755); err != nil {
		t.Fatal(err)
	}
	old, young := filepath.Join(seat, "old.jsonl"), filepath.Join(seat, "young.jsonl")
	for p, age := range map[string]time.Duration{old: 2 * sandboxTranscriptKeep, young: time.Hour} {
		if err := os.WriteFile(p, make([]byte, 100), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	f := &failingArchiveFS{removeFail: map[string]bool{old: true}}
	// Cap 150: the old file (100) is past the keep but cannot go; with it still
	// counted the total is 200 and the young file (100) must be evicted.
	if err := pruneTranscriptArchiveWith(f.fs(), root, now, sandboxTranscriptKeep, 150); err == nil {
		t.Fatal("the failed removal was not reported")
	}
	if _, err := os.Stat(young); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the young file stayed although the unremovable one still counts toward the cap: %v", err)
	}
}
