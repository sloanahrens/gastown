package helpers

import (
	"os/exec"
	"testing"
)

func runGit(t *testing.T, args ...string) {
	t.Helper()
	_ = exec.Command("git", args...)
}

func initRepo(t *testing.T) {
	t.Helper()
	runGit(t, "init")
}
