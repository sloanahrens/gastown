package seatship

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

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
			must(t, os.MkdirAll(filepath.Join(dir, "p/b"), 0o755))
			for i, f := range tt.files {
				rel, body, _ := strings.Cut(f, "=")
				path, at := filepath.Join(dir, rel), oldest.Add(time.Duration(i)*time.Minute)
				must(t, os.WriteFile(path, []byte(body), 0o644))
				must(t, os.Chtimes(path, at, at))
			}
			if got, err := TailOf(dir, tt.n); err != nil || got != tt.want {
				t.Errorf("TailOf = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

func TestSeatShipTailErrors(t *testing.T) {
	t.Parallel()
	plain := filepath.Join(t.TempDir(), "plain")
	must(t, os.WriteFile(plain, nil, 0o644))
	if _, err := TailOf(filepath.Join(plain, "projects"), 16); err == nil {
		t.Error("TailOf under a regular file = nil error, want an error")
	}
	dangling := t.TempDir()
	must(t, os.Symlink(filepath.Join(dangling, "gone"), filepath.Join(dangling, "dangling.jsonl")))
	if _, err := TailOf(dangling, 16); err == nil {
		t.Error("TailOf over a dangling transcript = nil error, want an error")
	}
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o755))
	must(t, os.Symlink(filepath.Join(dir, "sub"), filepath.Join(dir, "x.jsonl")))
	if _, err := TailOf(dir, 16); err == nil {
		t.Error("TailOf read a directory as a transcript, want an error")
	}
}
