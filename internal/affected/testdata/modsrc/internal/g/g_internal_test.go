package g

import "testing"

// TestValue keeps an internal test in g, so go list reports g's test variant
// and the packages rebuilt against it.
func TestValue(t *testing.T) {
	if Value != 7 {
		t.Fatal("fixture g.Value changed")
	}
}
