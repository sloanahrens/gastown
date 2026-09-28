package execother

import (
	"context"
	"os/exec"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exec.CommandContext(ctx, "tmux", "ls")
}
