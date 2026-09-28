package setenvinsubtest

import "testing"

func TestA(t *testing.T) {
	t.Parallel()
	t.Run("x", func(t *testing.T) {
		t.Parallel()
		t.Setenv("A", "b")
	})
}
