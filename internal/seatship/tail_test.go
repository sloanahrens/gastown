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

// linkTranscript plants a symlink named like a transcript, pointing at a file
// outside the transcript names.
func linkTranscript(t *testing.T, dir string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, "out.txt"), []byte("secret\n"), 0o644))
	must(t, os.Symlink(filepath.Join(dir, "out.txt"), filepath.Join(dir, "x.jsonl")))
}

func TestSeatShipTail(t *testing.T) {
	t.Parallel()
	oldest := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		files    []string // "rel=contents", oldest first
		setup    func(*testing.T, string)
		projects string // subpath under the temp dir; "" is the dir itself
		n        int
		want     string
		fails    bool
	}{
		{"starts mid line", []string{"p/a.jsonl=one\ntwo\nthree\n"}, nil, "", 8, "three\n", false},
		{"starts on a boundary", []string{"p/a.jsonl=one\ntwo\nthree\n"}, nil, "", 6, "three\n", false},
		{"shorter than n", []string{"p/a.jsonl=a\nb\n"}, nil, "", 100, "a\nb\n", false},
		{"newest of two", []string{"p/a.jsonl=old\n", "p/b/c.jsonl=new\n"}, nil, "", 100, "new\n", false},
		{"a final record longer than n has no line boundary", []string{"p/a.jsonl=abc\nlongrecord\n"}, nil, "", 4, "", false},
		{"no transcript", nil, nil, "", 100, "", false},
		{"a projects directory that does not exist", nil, nil, "absent", 100, "", true},
		{"a symlinked transcript is refused, not followed", nil, linkTranscript, "", 16, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			for i, f := range tt.files {
				rel, body, _ := strings.Cut(f, "=")
				p, at := filepath.Join(dir, rel), oldest.Add(time.Duration(i)*time.Minute)
				must(t, os.MkdirAll(filepath.Dir(p), 0o755))
				must(t, os.WriteFile(p, []byte(body), 0o644))
				must(t, os.Chtimes(p, at, at))
			}
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			if got, err := TailOf(filepath.Join(dir, tt.projects), tt.n); (err != nil) != tt.fails || got != tt.want {
				t.Errorf("TailOf = %q, %v; want %q, error %v", got, err, tt.want, tt.fails)
			}
		})
	}
}
