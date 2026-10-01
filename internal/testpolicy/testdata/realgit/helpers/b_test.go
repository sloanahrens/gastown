package helpers

import "testing"

func TestUsesHelper(t *testing.T) {
	t.Parallel()
	// initRepo lives in a_test.go, so only the name ties this call to git.
	_ = t
	initRepo(t)
}
