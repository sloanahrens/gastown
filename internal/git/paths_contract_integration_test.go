//go:build integration

package git_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestIntegrationGitfakePathContract(t *testing.T) {
	gitfake.RunPathContract(t, func(t *testing.T) gitfake.BranchEnv { return &realEnv{} })
}
