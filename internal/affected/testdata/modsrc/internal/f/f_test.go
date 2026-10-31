package f_test

import (
	"testing"

	"example.com/m/internal/a"
)

// TestValue keeps a's import in f's external test package, so a change to a
// reaches f through a test the package f itself does not import.
func TestValue(t *testing.T) {
	if a.Value != 1 {
		t.Fatal("fixture a.Value changed")
	}
}
