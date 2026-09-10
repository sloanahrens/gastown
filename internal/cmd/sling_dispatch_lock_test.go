package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestBuildSlingFormulaVars_ResumeDispatch guards gt-a8i3: a resume dispatch
// (`gt sling --branch <resume_branch>`) must populate resume_branch only.
// base_branch must stay whatever SpawnedPolecatInfo.BaseBranch reports (the
// merge-target base, e.g. "main" — never the resume branch), or `gt done`
// reads {{base_branch}} as --target and submits a self-targeted MR that
// merges as a no-op and gets deleted by post-merge cleanup.
func TestBuildSlingFormulaVars_ResumeDispatch(t *testing.T) {
	resumeBranch := "polecat/thunder/be-r18+mtvr3qm3"

	// Post-fix: spawnPolecatForSling never sets BaseBranch to the resume
	// branch, so this is "main" for an ordinary resume dispatch with no
	// --base-branch override.
	got := buildSlingFormulaVars(nil, nil, "main", resumeBranch)

	for _, v := range got {
		if strings.HasPrefix(v, "base_branch=") {
			t.Fatalf("resume dispatch must not set base_branch, got var %q (full list: %v)", v, got)
		}
	}
	wantResumeVar := "resume_branch=" + resumeBranch
	found := false
	for _, v := range got {
		if v == wantResumeVar {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected %q in formula vars, got: %v", wantResumeVar, got)
	}
}

// TestBuildSlingFormulaVars_NonMainBaseBranch verifies the ordinary
// (non-resume) --base-branch override path still works: rig defaults first,
// then user vars, then the non-"main" base_branch override.
func TestBuildSlingFormulaVars_NonMainBaseBranch(t *testing.T) {
	got := buildSlingFormulaVars(
		[]string{"lint_command=make lint"},
		[]string{"issue=gt-abc"},
		"integration/epic-1",
		"",
	)

	want := []string{"lint_command=make lint", "issue=gt-abc", "base_branch=integration/epic-1"}
	if len(got) != len(want) {
		t.Fatalf("buildSlingFormulaVars() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildSlingFormulaVars()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestExecuteSling_AcquiresBeadLock verifies that executeSling acquires the
// per-bead flock before reading bead status. This prevents TOCTOU races where
// multiple batch/queue dispatch calls read status=open concurrently.
func TestExecuteSling_AcquiresBeadLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}

	beadID := "gt-locktest1"

	// Hold the flock from outside executeSling — this simulates a concurrent dispatch.
	release, err := tryAcquireSlingBeadLock(townRoot, beadID)
	if err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}
	defer release()

	// Create a bd stub (won't be reached since lock should block first)
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
echo '[{"title":"Test","status":"open","assignee":"","description":""}]'
exit 0
`
	writeBDStub(t, binDir, bdScript, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := SlingParams{
		BeadID:   beadID,
		RigName:  "testrig",
		TownRoot: townRoot,
	}

	_, err = executeSling(params)
	if err == nil {
		t.Fatal("expected executeSling to fail when lock is held, got nil error")
	}
	if !strings.Contains(err.Error(), "already being slung") {
		t.Fatalf("expected lock contention error, got: %v", err)
	}
}

// TestExecuteSling_LockReleasedAfterReturn verifies that the flock is released
// when executeSling returns (even on error), allowing a subsequent call to proceed.
func TestExecuteSling_LockReleasedAfterReturn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("advisory flock is a no-op on Windows")
	}

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("failed to create .beads: %v", err)
	}

	beadID := "gt-lockrel1"

	// Create a bd stub that returns closed status (causes executeSling to error out)
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	bdScript := `#!/bin/sh
echo '[{"title":"Done","status":"closed","assignee":"","description":""}]'
exit 0
`
	writeBDStub(t, binDir, bdScript, "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := SlingParams{
		BeadID:   beadID,
		RigName:  "testrig",
		TownRoot: townRoot,
	}

	// First call — acquires lock, fails on closed guard, releases lock
	_, err := executeSling(params)
	if err == nil {
		t.Fatal("expected closed guard error")
	}

	// Second call — should acquire the lock (not contention error)
	_, err = executeSling(params)
	if err == nil {
		t.Fatal("expected closed guard error on second call")
	}
	if strings.Contains(err.Error(), "already being slung") {
		t.Fatal("lock was not released after first executeSling returned")
	}
}
