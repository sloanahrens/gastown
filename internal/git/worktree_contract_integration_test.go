//go:build integration

package git_test

import (
	"testing"

	"github.com/steveyegge/gastown/internal/git/gitfake"
)

func TestIntegrationGitfakeWorkTreeContract(t *testing.T) {
	t.Parallel()
	gitfake.RunWorkTreeContract(t, func(t *testing.T) gitfake.Env { return &realEnv{} })
}
