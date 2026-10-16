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

	body := queueListFunc(t)
	if want := `qRig !== "all" && !rigs[qRig]) qRig = "all";`; !strings.Contains(body, want) {
		t.Errorf("the queue list no longer falls back to all stores when the chosen rig has no rows")
	}
}

// queueListFunc is the body of drawQueueList, the function that draws the
// whole queue inside the Work panel's folded list (gt-vcr3z). A test naming a
// function that is gone fails here rather than checking nothing.
func queueListFunc(t *testing.T) string {
	t.Helper()

	return pageFunc(t, "drawQueueList")
}

// TestWorkQueueEmptyDispatchableView reads the embedded page (gt-x9gk4): the
// pane's default view is empty whenever every ready bead is held or unshaped,
// so the message names the counts the operator would act on rather than
// reporting that no filter matched.
func TestWorkQueueEmptyDispatchableView(t *testing.T) {
	t.Parallel()

	body := queueListFunc(t)
	start := strings.Index(body, "if (!rows.length)")
	if start < 0 {
		t.Fatal("the queue list no longer branches on an empty row set")
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

// TestWorkQueueHidesWispsOnTheBlockedTab reads the embedded page (gt-q6ed5):
// dead wisp steps outnumbered the real blocked beads, so the Blocked tab opens
// with them filtered out. The default is a named constant and the chip carries
// the count of the wisps in scope. Which rows are wisps is the server's own
// verdict, read off the row (gt-vcr3z), so the list hides exactly the rows the
// Work panel's blocked line counts as leaked step wisps.
func TestWorkQueueHidesWispsOnTheBlockedTab(t *testing.T) {
	t.Parallel()

	page := string(indexHTML)
	for _, want := range []string{
		`const DEFAULT_QWISP = false;`,
		"let qWisp = DEFAULT_QWISP;",
		`const isWisp = b => !!b.wisp;`,
		`chip("wisps", inRig.filter(isWisp).length, qWisp,`,
		`(qTab !== "blocked" || qWisp || !isWisp(b))`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html has no %q", want)
		}
	}

	if want := "nothing is blocked outside"; !strings.Contains(queueListFunc(t), want) {
		t.Errorf("the empty blocked view no longer names the hidden wisps")
	}
}
