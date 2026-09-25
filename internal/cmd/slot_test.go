package cmd

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/slot"
)

// TestResolveSlotRunRole pins 'gt slot run's --role default (gt-cet2): an
// explicit --role always wins, an omitted one nested under a real ancestor
// hold inherits that ancestor's exact role (so it stays reentrant without
// having to know the role string in advance), and an omitted one with no
// ancestor hold falls back to the old unique-pid placeholder.
func TestResolveSlotRunRole(t *testing.T) {
	townRoot := t.TempDir()

	t.Run("explicit role always wins", func(t *testing.T) {
		t.Setenv(slot.ReentrantEnvVar, "")
		if got := resolveSlotRunRole("gastown/refinery", townRoot); got != "gastown/refinery" {
			t.Errorf("resolveSlotRunRole() = %q, want the explicit role unchanged", got)
		}
	})

	t.Run("omitted role nested under an ancestor hold inherits it", func(t *testing.T) {
		foreignPID := strconv.Itoa(os.Getpid() + 100000)
		t.Setenv(slot.ReentrantEnvVar, slot.LockPath(townRoot)+"|"+foreignPID+"|gastown/refinery-batch")
		if got := resolveSlotRunRole("", townRoot); got != "gastown/refinery-batch" {
			t.Errorf("resolveSlotRunRole() = %q, want the inherited ancestor role", got)
		}
	})

	t.Run("omitted role with no ancestor hold falls back to a unique pid role", func(t *testing.T) {
		t.Setenv(slot.ReentrantEnvVar, "")
		want := fmt.Sprintf("pid-%d", os.Getpid())
		if got := resolveSlotRunRole("", townRoot); got != want {
			t.Errorf("resolveSlotRunRole() = %q, want %q", got, want)
		}
	})
}
