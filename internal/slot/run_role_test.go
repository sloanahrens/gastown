package slot

import (
	"fmt"
	"os"
	"testing"
)

// TestRunRole pins 'gt slot run's --role default (gt-cet2): an
// explicit --role always wins, an omitted one nested under a real ancestor
// hold inherits that ancestor's exact role (so it stays reentrant without
// having to know the role string in advance), and an omitted one with no
// ancestor hold falls back to the old unique-pid placeholder.
func TestRunRole(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	noAncestor := func(string) (string, bool) { return "", false }

	t.Run("explicit role always wins", func(t *testing.T) {
		ancestor := func(string) (string, bool) { return "hm/landing", true }
		if got := RunRole("gastown/landing", townRoot, ancestor); got != "gastown/landing" {
			t.Errorf("RunRole() = %q, want the explicit role unchanged", got)
		}
	})

	t.Run("omitted role nested under an ancestor hold inherits it", func(t *testing.T) {
		ancestor := func(root string) (string, bool) { return "hm/landing", root == townRoot }
		if got := RunRole("", townRoot, ancestor); got != "hm/landing" {
			t.Errorf("RunRole() = %q, want the inherited ancestor role", got)
		}
	})

	t.Run("omitted role with no ancestor hold falls back to a unique pid role", func(t *testing.T) {
		want := fmt.Sprintf("pid-%d", os.Getpid())
		if got := RunRole("", townRoot, noAncestor); got != want {
			t.Errorf("RunRole() = %q, want %q", got, want)
		}
	})
}
