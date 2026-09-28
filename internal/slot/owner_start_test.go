//go:build darwin || linux

package slot

import (
	"os"
	"testing"
)

// processStartToken is what tells a reused pid apart, so it is pinned against
// a real process, read-only: this process's token is readable and stable.
func TestProcessStartToken(t *testing.T) {
	t.Parallel()
	a, ok := processStartToken(os.Getpid())
	if !ok {
		t.Fatal("start token unreadable for this process")
	}
	b, _ := processStartToken(os.Getpid())
	if a == "" || a != b {
		t.Errorf("start token = %q then %q, want a stable non-empty value", a, b)
	}
	if _, ok := processStartToken(-1); ok {
		t.Error("start token readable for pid -1")
	}
}
