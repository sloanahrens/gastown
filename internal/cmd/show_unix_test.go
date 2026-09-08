//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareBdShowExecAnchorsRelativePathBeforeChdir(t *testing.T) {
	startDir := filepath.Join(t.TempDir(), "start")
	targetDir := filepath.Join(t.TempDir(), "target")
	for _, dir := range []string{startDir, targetDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// os.Getwd() after os.Chdir returns the kernel-resolved path on macOS
	// (where the temp dir is a /private symlink) — resolve upfront so
	// comparisons below agree with what prepareBdShowExec actually sees.
	if resolved, err := filepath.EvalSymlinks(startDir); err == nil {
		startDir = resolved
	}
	if resolved, err := filepath.EvalSymlinks(targetDir); err == nil {
		targetDir = resolved
	}

	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	if err := os.Chdir(startDir); err != nil {
		t.Fatalf("chdir start: %v", err)
	}

	got, err := prepareBdShowExec(filepath.Join("bin", "bd"), bdShowInvocation{Dir: targetDir})
	if err != nil {
		t.Fatalf("prepareBdShowExec: %v", err)
	}
	if want := filepath.Join(startDir, "bin", "bd"); got != want {
		t.Fatalf("bd path = %q, want %q", got, want)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != targetDir {
		t.Fatalf("cwd = %q, %v; want %q", cwd, err, targetDir)
	}
}

func TestPrepareBdShowExecReturnsChdirError(t *testing.T) {
	startDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })
	if err := os.Chdir(startDir); err != nil {
		t.Fatalf("chdir start: %v", err)
	}

	missingDir := filepath.Join(startDir, "missing")
	_, err = prepareBdShowExec("bd", bdShowInvocation{Dir: missingDir})
	if err == nil {
		t.Fatal("expected chdir error")
	}
	if !strings.Contains(err.Error(), "chdir "+missingDir) {
		t.Fatalf("error = %q, want chdir context for %q", err, missingDir)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != startDir {
		t.Fatalf("cwd = %q, %v; want %q", cwd, err, startDir)
	}
}
