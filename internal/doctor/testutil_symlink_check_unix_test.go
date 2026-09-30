//go:build !windows

package doctor

// The symlink cases live in a file of their own: creating a symlink needs
// elevated privileges on Windows.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestutilSymlinkCheck_ValidSymlink(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "helper.go"), []byte("package testutil\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew worker with proper symlink
	crewInternal := filepath.Join(tmpDir, rigName, "crew", "bob", "internal")
	if err := os.MkdirAll(crewInternal, 0755); err != nil {
		t.Fatal(err)
	}
	// Relative symlink: crew/bob/internal/testutil -> ../../../mayor/rig/internal/testutil
	relTarget, err := filepath.Rel(crewInternal, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, filepath.Join(crewInternal, "testutil")); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("expected StatusOK for valid symlink, got %v: %s", result.Status, result.Message)
	}
}

func TestTestutilSymlinkCheck_BrokenSymlink(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	// Create crew worker with broken symlink
	crewInternal := filepath.Join(tmpDir, rigName, "crew", "charlie", "internal")
	if err := os.MkdirAll(crewInternal, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/path", filepath.Join(crewInternal, "testutil")); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for broken symlink, got %v: %s", result.Status, result.Message)
	}
}

func TestTestutilSymlinkCheck_WrongTarget(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a stale local copy that the symlink points to instead
	staleDir := filepath.Join(tmpDir, rigName, "crew", "dave", "internal", "stale_testutil")
	if err := os.MkdirAll(staleDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create crew worker symlink pointing to wrong target
	crewInternal := filepath.Join(tmpDir, rigName, "crew", "dave", "internal")
	if err := os.Symlink(staleDir, filepath.Join(crewInternal, "testutil")); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning for wrong target, got %v: %s", result.Status, result.Message)
	}
	if len(result.Details) == 0 || !strings.Contains(result.Details[0], "not canonical") {
		t.Errorf("expected detail about non-canonical target, got %v", result.Details)
	}
}

func TestTestutilSymlinkCheck_Fix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil with a file
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "helper.go"), []byte("package testutil\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create crew worker with real testutil directory
	crewTestutil := filepath.Join(tmpDir, rigName, "crew", "eve", "internal", "testutil")
	if err := os.MkdirAll(crewTestutil, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crewTestutil, "old.go"), []byte("package testutil // stale\n"), 0644); err != nil {
		t.Fatal(err)
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	// Run should detect issues
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected StatusWarning before fix, got %v: %s", result.Status, result.Message)
	}

	// Fix should replace dirs with symlinks
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify crew symlink
	crewLink := filepath.Join(tmpDir, rigName, "crew", "eve", "internal", "testutil")
	info, err := os.Lstat(crewLink)
	if err != nil {
		t.Fatalf("cannot stat crew symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("crew testutil should be a symlink after fix")
	}

	// Verify the symlink resolves and points to canonical
	resolved, err := filepath.EvalSymlinks(crewLink)
	if err != nil {
		t.Fatalf("crew symlink does not resolve: %v", err)
	}
	canonicalResolved, _ := filepath.EvalSymlinks(canonical)
	if resolved != canonicalResolved {
		t.Errorf("crew symlink resolves to %s, want %s", resolved, canonicalResolved)
	}

	// Verify canonical file is accessible through the symlink
	content, err := os.ReadFile(filepath.Join(crewLink, "helper.go"))
	if err != nil {
		t.Fatalf("cannot read through crew symlink: %v", err)
	}
	if string(content) != "package testutil\n" {
		t.Errorf("unexpected content through symlink: %q", content)
	}

	// Run again — should be clean now
	check2 := NewTestutilSymlinkCheck()
	result = check2.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected StatusOK after fix, got %v: %s", result.Status, result.Message)
	}
}

func TestTestutilSymlinkCheck_MultipleCrewMembers(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigName := "testrig"

	// Create canonical testutil
	canonical := filepath.Join(tmpDir, rigName, "mayor", "rig", "internal", "testutil")
	if err := os.MkdirAll(canonical, 0755); err != nil {
		t.Fatal(err)
	}

	// Create 3 crew members: 1 with symlink, 2 with real dirs
	crewNames := []string{"good", "bad1", "bad2"}
	for _, name := range crewNames {
		crewInternal := filepath.Join(tmpDir, rigName, "crew", name, "internal")
		if err := os.MkdirAll(crewInternal, 0755); err != nil {
			t.Fatal(err)
		}

		if name == "good" {
			// Proper symlink
			relTarget, _ := filepath.Rel(crewInternal, canonical)
			if err := os.Symlink(relTarget, filepath.Join(crewInternal, "testutil")); err != nil {
				t.Fatal(err)
			}
		} else {
			// Real directory
			if err := os.MkdirAll(filepath.Join(crewInternal, "testutil"), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}

	check := NewTestutilSymlinkCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}

	result := check.Run(ctx)

	if result.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %v: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "2 testutil") {
		t.Errorf("expected 2 issues, got %q", result.Message)
	}
}
