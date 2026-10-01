package helpers

import "testing"

func TestShadowed(t *testing.T) {
	t.Parallel()
	runGit := func(...string) {}
	runGit("status")
	setup()
}
