//go:build integration

package done

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/steveyegge/gastown/internal/git"
)

// The git operations under gt done's auto-save safety net and submodule
// push, against real repositories.

// TestAutoCommitSafetyNet verifies that the gt done auto-commit safety net
// (gt-pvx) correctly detects uncommitted implementation work and auto-commits it.
// This tests the git-level operations that underpin the safety net in done.go.
func TestIntegrationAutoCommitSafetyNet(t *testing.T) {
	t.Parallel()
	// Set up a git repo with uncommitted changes
	dir := t.TempDir()
	testRunGit(t, dir, "init")
	testRunGit(t, dir, "config", "user.email", "test@test.com")
	testRunGit(t, dir, "config", "user.name", "Test")

	// Create initial commit
	initialFile := filepath.Join(dir, "README.md")
	if err := os.WriteFile(initialFile, []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, dir, "add", "README.md")
	testRunGit(t, dir, "commit", "-m", "initial commit")

	g := gitpkg.NewGit(dir)

	t.Run("detects uncommitted new files", func(t *testing.T) {
		// Create uncommitted implementation files (simulates polecat forgetting to commit)
		implFile := filepath.Join(dir, "main.go")
		if err := os.WriteFile(implFile, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(implFile)

		ws, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !ws.HasUncommittedChanges {
			t.Error("expected HasUncommittedChanges=true for new file")
		}
		if ws.CleanExcludingRuntime() {
			t.Error("expected CleanExcludingRuntime=false for non-runtime file")
		}
	})

	t.Run("auto-commit preserves work", func(t *testing.T) {
		// Create implementation files
		implFile := filepath.Join(dir, "handler.go")
		if err := os.WriteFile(implFile, []byte("package main\n\nfunc handler() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}

		// Verify uncommitted
		ws, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !ws.HasUncommittedChanges || ws.CleanExcludingRuntime() {
			t.Fatal("expected non-runtime uncommitted changes")
		}

		// Simulate the auto-commit safety net
		if err := g.Add("-A"); err != nil {
			t.Fatalf("git add: %v", err)
		}
		if err := g.Commit("fix: auto-save uncommitted implementation work (gt-pvx safety net)"); err != nil {
			t.Fatalf("git commit: %v", err)
		}

		// Verify clean after auto-commit
		ws2, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork after commit: %v", err)
		}
		if ws2.HasUncommittedChanges {
			t.Error("expected clean working tree after auto-commit")
		}
	})

	t.Run("runtime-only changes skip auto-commit", func(t *testing.T) {
		// Runtime artifacts should NOT trigger auto-commit
		runtimeDir := filepath.Join(dir, ".claude")
		if err := os.MkdirAll(runtimeDir, 0755); err != nil {
			t.Fatal(err)
		}
		runtimeFile := filepath.Join(runtimeDir, "settings.json")
		if err := os.WriteFile(runtimeFile, []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(runtimeDir)

		ws, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		// HasUncommittedChanges is true (git sees the files), but CleanExcludingRuntime
		// should be true (only runtime artifacts)
		if ws.HasUncommittedChanges && !ws.CleanExcludingRuntime() {
			t.Error("runtime-only changes should be considered clean excluding runtime")
		}
	})

	t.Run("auto-commit excludes runtime artifacts recursively", func(t *testing.T) {
		repo := t.TempDir()
		testRunGit(t, repo, "init")
		testRunGit(t, repo, "config", "user.email", "test@test.com")
		testRunGit(t, repo, "config", "user.name", "Test")
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Test\n"), 0644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, repo, "add", "README.md")
		testRunGit(t, repo, "commit", "-m", "initial commit")

		writeFile := func(path, content string) {
			t.Helper()
			fullPath := filepath.Join(repo, path)
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}

		writeFile("src/handler.go", "package main\n\nfunc handler() {}\n")
		writeFile(".opencode/plugins/gastown.js", "// generated\n")
		writeFile("services/cyrus/workflow-cyrus-edge/node_modules/pkg/index.js", "module.exports = {}\n")
		writeFile("dashboard/public/meridian-dashboard/.vite/vitest/hash/results.json", "{}\n")
		writeFile("services/workflows/collateral-internal/execution_log.db", "sqlite\n")
		writeFile("api/.pytest_cache/v/cache/nodeids", "[]\n")
		writeFile("src/__pycache__/handler.cpython-312.pyc", "pyc\n")
		writeFile(".beads/.runtime/state.json", "{}\n")

		g := gitpkg.NewGit(repo)
		ws, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork: %v", err)
		}
		if !ws.HasUncommittedChanges || ws.CleanExcludingRuntime() {
			t.Fatal("expected mixed source and runtime changes")
		}

		if err := g.Add("-A"); err != nil {
			t.Fatalf("git add: %v", err)
		}
		if runtimePaths := ws.RuntimeArtifactPaths(); len(runtimePaths) > 0 {
			if err := g.ResetFiles(runtimePaths...); err != nil {
				t.Fatalf("reset runtime artifacts: %v", err)
			}
		}
		if err := g.Commit("fix: auto-save uncommitted implementation work (gt-pvx safety net)"); err != nil {
			t.Fatalf("git commit: %v", err)
		}

		changed, err := g.DiffNameOnly("HEAD~1", "HEAD")
		if err != nil {
			t.Fatalf("DiffNameOnly: %v", err)
		}
		if len(changed) != 1 || changed[0] != "src/handler.go" {
			t.Fatalf("auto-save committed %v, want only src/handler.go", changed)
		}

		wsAfter, err := g.CheckUncommittedWork()
		if err != nil {
			t.Fatalf("CheckUncommittedWork after commit: %v", err)
		}
		if !wsAfter.HasUncommittedChanges || !wsAfter.CleanExcludingRuntime() {
			t.Fatalf("runtime artifacts should remain uncommitted and clean-excluded, got %#v", wsAfter)
		}
	})
}

// TestPushSubmoduleChanges_Integration verifies that pushSubmoduleChanges detects
// modified submodules and pushes their commits before the parent repo push (gt-dzs).
func TestIntegrationPushSubmoduleChanges(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// testRunGit allows the file transport for the submodule clone; the push
	// pushSubmoduleChanges makes is a plain push, which git allows over it.

	// Create a "remote" bare repo for the submodule
	subRemote := filepath.Join(tmp, "sub-remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", subRemote)

	// Create a working clone of the submodule to add initial content
	subWork := filepath.Join(tmp, "sub-work")
	testRunGit(t, tmp, "clone", subRemote, subWork)
	testRunGit(t, subWork, "config", "user.email", "test@test.com")
	testRunGit(t, subWork, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(subWork, "lib.go"), []byte("package lib\n"), 0644); err != nil {
		t.Fatalf("write sub file: %v", err)
	}
	testRunGit(t, subWork, "add", ".")
	testRunGit(t, subWork, "commit", "-m", "initial sub commit")
	testRunGit(t, subWork, "push", "origin", "main")

	// Create a "remote" bare repo for the parent
	parentRemote := filepath.Join(tmp, "parent-remote.git")
	testRunGit(t, tmp, "init", "--bare", "--initial-branch", "main", parentRemote)

	// Create the parent repo
	parent := filepath.Join(tmp, "parent")
	testRunGit(t, tmp, "init", "--initial-branch", "main", parent)
	testRunGit(t, parent, "config", "user.email", "test@test.com")
	testRunGit(t, parent, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(parent, "README.md"), []byte("# Parent\n"), 0644); err != nil {
		t.Fatalf("write parent file: %v", err)
	}
	testRunGit(t, parent, "add", ".")
	testRunGit(t, parent, "commit", "-m", "initial parent commit")

	// Add the submodule
	testRunGit(t, parent, "submodule", "add", subRemote, "libs/sub")
	testRunGit(t, parent, "commit", "-m", "add submodule")

	// Add remote and push to parent remote
	testRunGit(t, parent, "remote", "add", "origin", parentRemote)
	testRunGit(t, parent, "push", "origin", "main")

	// Make a new commit in the submodule (but don't push it to submodule remote)
	subPath := filepath.Join(parent, "libs", "sub")
	if err := os.WriteFile(filepath.Join(subPath, "new.go"), []byte("package lib\n// new\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	testRunGit(t, subPath, "add", ".")
	testRunGit(t, subPath, "commit", "-m", "unpushed submodule commit")

	// Get the new submodule SHA
	cmd := exec.Command("git", "-C", subPath, "rev-parse", "HEAD")
	shaBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	newSHA := strings.TrimSpace(string(shaBytes))

	// Update parent to point to new submodule commit
	testRunGit(t, parent, "add", "libs/sub")
	testRunGit(t, parent, "commit", "-m", "update submodule pointer")

	// Verify the new submodule commit is NOT on the submodule remote yet
	lsCmd := exec.Command("git", "ls-remote", subRemote, "refs/heads/main")
	lsOut, _ := lsCmd.Output()
	remoteSHA := strings.Fields(string(lsOut))[0]
	if remoteSHA == newSHA {
		t.Fatal("new submodule commit should not be on remote yet")
	}

	// Call pushSubmoduleChanges — this should push the submodule commit
	g := gitpkg.NewGit(parent)
	pushSubmoduleChanges(g, "origin/main")

	// Verify the submodule commit IS now on the remote
	lsCmd = exec.Command("git", "ls-remote", subRemote, "refs/heads/main")
	lsOut, _ = lsCmd.Output()
	remoteSHA = strings.Fields(string(lsOut))[0]
	if remoteSHA != newSHA {
		t.Errorf("expected submodule remote main to be %s, got %s", newSHA, remoteSHA)
	}
}
