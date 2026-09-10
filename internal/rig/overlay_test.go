package rig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyOverlay_NoOverlayDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := t.TempDir()

	// No overlay directory exists
	err := CopyOverlay(tmpDir, destDir)
	if err != nil {
		t.Errorf("CopyOverlay() with no overlay directory should return nil, got %v", err)
	}
}

func TestCopyOverlay_CopiesFiles(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with test files
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Create test files
	testFile1 := filepath.Join(overlayDir, "test1.txt")
	testFile2 := filepath.Join(overlayDir, "test2.txt")

	if err := os.WriteFile(testFile1, []byte("content1"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	if err := os.WriteFile(testFile2, []byte("content2"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify files were copied
	destFile1 := filepath.Join(destDir, "test1.txt")
	destFile2 := filepath.Join(destDir, "test2.txt")

	content1, err := os.ReadFile(destFile1)
	if err != nil {
		t.Errorf("File test1.txt was not copied: %v", err)
	}
	if string(content1) != "content1" {
		t.Errorf("test1.txt content = %q, want %q", string(content1), "content1")
	}

	content2, err := os.ReadFile(destFile2)
	if err != nil {
		t.Errorf("File test2.txt was not copied: %v", err)
	}
	if string(content2) != "content2" {
		t.Errorf("test2.txt content = %q, want %q", string(content2), "content2")
	}
}

func TestCopyOverlay_PreservesPermissions(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with a file
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0755); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify permissions were preserved
	srcInfo, _ := os.Stat(testFile)
	destInfo, err := os.Stat(filepath.Join(destDir, "test.txt"))
	if err != nil {
		t.Fatalf("Failed to stat destination file: %v", err)
	}

	if srcInfo.Mode().Perm() != destInfo.Mode().Perm() {
		t.Errorf("Permissions not preserved: src=%v, dest=%v", srcInfo.Mode(), destInfo.Mode())
	}
}

func TestCopyOverlay_SkipsSubdirectories(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with a subdirectory
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Create a subdirectory
	subDir := filepath.Join(overlayDir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	// Create a file in the overlay root
	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a file in the subdirectory
	subFile := filepath.Join(subDir, "sub.txt")
	if err := os.WriteFile(subFile, []byte("subcontent"), 0644); err != nil {
		t.Fatalf("Failed to create sub file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify root file was copied
	if _, err := os.Stat(filepath.Join(destDir, "test.txt")); err != nil {
		t.Error("Root file should be copied")
	}

	// Verify subdirectory was NOT copied
	if _, err := os.Stat(filepath.Join(destDir, "subdir")); err == nil {
		t.Error("Subdirectory should not be copied")
	}
	if _, err := os.Stat(filepath.Join(destDir, "subdir", "sub.txt")); err == nil {
		t.Error("File in subdirectory should not be copied")
	}
}

func TestCopyOverlay_EmptyOverlay(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create empty overlay directory
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Should succeed without errors
}

func TestCopyOverlay_OverwritesExisting(t *testing.T) {
	rigDir := t.TempDir()
	destDir := t.TempDir()

	// Create overlay directory with test file
	overlayDir := filepath.Join(rigDir, ".runtime", "overlay")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatalf("Failed to create overlay dir: %v", err)
	}

	testFile := filepath.Join(overlayDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("new content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create existing file in destination with different content
	destFile := filepath.Join(destDir, "test.txt")
	if err := os.WriteFile(destFile, []byte("old content"), 0644); err != nil {
		t.Fatalf("Failed to create dest file: %v", err)
	}

	// Copy overlay
	err := CopyOverlay(rigDir, destDir)
	if err != nil {
		t.Fatalf("CopyOverlay() error = %v", err)
	}

	// Verify file was overwritten
	content, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("Failed to read dest file: %v", err)
	}
	if string(content) != "new content" {
		t.Errorf("File content = %q, want %q", string(content), "new content")
	}
}

func TestCopyFilePreserveMode(t *testing.T) {
	tmpDir := t.TempDir()

	// Create source file
	srcFile := filepath.Join(tmpDir, "src.txt")
	if err := os.WriteFile(srcFile, []byte("test content"), 0644); err != nil {
		t.Fatalf("Failed to create src file: %v", err)
	}

	// Copy file
	dstFile := filepath.Join(tmpDir, "dst.txt")
	err := copyFilePreserveMode(srcFile, dstFile)
	if err != nil {
		t.Fatalf("copyFilePreserveMode() error = %v", err)
	}

	// Verify content
	content, err := os.ReadFile(dstFile)
	if err != nil {
		t.Errorf("Failed to read dst file: %v", err)
	}
	if string(content) != "test content" {
		t.Errorf("Content = %q, want %q", string(content), "test content")
	}

	// Verify permissions
	srcInfo, _ := os.Stat(srcFile)
	dstInfo, err := os.Stat(dstFile)
	if err != nil {
		t.Fatalf("Failed to stat dst file: %v", err)
	}
	if srcInfo.Mode().Perm() != dstInfo.Mode().Perm() {
		t.Errorf("Permissions not preserved: src=%v, dest=%v", srcInfo.Mode(), dstInfo.Mode())
	}
}

func TestCopyFilePreserveMode_NonexistentSource(t *testing.T) {
	tmpDir := t.TempDir()

	srcFile := filepath.Join(tmpDir, "nonexistent.txt")
	dstFile := filepath.Join(tmpDir, "dst.txt")

	err := copyFilePreserveMode(srcFile, dstFile)
	if err == nil {
		t.Error("copyFilePreserveMode() with nonexistent source should return error")
	}
}

func TestEnsureGitignorePatterns_CreatesNewFile(t *testing.T) {
	tmpDir := t.TempDir()

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Check all required patterns are present (.beads/ intentionally excluded — see overlay.go)
	patterns := []string{".runtime/", ".claude/", ".opencode/", ".logs/", "__pycache__/", "state.json"}
	for _, pattern := range patterns {
		if !containsLine(string(content), pattern) {
			t.Errorf(".gitignore missing pattern %q", pattern)
		}
	}
}

func TestEnsureGitignorePatterns_AppendsToExisting(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with some content
	existing := "node_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should preserve existing content
	if !containsLine(string(content), "node_modules/") {
		t.Error("Existing pattern node_modules/ was removed")
	}

	// Should add header
	if !containsLine(string(content), "# Gas Town (added by gt)") {
		t.Error("Missing Gas Town header comment")
	}

	// Should add required patterns (.beads/ intentionally excluded — see overlay.go)
	patterns := []string{".runtime/", ".claude/", ".opencode/", ".logs/", "__pycache__/", "state.json"}
	for _, pattern := range patterns {
		if !containsLine(string(content), pattern) {
			t.Errorf(".gitignore missing pattern %q", pattern)
		}
	}
}

func TestEnsureGitignorePatterns_SkipsExistingPatterns(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with some Gas Town patterns already.
	// The broader ".claude/" covers ".claude/commands/", so it should
	// not add the narrower pattern.
	existing := ".runtime/\n.claude/\n.opencode/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should not duplicate existing patterns
	count := countOccurrences(string(content), ".runtime/")
	if count != 1 {
		t.Errorf(".runtime/ appears %d times, expected 1", count)
	}

	// .claude/ is now a direct required pattern — should not be duplicated
	claudeCount := countOccurrences(string(content), ".claude/")
	if claudeCount != 1 {
		t.Errorf(".claude/ appears %d times, expected 1", claudeCount)
	}
	opencodeCount := countOccurrences(string(content), ".opencode/")
	if opencodeCount != 1 {
		t.Errorf(".opencode/ appears %d times, expected 1", opencodeCount)
	}

	// Should add missing patterns
	if !containsLine(string(content), ".logs/") {
		t.Error(".gitignore missing pattern .logs/")
	}
	if !containsLine(string(content), "__pycache__/") {
		t.Error(".gitignore missing pattern __pycache__/")
	}
	if !containsLine(string(content), "state.json") {
		t.Error(".gitignore missing pattern state.json")
	}

	// Regression guard: .beads/ must NOT be in required patterns.
	// Beads manages its own .beads/.gitignore via bd init.
	// Adding .beads/ here breaks bd sync. This has regressed twice
	// (PR #753, #966). If this test fails, you're about to break polecats.
	if containsLine(string(content), ".beads/") {
		t.Error(".gitignore must NOT contain .beads/ - beads manages its own .gitignore (see overlay.go comment)")
	}
}

func TestEnsureGitignorePatterns_RecognizesVariants(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with variant patterns (without trailing slash).
	// ".claude" (no trailing slash) should be recognized as covering ".claude/commands/".
	existing := ".runtime\n/.claude\n/.opencode\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// Should recognize variants and not add duplicates
	// .runtime (no slash) should count as .runtime/
	runtimeCount := countOccurrences(string(content), ".runtime")
	if runtimeCount > 1 {
		t.Errorf(".runtime appears %d times (variant detection failed)", runtimeCount)
	}

	// /.claude (leading slash, no trailing slash) should cover .claude/
	if containsLine(string(content), ".claude/") {
		t.Error(".claude/ should not be added when /.claude already covers it")
	}
	if containsLine(string(content), ".opencode/") {
		t.Error(".opencode/ should not be added when /.opencode already covers it")
	}
}

func TestEnsureGitignorePatterns_AllPatternsPresent(t *testing.T) {
	tmpDir := t.TempDir()

	// Create existing .gitignore with all required patterns.
	existing := ".runtime/\n.claude/\n.opencode/\n.beads/\n.logs/\n__pycache__/\nstate.json\nCLAUDE.md\nCLAUDE.local.md\nGEMINI.md\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// File should be unchanged (no header added)
	if containsLine(string(content), "# Gas Town") {
		t.Error("Should not add header when all patterns already present")
	}

	// Content should match original
	if string(content) != existing {
		t.Errorf("File was modified when it shouldn't be.\nGot: %q\nWant: %q", string(content), existing)
	}
}

func TestEnsureGitignorePatterns_NarrowPatternPresent(t *testing.T) {
	tmpDir := t.TempDir()

	// Create .gitignore with the exact required patterns
	existing := ".runtime/\n.claude/\n.opencode/\n.logs/\n__pycache__/\nstate.json\nCLAUDE.md\nCLAUDE.local.md\nGEMINI.md\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// File should be unchanged
	if string(content) != existing {
		t.Errorf("File was modified when it shouldn't be.\nGot: %q\nWant: %q", string(content), existing)
	}
}

func TestEnsureGitignorePatterns_OldNarrowClaudeUpgraded(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate old installation with narrow .claude/commands/ pattern.
	// After upgrade, .claude/ (broad) should be added since .claude/commands/
	// does NOT cover .claude/ (the narrow is a subset, not a superset).
	existing := ".runtime/\n.claude/commands/\n.logs/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// .claude/ should be added (old .claude/commands/ doesn't cover it)
	if !containsLine(string(content), ".claude/") {
		t.Error(".claude/ should be added when only .claude/commands/ was present")
	}

	// __pycache__/ should be added
	if !containsLine(string(content), "__pycache__/") {
		t.Error("__pycache__/ should be added")
	}
}

func TestEnsureGitignorePatterns_UpgradePreservesBroadPattern(t *testing.T) {
	tmpDir := t.TempDir()

	// Simulate an existing installation that has .claude/ plus other Gas Town
	// patterns but is missing __pycache__/ (added later). After upgrade,
	// __pycache__/ should be appended.
	existing := "# Gas Town (added by gt)\n.runtime/\n.claude/\n.logs/\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(existing), 0644); err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	err := EnsureGitignorePatterns(tmpDir)
	if err != nil {
		t.Fatalf("EnsureGitignorePatterns() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, ".gitignore"))
	if err != nil {
		t.Fatalf("Failed to read .gitignore: %v", err)
	}

	// __pycache__/ should be appended
	if !containsLine(string(content), "__pycache__/") {
		t.Error("__pycache__/ should be added during upgrade")
	}

	// Existing patterns should be preserved
	if !containsLine(string(content), ".runtime/") {
		t.Error(".runtime/ should be preserved")
	}
	if !containsLine(string(content), ".claude/") {
		t.Error(".claude/ should be preserved")
	}
}

// TestGasTownLocalExcludePatterns_IncludesBeads verifies that the local exclude
// patterns include .beads/ (defense-in-depth for gas-7vg) while the gitignore
// patterns do NOT include .beads/ (regression guard).
func TestGasTownLocalExcludePatterns_IncludesBeads(t *testing.T) {
	localPatterns := gasTownLocalExcludePatterns()
	found := false
	for _, p := range localPatterns {
		if p == ".beads/" {
			found = true
			break
		}
	}
	if !found {
		t.Error("gasTownLocalExcludePatterns() must include .beads/ (gas-7vg defense-in-depth)")
	}

	// Regression guard: .gitignore patterns must NOT include .beads/
	gitignorePatterns := gasTownIgnorePatterns()
	for _, p := range gitignorePatterns {
		if p == ".beads/" {
			t.Error("gasTownIgnorePatterns() must NOT include .beads/ - that breaks bd sync (see overlay.go)")
		}
	}
}

// TestEnsureLocalExcludePatterns_LinkedWorktreeUsesCommonDir is a regression
// guard for gt-cqy8: a linked worktree's --git-dir is the worktree-private
// directory (.git/worktrees/<name>), and info/exclude written there is never
// read by git. The patterns must land in the shared common dir's info/exclude
// instead, where `git status` in the worktree actually honors them.
func TestEnsureLocalExcludePatterns_LinkedWorktreeUsesCommonDir(t *testing.T) {
	mainRepo := t.TempDir()
	runGit(t, mainRepo, "init")
	runGit(t, mainRepo, "config", "user.email", "test@test.com")
	runGit(t, mainRepo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("hi"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, mainRepo, "add", ".")
	runGit(t, mainRepo, "commit", "-m", "initial")

	worktreePath := filepath.Join(t.TempDir(), "linked-worktree")
	runGit(t, mainRepo, "worktree", "add", "-b", "feature", worktreePath, "HEAD")

	if err := EnsureLocalExcludePatterns(worktreePath); err != nil {
		t.Fatalf("EnsureLocalExcludePatterns() error = %v", err)
	}

	// The common dir's info/exclude (shared main-repo .git) must contain the
	// patterns - this is what `git status` in the worktree actually reads.
	commonExclude := filepath.Join(mainRepo, ".git", "info", "exclude")
	content, err := os.ReadFile(commonExclude)
	if err != nil {
		t.Fatalf("reading common-dir info/exclude: %v", err)
	}
	if !containsLine(string(content), ".claude/") {
		t.Errorf("common-dir info/exclude missing .claude/, got:\n%s", content)
	}

	// The worktree-private git dir (.git/worktrees/<name>/info/exclude) must
	// NOT be where patterns were written - that file is invisible to git.
	privateExclude := filepath.Join(mainRepo, ".git", "worktrees", "linked-worktree", "info", "exclude")
	if _, err := os.Stat(privateExclude); err == nil {
		t.Errorf("patterns must not be written to worktree-private exclude %s", privateExclude)
	}

	// Verify git itself now considers .claude/ ignored in the worktree.
	// check-ignore exits non-zero (which runGit turns into a fatal error) if
	// the path isn't actually ignored.
	runGit(t, worktreePath, "check-ignore", "-q", ".claude/marker")
}

// TestEnsureLocalExcludePatterns_RefusesTownRootWalkUp is a regression guard
// for gt-cqy8's rejected first fix: if worktreePath itself isn't a proper git
// working tree, plain `git rev-parse` discovery walks up parent directories
// and can land on an enclosing repository - in the real deployment, the Gas
// Town root. Writing there would pollute a repo far outside the intended
// target. EnsureLocalExcludePatterns must refuse instead of silently writing
// to the wrong repo.
func TestEnsureLocalExcludePatterns_RefusesTownRootWalkUp(t *testing.T) {
	townRoot := t.TempDir()
	runGit(t, townRoot, "init")
	runGit(t, townRoot, "config", "user.email", "test@test.com")
	runGit(t, townRoot, "config", "user.name", "Test User")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	runGit(t, townRoot, "add", ".")
	runGit(t, townRoot, "commit", "-m", "initial")

	// A plain subdirectory with no .git of its own - discovery would walk up
	// to townRoot's .git if not guarded.
	notAWorktree := filepath.Join(townRoot, "gastown", "polecats", "ghost")
	if err := os.MkdirAll(notAWorktree, 0755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	err := EnsureLocalExcludePatterns(notAWorktree)
	if err == nil {
		t.Fatal("expected EnsureLocalExcludePatterns() to refuse writing into the town root, got nil error")
	}

	content, readErr := os.ReadFile(filepath.Join(townRoot, ".git", "info", "exclude"))
	if readErr == nil && containsLine(string(content), ".claude/") {
		t.Errorf("patterns leaked into town root's info/exclude:\n%s", content)
	}
}

// Helper functions

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func containsLine(content, pattern string) bool {
	for _, line := range splitLines(content) {
		if line == pattern {
			return true
		}
	}
	return false
}

func countOccurrences(content, pattern string) int {
	count := 0
	for _, line := range splitLines(content) {
		if line == pattern {
			count++
		}
	}
	return count
}

func splitLines(content string) []string {
	var lines []string
	start := 0
	for i, c := range content {
		if c == '\n' {
			lines = append(lines, content[start:i])
			start = i + 1
		}
	}
	if start < len(content) {
		lines = append(lines, content[start:])
	}
	return lines
}
