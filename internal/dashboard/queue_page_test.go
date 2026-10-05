package dashboard

import (
	"strings"
	"testing"
)

// TestWorkQueueOpensOnGastown reads the embedded page (gt-fi93j): the work
// queue is what the operator reloads all day, so it opens on the rig he works
// in rather than on every store, with the other stores one chip away. The
// default lives in one named constant, and the fallback that catches a rig
// with no rows survives — a renamed or absent gastown must still show the
// other stores rather than an empty pane.
func TestWorkQueueOpensOnGastown(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	if want := `const DEFAULT_QRIG = "gastown";`; !strings.Contains(page, want) {
		t.Fatalf("index.html has no %s", want)
	}
	if want := "let qRig = DEFAULT_QRIG;"; !strings.Contains(page, want) {
		t.Errorf("the queue's rig filter does not start on DEFAULT_QRIG (want %q)", want)
	}

	start := strings.Index(page, "function renderQueue(")
	if start < 0 {
		t.Fatal("index.html has no renderQueue")
	}
	body := page[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	if want := `qRig !== "all" && !rigs[qRig]) qRig = "all";`; !strings.Contains(body, want) {
		t.Errorf("renderQueue no longer falls back to all stores when the chosen rig has no rows")
	}
}
