package g_test

import (
	"testing"

	"example.com/m/internal/h"
)

// TestValue imports h, which imports g, so g's test binary needs a rebuild of h
// and a change to h reaches g.
func TestValue(t *testing.T) {
	if h.Value != 8 {
		t.Fatal("fixture h.Value changed")
	}
}
