package cmd

// Unit-tier test helpers shared across this package. Those that run git live
// in git_helpers_integration_test.go. They lived in merge-queue test
// files until those were deleted with the merge queue (gt-v4ssj.6).

import (
	"testing"

	"github.com/spf13/cobra"
)

// contains checks if s contains substr (helper for styled output)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// CodeOfErr is CodeOf with the command's own error, so a caller can tell an
// approve (no error at all) from a request_changes (a SilentExitError).
func CodeOfErr(t *testing.T, cmd *cobra.Command) (int, error) {
	t.Helper()
	err := cmd.Execute()
	// Execute re-adds the default help command to the root it runs from
	// (cobra's InitDefaultHelpCmd removes and adds it on every call), which
	// marks the root unsorted. Sort it again before a parallel test walks
	// the shared tree (TestCommandTreeWalkIsReadOnlyUnderParallelTests).
	presortCommandTree(cmd.Root())
	if err == nil {
		return 0, nil
	}
	if code, ok := IsSilentExit(err); ok {
		return code, err
	}
	t.Errorf("command error: %v", err)
	return 1, err
}
