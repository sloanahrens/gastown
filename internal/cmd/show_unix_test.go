//go:build !windows

package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeShowCwd is a working directory prepareBdShowExec reads and moves.
type fakeShowCwd struct {
	dir     string
	missing map[string]bool
}

func (c *fakeShowCwd) getwd() (string, error) { return c.dir, nil }

func (c *fakeShowCwd) chdir(dir string) error {
	if c.missing[dir] {
		return errors.New("no such file or directory")
	}
	c.dir = dir
	return nil
}

func TestPrepareBdShowExecAnchorsRelativePathBeforeChdir(t *testing.T) {
	t.Parallel()
	startDir, targetDir := "/work/start", "/work/target"
	cwd := &fakeShowCwd{dir: startDir}

	got, err := prepareBdShowExec(filepath.Join("bin", "bd"), bdShowInvocation{Dir: targetDir}, cwd.getwd, cwd.chdir)
	if err != nil {
		t.Fatalf("prepareBdShowExec: %v", err)
	}
	if want := filepath.Join(startDir, "bin", "bd"); got != want {
		t.Fatalf("bd path = %q, want %q", got, want)
	}
	if cwd.dir != targetDir {
		t.Fatalf("cwd = %q; want %q", cwd.dir, targetDir)
	}
}

func TestPrepareBdShowExecReturnsChdirError(t *testing.T) {
	t.Parallel()
	startDir := "/work/start"
	missingDir := filepath.Join(startDir, "missing")
	cwd := &fakeShowCwd{dir: startDir, missing: map[string]bool{missingDir: true}}

	_, err := prepareBdShowExec("bd", bdShowInvocation{Dir: missingDir}, cwd.getwd, cwd.chdir)
	if err == nil {
		t.Fatal("expected chdir error")
	}
	if !strings.Contains(err.Error(), "chdir "+missingDir) {
		t.Fatalf("error = %q, want chdir context for %q", err, missingDir)
	}
	if cwd.dir != startDir {
		t.Fatalf("cwd = %q; want %q", cwd.dir, startDir)
	}
}
