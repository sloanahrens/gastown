package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// primingCheck is a PrimingCheck that finds gt on PATH.
func primingCheck() *PrimingCheck {
	c := NewPrimingCheck()
	c.lookPath = func(name string) (string, error) { return "/usr/local/bin/" + name, nil }
	return c
}

// A town whose PATH has no gt is told so, from the lookup alone.
func TestPrimingCheck_GTNotInPath(t *testing.T) {
	t.Parallel()
	check := NewPrimingCheck()
	var asked []string
	check.lookPath = func(name string) (string, error) {
		asked = append(asked, name)
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}

	result := check.Run(&CheckContext{TownRoot: t.TempDir()})

	if !slices.Equal(asked, []string{"gt"}) {
		t.Errorf("looked up %v, want [gt]", asked)
	}
	found := false
	for _, iss := range check.issues {
		if iss.issueType == "gt_not_in_path" && !iss.fixable {
			found = true
		}
	}
	if !found || !slices.Contains(result.Details, "gt binary not found in PATH") {
		t.Errorf("no unfixable gt_not_in_path issue: issues=%v details=%v", check.issues, result.Details)
	}
}

func TestPrimingCheck_PolecatNewStructure(t *testing.T) {
	t.Parallel()
	// This test verifies that priming check correctly handles the new polecat path structure.
	// Bug: priming_check.go looks at polecats/<name>/ but the actual worktree is at
	// polecats/<name>/<rigname>/ which is where the .beads/redirect file lives.
	//
	// Expected behavior: If polecats/<name>/<rigname>/.beads/redirect points to rig's beads
	// which has PRIME.md, the priming check should report no issues.
	//
	// Actual behavior with bug: Check looks at polecats/<name>/ which has no .beads/redirect,
	// so it incorrectly reports PRIME.MD as missing.

	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatName := "testpc"

	// Set up rig structure with .beads and PRIME.md
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	primeMdPath := filepath.Join(rigBeadsDir, "PRIME.md")
	if err := os.WriteFile(primeMdPath, []byte("# Test PRIME.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up polecat with NEW structure: polecats/<name>/<rigname>/
	polecatWorktree := filepath.Join(tmpDir, rigName, "polecats", polecatName, rigName)
	polecatWorktreeBeads := filepath.Join(polecatWorktree, ".beads")
	if err := os.MkdirAll(polecatWorktreeBeads, 0755); err != nil {
		t.Fatal(err)
	}

	// Create redirect file pointing to rig's beads (from worktree perspective)
	// From polecats/<name>/<rigname>/, we go up 3 levels to rig root: ../../../.beads
	redirectFile := filepath.Join(polecatWorktreeBeads, "redirect")
	if err := os.WriteFile(redirectFile, []byte("../../../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Look for any polecat-related issues
	polecatIssueFound := false
	for _, detail := range result.Details {
		// The bug would report: "testrig/polecats/testpc: Missing PRIME.md..."
		if filepath.Base(filepath.Dir(detail)) == "polecats" ||
			(len(detail) > 0 && detail[0:len(rigName)+len("/polecats/")] == rigName+"/polecats/") {
			polecatIssueFound = true
			t.Logf("Found polecat issue: %s", detail)
		}
	}

	// With the bug fixed, there should be NO polecat issues because:
	// - The worktree at polecats/<name>/<rigname>/ has .beads/redirect
	// - The redirect points to rig's .beads which has PRIME.md
	//
	// With the bug present, the check looks at polecats/<name>/ which has no redirect,
	// so it reports missing PRIME.md
	if polecatIssueFound {
		t.Errorf("priming check incorrectly reported polecat issues; result: %+v", result)
	}
}

func TestPrimingCheck_PolecatDirLevel_NoPrimeMD(t *testing.T) {
	t.Parallel()
	// This test verifies that NO PRIME.md should exist at the polecatDir level
	// (polecats/<name>/.beads/PRIME.md). PRIME.md should only exist at:
	// 1. Rig level: <rig>/.beads/PRIME.md
	// 2. Accessed via redirect from worktree: polecats/<name>/<rigname>/.beads/redirect -> rig's beads

	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatName := "testpc"

	// Set up rig with PRIME.md
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create the intermediate polecatDir level (polecats/<name>/)
	// This directory should NOT have .beads/PRIME.md
	polecatDir := filepath.Join(tmpDir, rigName, "polecats", polecatName)
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create the actual worktree with redirect
	polecatWorktree := filepath.Join(polecatDir, rigName)
	polecatWorktreeBeads := filepath.Join(polecatWorktree, ".beads")
	if err := os.MkdirAll(polecatWorktreeBeads, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatWorktreeBeads, "redirect"), []byte("../../../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify: polecatDir level should NOT have .beads/PRIME.md
	polecatDirPrimeMd := filepath.Join(polecatDir, ".beads", "PRIME.md")
	if _, err := os.Stat(polecatDirPrimeMd); err == nil {
		t.Errorf("PRIME.md should NOT exist at polecatDir level: %s", polecatDirPrimeMd)
	}

	// Also verify the doctor doesn't create one at the wrong level when fixing
	// (This would happen if doctor --fix incorrectly provisions PRIME.md at polecats/<name>/)
}

func TestPrimingCheck_FixRemovesBadPolecatBeads(t *testing.T) {
	t.Parallel()
	// This test verifies that doctor --fix removes spurious .beads directories
	// that were incorrectly created at the polecatDir level (polecats/<name>/.beads).
	//
	// Background: A bug in priming_check.go caused it to look at polecats/<name>/
	// instead of polecats/<name>/<rigname>/. When it didn't find a redirect, it
	// would create .beads/PRIME.md at the wrong level. These orphaned .beads
	// directories should be cleaned up.

	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatName := "testpc"

	// Set up rig with .beads and PRIME.md
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up correct polecat worktree structure: polecats/<name>/<rigname>/
	polecatDir := filepath.Join(tmpDir, rigName, "polecats", polecatName)
	polecatWorktree := filepath.Join(polecatDir, rigName)
	polecatWorktreeBeads := filepath.Join(polecatWorktree, ".beads")
	if err := os.MkdirAll(polecatWorktreeBeads, 0755); err != nil {
		t.Fatal(err)
	}
	// Create redirect pointing to rig's beads
	if err := os.WriteFile(filepath.Join(polecatWorktreeBeads, "redirect"), []byte("../../../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create BAD .beads at polecatDir level (this is what the bug created)
	badBeadsDir := filepath.Join(polecatDir, ".beads")
	if err := os.MkdirAll(badBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	badPrimeMd := filepath.Join(badBeadsDir, "PRIME.md")
	if err := os.WriteFile(badPrimeMd, []byte("# BAD PRIME - should be removed\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Verify bad .beads exists before fix
	if _, err := os.Stat(badBeadsDir); os.IsNotExist(err) {
		t.Fatal("test setup failed: bad .beads directory should exist")
	}

	// Run priming check and fix
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	_ = check.Run(ctx) // Populate issues

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}

	// Verify bad .beads was removed
	if _, err := os.Stat(badBeadsDir); err == nil {
		t.Errorf("bad .beads directory at polecatDir level should have been removed: %s", badBeadsDir)
		// List contents for debugging
		entries, _ := os.ReadDir(badBeadsDir)
		for _, e := range entries {
			t.Logf("  - %s", e.Name())
		}
	}

	// Verify correct structure still exists
	if _, err := os.Stat(polecatWorktreeBeads); os.IsNotExist(err) {
		t.Errorf("correct .beads at worktree level should still exist: %s", polecatWorktreeBeads)
	}
	if _, err := os.Stat(filepath.Join(polecatWorktreeBeads, "redirect")); os.IsNotExist(err) {
		t.Errorf("redirect file should still exist")
	}

	// Verify rig's PRIME.md still exists
	if _, err := os.Stat(filepath.Join(rigBeadsDir, "PRIME.md")); os.IsNotExist(err) {
		t.Errorf("rig's PRIME.md should still exist")
	}
}

// TestPrimingCheck_AllowsClaudeMdInMayorRig verifies that CLAUDE.md
// inside mayor/rig/ (the customer's source repo) is NOT flagged.
// With sparse checkout removed, this is the customer's legitimate file.
func TestPrimingCheck_AllowsClaudeMdInMayorRig(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create mayor/rig/ structure (the source repo clone)
	mayorRigPath := filepath.Join(tmpDir, rigName, "mayor", "rig")
	if err := os.MkdirAll(mayorRigPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create CLAUDE.md inside mayor/rig/ — customer's legitimate file
	customerClaudeMd := filepath.Join(mayorRigPath, "CLAUDE.md")
	if err := os.WriteFile(customerClaudeMd, []byte("# Customer CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Should NOT flag CLAUDE.md inside worktrees
	for _, detail := range result.Details {
		if strings.Contains(detail, "mayor/rig") && strings.Contains(detail, "CLAUDE.md") {
			t.Errorf("CLAUDE.md inside mayor/rig should NOT be flagged (customer file), got: %s", detail)
		}
	}
}

// TestPrimingCheck_AllowsClaudeMdInCrewWorktree verifies that CLAUDE.md
// inside crew/<name>/ (the customer's worktree) is NOT flagged.
func TestPrimingCheck_AllowsClaudeMdInCrewWorktree(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	crewName := "alice"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew/<name>/ structure with beads redirect
	crewWorktree := filepath.Join(tmpDir, rigName, "crew", crewName)
	crewBeadsDir := filepath.Join(crewWorktree, ".beads")
	if err := os.MkdirAll(crewBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewBeadsDir, "redirect"), []byte("../../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create CLAUDE.md inside crew worktree — customer's legitimate file
	customerClaudeMd := filepath.Join(crewWorktree, "CLAUDE.md")
	if err := os.WriteFile(customerClaudeMd, []byte("# Customer CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Should NOT flag CLAUDE.md inside worktrees
	for _, detail := range result.Details {
		if strings.Contains(detail, "crew/"+crewName) && strings.Contains(detail, "CLAUDE.md") {
			t.Errorf("CLAUDE.md inside crew/%s should NOT be flagged (customer file), got: %s", crewName, detail)
		}
	}
}

// TestPrimingCheck_AllowsClaudeMdInPolecatWorktree verifies that CLAUDE.md
// inside polecats/<name>/<rigname>/ (the customer's worktree) is NOT flagged.
func TestPrimingCheck_AllowsClaudeMdInPolecatWorktree(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"
	polecatName := "testpc"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create polecat worktree structure: polecats/<name>/<rigname>/
	polecatWorktree := filepath.Join(tmpDir, rigName, "polecats", polecatName, rigName)
	polecatBeadsDir := filepath.Join(polecatWorktree, ".beads")
	if err := os.MkdirAll(polecatBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(polecatBeadsDir, "redirect"), []byte("../../../.beads\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create CLAUDE.md inside polecat worktree — customer's legitimate file
	customerClaudeMd := filepath.Join(polecatWorktree, "CLAUDE.md")
	if err := os.WriteFile(customerClaudeMd, []byte("# Customer CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Should NOT flag CLAUDE.md inside worktrees
	for _, detail := range result.Details {
		if strings.Contains(detail, "polecats/"+polecatName) && strings.Contains(detail, "CLAUDE.md") {
			t.Errorf("CLAUDE.md inside polecats/%s should NOT be flagged (customer file), got: %s", polecatName, detail)
		}
	}
}

// TestPrimingCheck_FixPreservesCustomerClaudeMd verifies that doctor --fix
// does NOT delete CLAUDE.md files from inside source repo worktrees.
// With sparse checkout removed, these are the customer's legitimate files.
func TestPrimingCheck_FixPreservesCustomerClaudeMd(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create mayor/rig/ with customer's CLAUDE.md
	mayorRigPath := filepath.Join(tmpDir, rigName, "mayor", "rig")
	if err := os.MkdirAll(mayorRigPath, 0755); err != nil {
		t.Fatal(err)
	}
	customerClaudeMd := filepath.Join(mayorRigPath, "CLAUDE.md")
	if err := os.WriteFile(customerClaudeMd, []byte("# Customer CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check and fix
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	_ = check.Run(ctx)

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}

	// Verify customer's CLAUDE.md was NOT removed
	if _, err := os.Stat(customerClaudeMd); os.IsNotExist(err) {
		t.Errorf("customer's CLAUDE.md should NOT have been removed: %s", customerClaudeMd)
	}
}

// TestPrimingCheck_FlagsStaleAgentLevelFiles verifies that CLAUDE.md/AGENTS.md
// at agent level (e.g., crew/CLAUDE.md) ARE flagged as stale files.
// These are no longer created — only ~/gt/CLAUDE.md (town root) exists.
func TestPrimingCheck_FlagsStaleAgentLevelFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor (required by upstream check)
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew/ directory structure with stale files
	crewPath := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(filepath.Join(crewPath, "max"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create stale CLAUDE.md and AGENTS.md at agent level
	if err := os.WriteFile(filepath.Join(crewPath, "CLAUDE.md"), []byte("# Stale CLAUDE.md\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewPath, "AGENTS.md"), []byte("# Stale AGENTS.md\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Should find stale file issues for crew
	foundClaudeMdIssue := false
	foundAgentsMdIssue := false
	for _, detail := range result.Details {
		if strings.Contains(detail, rigName+"/crew:") && strings.Contains(detail, "Stale CLAUDE.md") {
			foundClaudeMdIssue = true
		}
		if strings.Contains(detail, rigName+"/crew:") && strings.Contains(detail, "Stale AGENTS.md") {
			foundAgentsMdIssue = true
		}
	}

	if !foundClaudeMdIssue {
		t.Errorf("expected stale CLAUDE.md issue for crew, got details: %v", result.Details)
	}
	if !foundAgentsMdIssue {
		t.Errorf("expected stale AGENTS.md issue for crew, got details: %v", result.Details)
	}
}

// TestPrimingCheck_NoIssuesWhenCorrectlyConfigured verifies that a correctly
// configured rig reports no priming issues.
// A correctly configured rig has NO per-directory CLAUDE.md/AGENTS.md files.
// Only ~/gt/CLAUDE.md (town root identity anchor) exists on disk.
func TestPrimingCheck_NoIssuesWhenCorrectlyConfigured(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor (required by upstream check)
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads and PRIME.md
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew directory — NO CLAUDE.md or AGENTS.md
	crewPath := filepath.Join(tmpDir, rigName, "crew")
	if err := os.MkdirAll(crewPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create polecats directory — NO CLAUDE.md or AGENTS.md
	polecatsPath := filepath.Join(tmpDir, rigName, "polecats")
	if err := os.MkdirAll(polecatsPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	if len(result.Details) > 0 {
		t.Errorf("expected no priming issues for correctly configured rig, got: %v", result.Details)
	}
}

// TestPrimingCheck_DetectsStaleIntermediateFiles verifies that stale CLAUDE.md/AGENTS.md
// at intermediate directories (crew/, polecats/) are detected, and that
// leftover refinery/ and witness/ directories from retired roles are ignored.
func TestPrimingCheck_DetectsStaleIntermediateFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create stale files at the live intermediate directories and at leftover
	// retired-role directories
	for _, role := range []string{"refinery", "witness", "crew", "polecats"} {
		rolePath := filepath.Join(tmpDir, rigName, role)
		if err := os.MkdirAll(rolePath, 0755); err != nil {
			t.Fatal(err)
		}
		for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
			if err := os.WriteFile(filepath.Join(rolePath, filename), []byte("# Stale\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Run priming check
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	result := check.Run(ctx)

	// Leftover retired-role directories must not be reported
	for _, detail := range result.Details {
		if strings.Contains(detail, rigName+"/refinery") || strings.Contains(detail, rigName+"/witness") {
			t.Errorf("leftover retired-role directory should be ignored, got: %s", detail)
		}
	}

	// Should find stale issues for all live roles
	expectedLocations := []string{"crew", "polecats"}
	for _, loc := range expectedLocations {
		found := false
		for _, detail := range result.Details {
			if strings.Contains(detail, loc) && strings.Contains(detail, "Stale") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected stale file issue for %s, got details: %v", loc, result.Details)
		}
	}
}

// TestPrimingCheck_FixRemovesStaleIntermediateFiles verifies that doctor --fix
// removes stale CLAUDE.md/AGENTS.md from intermediate directories.
func TestPrimingCheck_FixRemovesStaleIntermediateFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create town root CLAUDE.md identity anchor
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Gas Town\nRun gt prime\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Set up rig with .beads
	rigBeadsDir := filepath.Join(tmpDir, rigName, ".beads")
	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "PRIME.md"), []byte("# PRIME\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create stale files at intermediate directories
	staleFiles := []string{}
	for _, role := range []string{"crew", "polecats"} {
		rolePath := filepath.Join(tmpDir, rigName, role)
		if err := os.MkdirAll(rolePath, 0755); err != nil {
			t.Fatal(err)
		}
		for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
			filePath := filepath.Join(rolePath, filename)
			if err := os.WriteFile(filePath, []byte("# Stale\n"), 0644); err != nil {
				t.Fatal(err)
			}
			staleFiles = append(staleFiles, filePath)
		}
	}

	// A leftover retired-role directory's file must be left alone
	leftoverPath := filepath.Join(tmpDir, rigName, "witness", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(leftoverPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leftoverPath, []byte("# Leftover\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Run priming check and fix
	check := primingCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	_ = check.Run(ctx)

	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix() failed: %v", err)
	}

	// Verify all stale files were removed
	for _, filePath := range staleFiles {
		if _, err := os.Stat(filePath); err == nil {
			t.Errorf("stale file should have been removed: %s", filePath)
		}
	}

	// Verify the leftover retired-role file was NOT touched
	if _, err := os.Stat(leftoverPath); err != nil {
		t.Errorf("leftover retired-role file should not have been removed: %v", err)
	}

	// Verify town root CLAUDE.md was NOT removed (it's the identity anchor)
	townRootClaude := filepath.Join(tmpDir, "CLAUDE.md")
	if _, err := os.Stat(townRootClaude); os.IsNotExist(err) {
		t.Errorf("town root CLAUDE.md should NOT have been removed")
	}
}
