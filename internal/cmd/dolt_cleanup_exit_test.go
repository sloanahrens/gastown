package cmd

import (
	"strings"
	"testing"
)

// TestCleanupExitError covers the exit status of a cleanup pass: any orphan
// left in place must make the command fail, or a script that gates on the exit
// code reads "Removed 0/1" as success (gt-tfsp6).
func TestCleanupExitError(t *testing.T) {
	t.Parallel()

	t.Run("every orphan removed", func(t *testing.T) {
		if err := cleanupExitError(nil, 3); err != nil {
			t.Errorf("cleanupExitError(no failures) = %v, want nil", err)
		}
	})

	t.Run("no orphan removed", func(t *testing.T) {
		err := cleanupExitError([]string{"beads"}, 1)
		if err == nil {
			t.Fatal("expected non-zero exit when the only removal failed")
		}
		for _, want := range []string{"1 of 1", "beads"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should name %q", err, want)
			}
		}
	})

	t.Run("partial removal names every survivor", func(t *testing.T) {
		err := cleanupExitError([]string{"beads", "testdb_x"}, 4)
		if err == nil {
			t.Fatal("expected non-zero exit when orphans survive")
		}
		for _, want := range []string{"2 of 4", "beads", "testdb_x"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should name %q", err, want)
			}
		}
	})
}
