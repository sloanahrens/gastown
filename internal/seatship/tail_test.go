package seatship

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSeatShipTail: the newest transcript's last n bytes from a line boundary,
// a transcript shorter than n whole, and "" with no transcript.
func TestSeatShipTail(t *testing.T) {
	t.Parallel()
	oldest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		files []string // "rel=contents", oldest first
		n     int
		want  string
	}{
		{"starts mid line", []string{"p/a.jsonl=one\ntwo\nthree\n"}, 8, "three\n"},
		{"starts on a boundary", []string{"p/a.jsonl=one\ntwo\nthree\n"}, 6, "three\n"},
		{"shorter than n", []string{"p/a.jsonl=a\nb\n"}, 100, "a\nb\n"},
		{"newest of two", []string{"p/a.jsonl=old\n", "p/b/c.jsonl=new\n"}, 100, "new\n"},
		{"no transcript", nil, 100, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for i, f := range tt.files {
				rel, body, _ := strings.Cut(f, "=")
				path, at := filepath.Join(dir, rel), oldest.Add(time.Duration(i)*time.Minute)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := TailOf(dir, tt.n); err != nil || got != tt.want {
				t.Errorf("TailOf = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

// TestSeatShipTailRefusals: a projects path the walk cannot enter is an error,
// and a projects path that does not exist holds no transcript.
func TestSeatShipTailRefusals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Run("unreadable", func(t *testing.T) {
		if _, err := TailOf(filepath.Join(file, "projects"), 16); err == nil {
			t.Error("TailOf under a regular file = nil error, want an error")
		}
	})
	t.Run("nonexistent", func(t *testing.T) {
		if got, err := TailOf(filepath.Join(dir, "absent"), 16); err != nil || got != "" {
			t.Errorf("TailOf(absent) = %q, %v; want \"\", nil", got, err)
		}
	})
}
