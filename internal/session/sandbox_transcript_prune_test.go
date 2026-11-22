package session

import (
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
	now := time.Now()
	if err := pruneTranscriptArchive(filepath.Join(t.TempDir(), "gone"), now, sandboxTranscriptKeep, 1<<20); err != nil {
		t.Errorf("prune of a missing root: %v", err)
	}
	for _, name := range []string{"root", "rig"} {
		root := filepath.Join(t.TempDir(), "archive")
		want := root
		if name == "rig" {
			root = t.TempDir()
			want = filepath.Join(root, "rig")
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
