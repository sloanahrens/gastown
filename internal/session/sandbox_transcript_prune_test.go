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
