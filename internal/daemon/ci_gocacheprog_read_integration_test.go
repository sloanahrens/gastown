//go:build integration

// The read program is shell text, so proof of its refusals comes from running
// it under sh over a temp dir standing in for the ci-gocacheprog volume (gt-5lpie8).

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ciGocacheprogWrite writes content to dir/name.
func ciGocacheprogWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// TestIntegrationCIGocacheprogRead runs the read program over a temp volume: a
// regular file is printed and removed, an absent one exits 3; a link (to a file
// or dangling) exits 4, removed with its target untouched, as does a directory,
// left; a directory at the aside path exits 5 with the stats file unread.
func TestIntegrationCIGocacheprogRead(t *testing.T) {
	rows := []struct {
		name  string
		place func(t *testing.T, dir string)
		want  string
		code  int
		stays bool
	}{
		{"a regular file", func(t *testing.T, dir string) { ciGocacheprogWrite(t, dir, "stats.jsonl", "the stats\n") }, "the stats\n", 0, false},
		{"an absent file", nil, "", 3, false},
		{"a link to a file", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "target.txt"), filepath.Join(dir, "stats.jsonl")); err != nil {
				t.Fatalf("making the link: %v", err)
			}
		}, "", 4, false},
		{"a dangling link", func(t *testing.T, dir string) {
			if err := os.Symlink(filepath.Join(dir, "gone.txt"), filepath.Join(dir, "stats.jsonl")); err != nil {
				t.Fatalf("making the dangling link: %v", err)
			}
		}, "", 4, false},
		{"a directory", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "stats.jsonl"), 0o755); err != nil {
				t.Fatalf("making the directory: %v", err)
			}
		}, "", 4, true},
		{"a directory at the aside path", func(t *testing.T, dir string) {
			ciGocacheprogWrite(t, dir, "stats.jsonl", "the stats\n")
			if err := os.Mkdir(filepath.Join(dir, "stats.jsonl.read"), 0o755); err != nil {
				t.Fatalf("making the aside directory: %v", err)
			}
		}, "", 5, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			dir := t.TempDir()
			ciGocacheprogWrite(t, dir, "target.txt", "keep\n")
			if row.place != nil {
				row.place(t, dir)
			}
			cmd := exec.Command("/bin/sh", "-c", ciGocacheprogReadScript, "ci-gocacheprog-read",
				filepath.Join(dir, "stats.jsonl"), filepath.Join(dir, "stats.jsonl.read"), "4096")
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.Output()
			code, stderr := 0, []byte(nil)
			if exitErr, ok := err.(*exec.ExitError); err != nil {
				if !ok {
					t.Fatalf("running the read program: %v", err)
				}
				code, stderr = exitErr.ExitCode(), exitErr.Stderr
			}
			if string(out) != row.want || code != row.code {
				t.Fatalf("read = %q, exit %d; want %q, exit %d", out, code, row.want, row.code)
			}
			if code == 4 && string(stderr) != "the stats path is not a regular file\n" {
				t.Fatalf("stderr = %q; want one not-regular line", stderr)
			}
			if _, err := os.Lstat(filepath.Join(dir, "stats.jsonl")); (err == nil) != row.stays {
				t.Fatalf("stats path after the read: %v; want it to stay = %v", err, row.stays)
			}
			if b, err := os.ReadFile(filepath.Join(dir, "target.txt")); err != nil || string(b) != "keep\n" {
				t.Fatalf("link target = %q, %v; want it untouched", b, err)
			}
		})
	}
}
