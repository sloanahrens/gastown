//go:build integration

package git_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestIntegrationGitfakeCrewContract(t *testing.T) {
	gitfake.RunCrewContract(t, func(t *testing.T) gitfake.Env { return &realEnv{} })
}
