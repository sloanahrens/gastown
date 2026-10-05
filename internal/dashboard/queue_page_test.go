package dashboard

import (
	"strings"
	"testing"
)

// TestWorkQueueOpensOnAllStoresDispatchable reads the embedded page (gt-x9gk4):
// the work queue is what the operator reloads all day, so it opens on every
// store with the dispatchable chip on — the view he works from — rather than
// on one rig with every shape. Both defaults live in named constants, and the
// fallback that catches a chosen store with no rows survives (gt-fi93j).
func TestWorkQueueOpensOnAllStoresDispatchable(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, want := range []string{
		`const DEFAULT_QRIG = "all";`,
		`const DEFAULT_QSHAPE = "ok";`,
		"let qRig = DEFAULT_QRIG;",
		"let qShape = DEFAULT_QSHAPE;",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
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

// TestWorkQueueEmptyDispatchableView reads the embedded page (gt-x9gk4): the
// pane's default view is empty whenever every ready bead is held or unshaped,
// so the message names the counts the operator would act on rather than
// reporting that no filter matched.
func TestWorkQueueEmptyDispatchableView(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	start := strings.Index(page, "function renderQueue(")
	if start < 0 {
		t.Fatal("index.html has no renderQueue")
	}
	body := page[start:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	start = strings.Index(body, "if (!rows.length)")
	if start < 0 {
		t.Fatal("renderQueue no longer branches on an empty row set")
	}
	body = body[start:]
	for _, want := range []string{
		`qTab === "ready" && qShape === "ok"`,
		"nothing is dispatchable right now",
		"beads need shaping",
		"beads are held",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the empty dispatchable view has no %q", want)
		}
	}
}

// TestWorkQueueMarksBeadsHeldByAnAssignee reads the embedded page (gt-j70o5):
// the dispatcher takes only unassigned beads, so a bead someone holds has to
// be told apart from free work — a tag on the row, a chip that filters to it —
// and the caption has to say which way the dispatcher reads an assignee.
func TestWorkQueueMarksBeadsHeldByAnAssignee(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, want := range []string{
		`else if (b.shape === "held")`,
		`chip("held", inRig.filter(b => b.shape === "held").length`,
		"the spec dispatcher: shaped, unassigned",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}
}
