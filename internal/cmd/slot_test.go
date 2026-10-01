package cmd

import (
	"fmt"
	"os"
	"testing"
)

// TestResolveSlotRunRole pins 'gt slot run's --role default (gt-cet2): an
// explicit --role always wins, an omitted one nested under a real ancestor
// hold inherits that ancestor's exact role (so it stays reentrant without
// having to know the role string in advance), and an omitted one with no
// ancestor hold falls back to the old unique-pid placeholder.
func TestResolveSlotRunRole(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	noAncestor := func(string) (string, bool) { return "", false }

	t.Run("explicit role always wins", func(t *testing.T) {
		ancestor := func(string) (string, bool) { return "gastown/refinery-batch", true }
		if got := resolveSlotRunRole("gastown/refinery", townRoot, ancestor); got != "gastown/refinery" {
			t.Errorf("resolveSlotRunRole() = %q, want the explicit role unchanged", got)
		}
	})

	t.Run("omitted role nested under an ancestor hold inherits it", func(t *testing.T) {
		ancestor := func(root string) (string, bool) { return "gastown/refinery-batch", root == townRoot }
		if got := resolveSlotRunRole("", townRoot, ancestor); got != "gastown/refinery-batch" {
			t.Errorf("resolveSlotRunRole() = %q, want the inherited ancestor role", got)
		}
	})

	t.Run("omitted role with no ancestor hold falls back to a unique pid role", func(t *testing.T) {
		want := fmt.Sprintf("pid-%d", os.Getpid())
		if got := resolveSlotRunRole("", townRoot, noAncestor); got != want {
			t.Errorf("resolveSlotRunRole() = %q, want %q", got, want)
		}
	})
}
