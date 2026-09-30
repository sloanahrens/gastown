//go:build integration

package integrationonly

import (
	"os/exec"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

func TestIntegrationReal(t *testing.T) {
	_ = exec.Command("git", "status")
	_ = git.NewGit(t.TempDir())
}
