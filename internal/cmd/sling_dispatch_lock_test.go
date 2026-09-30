package cmd

import (
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
	t.Parallel()
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
	t.Parallel()
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
