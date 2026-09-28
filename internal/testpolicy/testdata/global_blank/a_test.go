package globalblank

import "testing"

// Discarding a result into the blank identifier assigns no package state.
func TestBlank(t *testing.T) {
	t.Parallel()
	_ = f()
}
