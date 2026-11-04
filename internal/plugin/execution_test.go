package plugin

import (
	"os"
	"path/filepath"
	"runtime"
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
	danglingOut := filepath.Join(dir, "dangling-out")
	if err := os.Symlink(filepath.Join(outside, "not-yet"), danglingOut); err != nil {
		t.Fatal(err)
	}
	danglingIn := filepath.Join(dir, "dangling-in")
	if err := os.Symlink(filepath.Join(dir, "bin", "later"), danglingIn); err != nil {
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
		{"dangling symlink to a path outside", "dangling-out", "", true},
		{"dangling symlink to a path inside", "dangling-in", "", true},
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

// The container daemon and the Mac host share a town directory, so this
// platform's build beside the plain one wins: the Mac build would be an exec
// format error in the container.
func TestBinaryPathPrefersPerOSBinary(t *testing.T) {
	t.Parallel()
	p := &Plugin{Path: t.TempDir(), Execution: &Execution{Binary: "bin/work"}}
	plain := filepath.Join(p.Path, "bin", "work")
	if err := os.MkdirAll(filepath.Dir(plain), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("mac build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(p.Path, "bin", runtime.GOOS+"-"+runtime.GOARCH, "work")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("platform build\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustBinaryPath(t, p); got != want {
		t.Errorf("BinaryPath() = %q, want the per-OS build %q", got, want)
	}
}

// Without a build for this platform the plugin.md path is returned as before.
func TestBinaryPathFallsBackToPlainBinary(t *testing.T) {
	t.Parallel()
	p := &Plugin{Path: t.TempDir(), Execution: &Execution{Binary: "bin/work"}}
	plain := filepath.Join(p.Path, "bin", "work")
	if err := os.MkdirAll(filepath.Dir(plain), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustBinaryPath(t, p); got != plain {
		t.Errorf("BinaryPath() = %q, want the plain binary %q", got, plain)
	}
}

// The per-OS build is run the same way, so a symlink there is a way out too:
// a candidate that resolves outside the plugin directory is refused.
func TestBinaryPathPerOSCandidateIsEscapeChecked(t *testing.T) {
	t.Parallel()
	p := &Plugin{Path: t.TempDir(), Execution: &Execution{Binary: "bin/work"}}
	outside := filepath.Join(t.TempDir(), "evil")
	if err := os.WriteFile(outside, []byte("evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	perOS := filepath.Join(p.Path, "bin", runtime.GOOS+"-"+runtime.GOARCH, "work")
	if err := os.MkdirAll(filepath.Dir(perOS), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, perOS); err != nil {
		t.Fatal(err)
	}
	if got, err := p.BinaryPath(); err == nil {
		t.Fatalf("BinaryPath() = %q, want the per-OS symlink refused", got)
	}
}

// A per-OS build that cannot be checked is not replaced by the plain binary:
// a dangling link on the candidate's path, and a candidate that fails to stat
// for any reason but absence, are refused. A platform directory that links to
// a real directory with no build inside still falls back.
func TestBinaryPathRefusesUncheckableBuild(t *testing.T) {
	t.Parallel()
	perOS := runtime.GOOS + "-" + runtime.GOARCH
	for _, tc := range []struct {
		name    string
		setup   func(dir string) error
		wantErr bool
	}{
		{"dangling per-OS symlink", func(dir string) error {
			if err := os.MkdirAll(filepath.Join(dir, "bin", perOS), 0o755); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(dir, "bin", "later"), filepath.Join(dir, "bin", perOS, "work"))
		}, true},
		{"dangling per-OS directory symlink", func(dir string) error {
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "bin", perOS))
		}, true},
		{"per-OS path under a regular file", func(dir string) error {
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "bin", perOS), []byte("x\n"), 0o644)
		}, true},
		{"per-OS directory links to an empty directory", func(dir string) error {
			if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
				return err
			}
			target := filepath.Join(dir, "target")
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return os.Symlink(target, filepath.Join(dir, "bin", perOS))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := tc.setup(dir); err != nil {
				t.Fatal(err)
			}
			p := &Plugin{Path: dir, Execution: &Execution{Binary: "bin/work"}}
			got, err := p.BinaryPath()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BinaryPath() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BinaryPath() failed: %v", err)
			}
			if want := filepath.Join(dir, "bin", "work"); got != want {
				t.Errorf("BinaryPath() = %q, want the plain binary %q", got, want)
			}
		})
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
