package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// A binary path resolves under the plugin directory. A lexical escape is
// refused, and so is a symlink inside the directory whose target lies outside:
// the runner execs whatever the path resolves to, so a link is a way out.
func TestResolveBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "bin", "work")
	if err := os.WriteFile(work, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	evil := filepath.Join(outside, "evil")
	if err := os.WriteFile(evil, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(dir, "file-link")
	if err := os.Symlink(evil, fileLink); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(dir, "dir-link")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	insideLink := filepath.Join(dir, "inside-link")
	if err := os.Symlink(work, insideLink); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		rel     string
		want    string
		wantErr bool
	}{
		{"nested", "bin/work", work, false},
		{"steps back inside", "bin/../bin/work", work, false},
		{"symlink to a file inside", "inside-link", insideLink, false},
		{"binary not yet written", "bin/later", filepath.Join(dir, "bin", "later"), false},
		{"absolute", "/bin/sh", "", true},
		{"parent", "../evil", "", true},
		{"steps back inside then out", "bin/../../evil", "", true},
		{"the plugin directory itself", ".", "", true},
		{"symlink to a file outside", "file-link", "", true},
		{"symlink to a directory outside", "dir-link/evil", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveBinary(dir, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveBinary(%q) = %q, want an error", tc.rel, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBinary(%q) failed: %v", tc.rel, err)
			}
			if got != tc.want {
				t.Errorf("resolveBinary(%q) = %q, want %q", tc.rel, got, tc.want)
			}
		})
	}
}

// BinaryPath is the same contract through the plugin: empty when no binary is
// named, the contained path when one is, an error when the name leaves.
func TestPluginBinaryPath(t *testing.T) {
	t.Parallel()
	p := &Plugin{Path: t.TempDir()}
	if got, err := p.BinaryPath(); err != nil || got != "" {
		t.Fatalf("BinaryPath() = %q, %v; want empty", got, err)
	}
	p.Execution = &Execution{Binary: "../escape"}
	if _, err := p.BinaryPath(); err == nil {
		t.Fatal("BinaryPath() accepted a path outside the plugin directory")
	}
	p.Execution.Binary = "bin/work"
	if got, want := mustBinaryPath(t, p), filepath.Join(p.Path, "bin", "work"); got != want {
		t.Errorf("BinaryPath() = %q, want %q", got, want)
	}
}

func mustBinaryPath(t *testing.T, p *Plugin) string {
	t.Helper()
	got, err := p.BinaryPath()
	if err != nil {
		t.Fatalf("BinaryPath() failed: %v", err)
	}
	return got
}
