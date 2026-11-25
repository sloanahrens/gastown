//go:build integration

package rig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestIntegrationEnsureLocalExcludePatternsWritesLocalExcludePatterns drives
// the exported entry point itself, which the unit tier cannot: it opens a real
// git, and that tier refuses git. What it writes into a fresh worktree's
// info/exclude is exactly LocalExcludePatterns(), the list the sandbox seat
// clone reads (gt-f57pof).
func TestIntegrationEnsureLocalExcludePatternsWritesLocalExcludePatterns(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	gitT(t, worktree, "init", "-b", "main")

	if err := EnsureLocalExcludePatterns(worktree); err != nil {
		t.Fatalf("EnsureLocalExcludePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(worktree, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("reading info/exclude: %v", err)
	}
	var written []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		written = append(written, line)
	}
	if !slices.Equal(written, LocalExcludePatterns()) {
		t.Errorf("info/exclude patterns = %v, LocalExcludePatterns() = %v", written, LocalExcludePatterns())
	}
}
