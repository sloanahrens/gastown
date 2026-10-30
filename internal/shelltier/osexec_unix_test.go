//go:build unix

package shelltier

import (
	"slices"
	"testing"
)

// TestNewCommandIsBoundedAndGrouped checks the command the budget kills without
// starting it: the context's kill hook is installed, the command leads its own
// process group, and Wait is bounded.
func TestNewCommandIsBoundedAndGrouped(t *testing.T) {
	t.Parallel()
	c := testCommand()
	cmd := newCommand(t.Context(), c)
	if cmd.Cancel == nil {
		t.Fatal("Cancel is nil: the budget would signal only the shell, not its children")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("the command does not lead its own process group")
	}
	if cmd.WaitDelay != killWaitDelay {
		t.Fatalf("WaitDelay = %v, want %v", cmd.WaitDelay, killWaitDelay)
	}
	if cmd.Dir != c.Dir {
		t.Fatalf("Dir = %q, want %q", cmd.Dir, c.Dir)
	}
	for _, kv := range c.Env {
		if !slices.Contains(cmd.Env, kv) {
			t.Fatalf("env %q is missing from the command's environment", kv)
		}
	}
}
