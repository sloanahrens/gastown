//go:build integration

package git_test

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestIntegrationGitfakeCrewContract(t *testing.T) {
	t.Parallel()
	gitfake.RunCrewContract(t, func(t *testing.T) gitfake.Env { return &realEnv{} })
}

// TestIntegrationCrewCloneKeepsHistoryAndBranches runs the crew full-clone
// case against real git, then reads the clone with git itself.
func TestIntegrationCrewCloneKeepsHistoryAndBranches(t *testing.T) {
	t.Parallel()
	env := &realEnv{}
	crew := gitfake.CrewCloneKeepsHistoryAndBranches(t, func(t *testing.T) gitfake.Env { return env })
	if shallow := env.git(t, crew, "", nil, "rev-parse", "--is-shallow-repository"); shallow != "false" {
		t.Errorf("rev-parse --is-shallow-repository = %q; want false", shallow)
	}
	if count := env.git(t, crew, "", nil, "rev-list", "--count", "HEAD"); count != "3" {
		t.Errorf("rev-list --count HEAD = %q; want 3", count)
	}
	if refs := env.git(t, crew, "", nil, "branch", "-r"); !strings.Contains(refs, "origin/main") || !strings.Contains(refs, "origin/other") {
		t.Errorf("branch -r = %q; want origin/main and origin/other", refs)
	}
}
