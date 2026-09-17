package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFakePolecatTown builds a minimal on-disk town tree at town, mirroring
// the real layout so the path classifier can be exercised against a real
// structure.
func makeFakePolecatTown(t *testing.T, town string) string {
	t.Helper()
	for _, d := range []string{
		"mayor",
		"logs",
		".dolt-data",
		filepath.Join(fakeRigName, "polecats", "opal", "gastown", "internal"),
		filepath.Join(fakeRigName, "polecats", "onyx", "gastown"),
		filepath.Join(fakeRigName, "crew"),
		filepath.Join(fakeRigName, "refinery", "rig"),
		filepath.Join(fakeRigName, "witness"),
		filepath.Join(fakeRigName, "mayor", "rig"),
		filepath.Join(fakeRigName, "settings"),
		filepath.Join(fakeRigName, ".repo.git"),
	} {
		if err := os.MkdirAll(filepath.Join(town, d), 0o755); err != nil {
			t.Fatalf("building fake town: %v", err)
		}
	}
	return town
}

func fakePolecatTown(t *testing.T) (town, rig, worktree string) {
	town = filepath.Join(t.TempDir(), "gt")
	makeFakePolecatTown(t, town)
	rig = filepath.Join(town, fakeRigName)
	worktree = filepath.Join(rig, "polecats", "opal", "gastown")
	return
}

// TestEditWriteBlocksOutsideWorktree verifies that Edit and Write operations
// targeting paths outside the polecat's own worktree are blocked, while those
// within the worktree are allowed.
func TestEditWriteBlocksOutsideWorktree(t *testing.T) {
	town, rig, worktree := fakePolecatTown(t)

	t.Setenv("GT_POLECAT", "opal")
	t.Setenv("GT_ROLE", "gastown/polecats/opal")

	tests := []struct {
		name    string
		tool    string
		path    string
		blocked bool
	}{
		// Blocked — sibling worktree.
		{"Edit sibling worktree", "Edit", filepath.Join(rig, "polecats", "onyx", "gastown", "main.go"), true},
		{"Write sibling worktree", "Write", filepath.Join(rig, "polecats", "onyx", "gastown", "main.go"), true},

		// Blocked — rig/town root.
		{"Edit rig root", "Edit", filepath.Join(rig, "internal", "cmd", "gt.go"), true},
		{"Write rig root", "Write", filepath.Join(rig, "internal", "cmd", "gt.go"), true},

		// Blocked — town-level directories.
		{"Edit mayor", "Edit", filepath.Join(rig, "mayor", "town.json"), true},
		{"Write deacon", "Write", filepath.Join(rig, "deacon", "dogs", "spot.md"), true},
		{"Edit settings", "Edit", filepath.Join(rig, "settings", "config.json"), true},
		{"Write logs", "Write", filepath.Join(rig, "logs", "app.log"), true},

		// Allowed — within own worktree.
		{"Edit own worktree", "Edit", filepath.Join(worktree, "internal", "cmd", "guard.go"), false},
		{"Write own worktree", "Write", filepath.Join(worktree, "internal", "cmd", "guard.go"), false},

		// Allowed — outside the town.
		{"Edit outside town", "Edit", filepath.Join(town, "other-project", "main.go"), false},
		{"Write /tmp", "Write", "/tmp/some-file.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"tool_name":"%s","tool_input":{"file_path":"%s"}}`, tt.tool, tt.path)

			var guardErr error
			stderr := captureStderr(t, func() {
				withStdin(t, input, func() {
					guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
				})
			})

			code, isSilent := IsSilentExit(guardErr)
			if tt.blocked {
				if guardErr == nil {
					t.Errorf("%s(%q) was allowed, expected blocked; stderr: %s", tt.tool, tt.path, stderr)
				} else if !isSilent || code != 2 {
					t.Errorf("%s(%q) blocked with err=%v (silent=%v code=%d), want silent exit 2; stderr: %s",
						tt.tool, tt.path, guardErr, isSilent, code, stderr)
				}
				if !strings.Contains(stderr, "CROSS-WORKTREE EDIT BLOCKED") {
					t.Errorf("%s(%q) blocked without CROSS-WORKTREE banner; stderr: %s",
						tt.tool, tt.path, stderr)
				}
			} else {
				if guardErr != nil {
					t.Errorf("%s(%q) was blocked (%v, silent=%v), expected allowed; stderr: %s",
						tt.tool, tt.path, guardErr, isSilent, stderr)
				}
			}
		})
	}
}

// TestNotebookEditBlocksOutsideWorktree verifies that NotebookEdit operations
// targeting paths outside the polecat's own worktree are blocked.
func TestNotebookEditBlocksOutsideWorktree(t *testing.T) {
	_, rig, worktree := fakePolecatTown(t)

	t.Setenv("GT_POLECAT", "opal")
	t.Setenv("GT_ROLE", "gastown/polecats/opal")

	var guardErr error
	stderr := captureStderr(t, func() {
		input := `{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"` + rig + `/notebook.ipynb"}}`
		withStdin(t, input, func() {
			guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
		})
	})

	code, isSilent := IsSilentExit(guardErr)
	if guardErr == nil {
		t.Error("NotebookEdit to rig root was allowed, expected blocked")
	} else if !isSilent || code != 2 {
		t.Errorf("NotebookEdit blocked with err=%v (silent=%v code=%d), want silent exit 2", guardErr, isSilent, code)
	}
	if !strings.Contains(stderr, "CROSS-WORKTREE EDIT BLOCKED") {
		t.Errorf("blocked without CROSS-WORKTREE banner; stderr: %s", stderr)
	}

	// NotebookEdit within worktree — allowed.
	guardErr = nil
	stderr = captureStderr(t, func() {
		input := `{"tool_name":"NotebookEdit","tool_input":{"notebook_path":"` + filepath.Join(worktree, "notebook.ipynb") + `"}}`
		withStdin(t, input, func() {
			guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
		})
	})

	if guardErr != nil {
		t.Errorf("NotebookEdit within worktree was blocked (%v), expected allowed", guardErr)
	}
	if strings.Contains(stderr, "CROSS-WORKTREE EDIT BLOCKED") {
		t.Errorf("NotebookEdit within worktree printed block banner; stderr: %s", stderr)
	}
}

// TestNonPolecatContextNotBlocked verifies that non-polecat contexts (e.g.
// crew) are not affected by this guard even when in the same town directory.
func TestNonPolecatContextNotBlocked(t *testing.T) {
	_, rig, _ := fakePolecatTown(t)

	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_ROLE", "gastown/crew")
	t.Chdir(filepath.Join(rig, "crew"))

	var guardErr error
	stderr := captureStderr(t, func() {
		input := `{"tool_name":"Edit","tool_input":{"file_path":"` + rig + `/internal/cmd/gt.go"}}`
		withStdin(t, input, func() {
			guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
		})
	})

	if guardErr != nil {
		t.Errorf("crew Edit to rig root was blocked (%v), expected allowed; stderr: %s", guardErr, stderr)
	}

	// Crew Bash targeting rig root should also be allowed.
	guardErr = nil
	stderr = captureStderr(t, func() {
		input := `{"tool_name":"Bash","tool_input":{"command":"python3 -c \"open('" + rig + "/x.py" + "', 'w').write('')\""}}`
		withStdin(t, input, func() {
			guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
		})
	})

	if guardErr != nil {
		t.Errorf("crew Bash python to rig root was blocked (%v), expected allowed; stderr: %s", guardErr, stderr)
	}
}

// TestBashBlocksWriteCapableTargets verifies that Bash commands targeting paths
// outside the worktree are blocked only for write-capable tools, while
// read-only tools are allowed everywhere.
func TestBashBlocksWriteCapableTargets(t *testing.T) {
	town, rig, worktree := fakePolecatTown(t)

	t.Setenv("GT_POLECAT", "opal")
	t.Setenv("GT_ROLE", "gastown/polecats/opal")

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — write-capable tools targeting outside the worktree.
		{"python to rig root", "python3 -c \"open('" + rig + "/x.py', 'w').write('')\"", true},
		{"node to rig root", "node -e \"require('fs').writeFileSync('" + rig + "/x.js', '')\"", true},
		{"sh to sibling", "sh -c \"echo x > '" + filepath.Join(rig, "polecats", "onyx", "gastown", "x.sh") + "\"", true},
		{"curl to rig root", "curl -o '" + rig + "/file' https://example.com/x", true},
		{"wget to mayor", "wget -O '" + filepath.Join(rig, "mayor", "town.json") + "' https://x", true},

		// Allowed — read-only tools anywhere.
		{"grep rig root", "grep -rn TODO " + rig, false},
		{"cat sibling", "cat " + filepath.Join(rig, "polecats", "onyx", "gastown", "x.go"), false},
		{"ls rig root", "ls -la " + rig, false},

		// Allowed — write-capable tools within the worktree.
		{"python own worktree", "python3 -c \"open('" + filepath.Join(worktree, "x.py") + "', 'w').write('')\"", false},

		// Allowed — outside the town.
		{"python outside town", "python3 -c \"open('" + filepath.Join(town, "other", "x.py") + "', 'w').write('')\"", false},
		{"curl outside town", "curl -o '" + filepath.Join(town, "other", "x") + "' https://example.com/x", false},

		// Allowed — paths with similar names outside the town.
		{"mkdir /tmp/gt/gastown", "mkdir -p /tmp/gt/gastown/polecats/opal", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := `{"tool_name":"Bash","tool_input":{"command":"` + tt.command + `"}}`

			var guardErr error
			stderr := captureStderr(t, func() {
				withStdin(t, input, func() {
					guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
				})
			})

			code, isSilent := IsSilentExit(guardErr)
			if tt.blocked {
				if guardErr == nil {
					t.Errorf("command %q was allowed, expected blocked; stderr: %s", tt.command, stderr)
				} else if !isSilent || code != 2 {
					t.Errorf("command %q blocked with err=%v (silent=%v code=%d), want silent exit 2; stderr: %s",
						tt.command, guardErr, isSilent, code, stderr)
				}
			} else {
				if guardErr != nil {
					t.Errorf("command %q was blocked (%v, silent=%v), expected allowed; stderr: %s",
						tt.command, guardErr, isSilent, stderr)
				}
			}
		})
	}
}
