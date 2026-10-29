package gitfake

import "testing"

func TestFakeCrewContract(t *testing.T) {
	t.Parallel()
	RunCrewContract(t, func(t *testing.T) Env { return New() })
}

// TestCrewCloneKeepsHistoryAndBranches is the crew full-clone case over the
// fake; TestIntegrationCrewCloneKeepsHistoryAndBranches runs the same case
// against real git.
func TestCrewCloneKeepsHistoryAndBranches(t *testing.T) {
	t.Parallel()
	CrewCloneKeepsHistoryAndBranches(t, func(t *testing.T) Env { return New() })
}
