//go:build integration && !darwin

package cmd

// cloneTree reports that this platform has no whole-tree clone, so
// cloneFixtureTree copies file by file.
func cloneTree(_, _ string) (bool, error) {
	return false, nil
}
