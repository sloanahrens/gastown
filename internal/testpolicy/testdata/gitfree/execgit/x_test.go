package execgit

import (
	"context"
	"os/exec"
	"testing"
)

func TestExec(t *testing.T) {
	t.Parallel()
	_ = exec.Command("git", "status")
	_ = exec.CommandContext(context.Background(), "git", "log")
	_ = exec.Command("gitk")
}
