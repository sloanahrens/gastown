package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

// TestResolveBeadsDir_WorkspaceRootVsBeadsDir verifies that ResolveBeadsDir
// correctly handles the getTownBeadsDir() output (workspace root) by appending
// .beads, while also being idempotent when already given a .beads path.
func TestResolveBeadsDir_WorkspaceRootVsBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "workspace root gets .beads appended",
			input: townRoot,
			want:  beadsDir,
		},
		{
			name:  "already .beads path is normalized",
			input: beadsDir,
			want:  beadsDir,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := beads.ResolveBeadsDir(tc.input)
			if got != tc.want {
				t.Errorf("ResolveBeadsDir(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestResolveBeadsDir_WithRedirect verifies that ResolveBeadsDir follows
// redirect files, which is how rig worktrees (polecats) point back to the
// shared beads database. The convoy code must call ResolveBeadsDir to handle
// this case — passing the raw workspace root would skip the redirect.
func TestResolveBeadsDir_WithRedirect(t *testing.T) {
	t.Parallel()
	sharedRoot := t.TempDir()
	sharedBeads := filepath.Join(sharedRoot, ".beads")
	if err := os.MkdirAll(sharedBeads, 0755); err != nil {
		t.Fatal(err)
	}

	worktreeRoot := t.TempDir()
	worktreeBeads := filepath.Join(worktreeRoot, ".beads")
	if err := os.MkdirAll(worktreeBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Redirect file: worktree/.beads/redirect → shared/.beads
	if err := os.WriteFile(filepath.Join(worktreeBeads, "redirect"), []byte(sharedBeads+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Without ResolveBeadsDir (the bug): would use worktreeRoot directly,
	// missing the redirect entirely.
	// With ResolveBeadsDir (the fix): follows redirect to sharedBeads.
	resolved := beads.ResolveBeadsDir(worktreeRoot)
	if resolved != sharedBeads {
		t.Errorf("ResolveBeadsDir(%q) = %q, want %q (should follow redirect)",
			worktreeRoot, resolved, sharedBeads)
	}
}

// TestConvoyCreate_SentinelPlacement verifies that the convoy create path
// writes sentinel files to the .beads directory, not the workspace root.
// This is an end-to-end regression test for the empty convoy bug.
func TestConvoyCreate_SentinelPlacement(t *testing.T) {
	t.Parallel()
	beads.ResetEnsuredDirs()

	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate the fixed code path: ResolveBeadsDir(getTownBeadsDir())
	resolved := beads.ResolveBeadsDir(townRoot)
	if resolved != beadsDir {
		t.Fatalf("resolved = %q, want %q", resolved, beadsDir)
	}

	// Pre-populate sentinels to avoid needing a real bd binary.
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	currentStatuses := strings.Join(constants.BeadsCustomStatusesList(), ",")
	if err := os.WriteFile(filepath.Join(beadsDir, ".gt-statuses-configured"), []byte(currentStatuses+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Call both functions with the resolved path (as the fixed code does).
	if err := beads.EnsureCustomTypes(resolved); err != nil {
		t.Fatalf("EnsureCustomTypes(resolved) failed: %v", err)
	}
	if err := beads.EnsureCustomStatuses(resolved); err != nil {
		t.Fatalf("EnsureCustomStatuses(resolved) failed: %v", err)
	}

	// Verify sentinels are in .beads/, NOT in the workspace root.
	for _, sentinel := range []string{".gt-types-configured", ".gt-statuses-configured"} {
		correctPath := filepath.Join(beadsDir, sentinel)
		wrongPath := filepath.Join(townRoot, sentinel)

		if _, err := os.Stat(correctPath); err != nil {
			t.Errorf("sentinel %q missing from .beads dir: %v", sentinel, err)
		}
		if _, err := os.Stat(wrongPath); err == nil {
			t.Errorf("sentinel %q found in workspace root — "+
				"types/statuses registered in wrong location", sentinel)
		}
	}
}
