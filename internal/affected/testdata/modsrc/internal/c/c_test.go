package c

import (
	"testing"

	"example.com/m/internal/a"
)

// TestValue keeps a's import in c's test variant, so a change to a reaches c.
func TestValue(t *testing.T) {
	if a.Value != 1 {
		t.Fatal("fixture a.Value changed")
	}
}
