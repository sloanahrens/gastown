package beads

import (
	"strings"
	"testing"
)

// TestGateGuidanceDoesNotOrderMakeGate pins the ## Gate hint in the filing
// guidance to what the landing gate runs. "usually `make gate`" read as an
// order to run the full gate in the session; the landing runs it (gt-7u6yd).
func TestGateGuidanceDoesNotOrderMakeGate(t *testing.T) {
	if !strings.Contains(primeContent, "landing gate") {
		t.Error("the ## Gate hint in primeContent must say what the landing gate runs")
	}
	if strings.Contains(primeContent, "usually `make gate`") {
		t.Error("the ## Gate hint in primeContent still tells the author to run `make gate`")
	}
}
