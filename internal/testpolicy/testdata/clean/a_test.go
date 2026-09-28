package clean

import "testing"

func TestAdd(t *testing.T) {
	t.Parallel()
	if got := Add(1, 2); got != 3 {
		t.Fatalf("Add(1, 2) = %d, want 3", got)
	}
}
