package execgit

import (
	"os/exec"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	exec.Command("git", "init")
}
