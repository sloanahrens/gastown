package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/callercwd"
)

// TestDoneRequiresWorktreeWithoutFacts: a caller whose directory is absent
// from this process is refused before anything is read, unless it is a crew
// submission that names both its branch and its head. A lone flag, or no
// flags, keeps the worktree rule (gt-pyh5p6).
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestDoneRequiresWorktreeWithoutFacts(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "gastown", "crew", "operator")
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, absent)

	crewEnv := func(key string) string {
		if key == "BD_ACTOR" {
			return "gastown/crew/operator"
		}
		return ""
	}
	fullHead := strings.Repeat("a1", 20)
	for _, tc := range []struct {
		name   string
		branch string
		head   string
		getenv func(string) string
	}{
		{"no flags", "", "", crewEnv},
		{"branch only", "crew/operator/gt-1", "", crewEnv},
		{"head only", "", fullHead, crewEnv},
	} {
		_, byFlags, err := doneCallerDir(tc.branch, tc.head, tc.getenv)
		if err == nil || !strings.Contains(err.Error(), "is not present here") {
			t.Errorf("%s: doneCallerDir err = %v, want the callercwd refusal %q", tc.name, err, "is not present here")
		}
		if byFlags {
			t.Errorf("%s: byFlags = true; only a crew submission with both flags skips the worktree rule", tc.name)
		}
	}

	// Both flags from a session that is not crew: the path names a polecat and
	// no identity says crew, so the worktree rule still applies.
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, filepath.Join(t.TempDir(), "gastown", "polecats", "shiny"))
	_, byFlags, err := doneCallerDir("crew/operator/gt-1", fullHead, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "is not present here") || byFlags {
		t.Errorf("non-crew with both flags: doneCallerDir = byFlags %v, err %v; want the callercwd refusal", byFlags, err)
	}
}

// TestDoneFlagsSkipWorktreeRuleForCrew: a crew caller that names both its
// branch and its head passes the gate with its worktree absent, and the run
// is told the facts come from the flags.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestDoneFlagsSkipWorktreeRuleForCrew(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "gastown", "crew", "operator")
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, absent)

	cwd, byFlags, err := doneCallerDir("crew/operator/gt-1", strings.Repeat("a1", 20), func(key string) string {
		if key == "BD_ACTOR" {
			return "gastown/crew/operator"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("doneCallerDir: %v", err)
	}
	if !byFlags || cwd != absent {
		t.Errorf("doneCallerDir = %q, byFlags %v; want %q with byFlags true", cwd, byFlags, absent)
	}
}

// TestDoneCallerDirRefusesUnusableDirectory: a GT_CALLER_CWD that cannot name
// a directory is refused on the flagged path too, before the role is read.
//
//testpolicy:allow parallel — GT_CALLER_CWD is the input under test, which t.Setenv shares with the whole process
func TestDoneCallerDirRefusesUnusableDirectory(t *testing.T) {
	//testpolicy:allow no-env — GT_CALLER_CWD is the input under test
	t.Setenv(callercwd.EnvCallerCwd, "relative/crew/operator")
	_, _, err := doneCallerDir("crew/operator/gt-1", strings.Repeat("a1", 20), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("doneCallerDir err = %v, want the absolute-path refusal", err)
	}
}
