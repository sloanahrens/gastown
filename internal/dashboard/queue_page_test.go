package dashboard

import (
	"encoding/json"
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

// townRow is one ready bead of the town's own store, which the spec dispatcher
// never reads, shaped as the row would be if the lint had been run on it.
func townRow(id, shape string) map[string]any {
	return map[string]any{"id": id, "title": id + " work", "priority": 2, "rig": "hq", "shape": shape}
}

// workSummary is the Work panel's summary tiles, label → count, as the panel
// draws them: a count and the word it is counted under.
func workSummary(t *testing.T, dom *pageDOM) map[string]string {
	t.Helper()

	v, err := dom.vm.RunString(`JSON.stringify((() => {
		const out = {};
		const sum = __byId["work"].children.find(c => String(c.className).split(" ").indexOf("wsum") >= 0);
		if (sum) for (const x of sum.children) out[String(x.children[1].textContent)] = String(x.children[0].textContent);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading the panel's summary tiles: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the panel's summary tiles: %v", err)
	}
	return out
}

// TestWorkTownStoreChip reads the drawn Work panel over a queue holding one
// gastown ready bead and two of the town store's (gt-50dkq): the spec
// dispatcher reads each rig's store and never the town's, so the town's beads
// are counted under a chip of their own, each row says the bead is not
// dispatched, and the ready-to-dispatch count and its row list leave them out.
func TestWorkTownStoreChip(t *testing.T) {
	t.Parallel()

	q := queueRead(readyRow("gt-rig1", "ok"), townRow("hq-town1", "ok"), townRow("hq-town2", "fix"))
	q["ready_rigs"] = map[string]any{"gastown": 1, "hq": 2}
	dom := drawQueuePage(t, q)

	chips := queueChips(t, dom)
	if got := chipNamed(t, chips, "ready to dispatch"); got.Count != "1" || !got.On {
		t.Errorf("the ready to dispatch chip reads %+v, want count 1 (the rig's bead) and open on a fresh page", got)
	}
	if got := chipNamed(t, chips, "town store, not dispatched"); got.Count != "2" || got.On {
		t.Errorf("the town store chip reads %+v, want count 2 and closed", got)
	}
	if got := workSummary(t, dom)["dispatchable"]; got != "1" {
		t.Errorf("the dispatchable tile counts %q, want 1 (the rig's bead alone)", got)
	}
	rows := queueRows(t, dom)
	if len(rows) != 1 || !strings.Contains(rows[0].Text, "gt-rig1") {
		t.Fatalf("the dispatchable view draws %+v, want the rig's bead alone", rows)
	}

	// The town store's chip lists every ready bead it holds, the unshaped one
	// included: no shape chip is offered there, since none of them is
	// dispatched.
	clickQueueChip(t, dom, "town store, not dispatched")
	rows = queueRows(t, dom)
	if len(rows) != 2 {
		t.Fatalf("the town store chip draws %+v, want both of the town's beads", rows)
	}
	for _, row := range rows {
		if !strings.Contains(row.Text, "not dispatched") {
			t.Errorf("the town store row reads %q, want it to say the bead is not dispatched", row.Text)
		}
	}

	// A town holding nothing but town-store beads used to read them all as
	// dispatchable. The view now names where they wait, and the chip that lists
	// them is drawn although the town store is the only store there is.
	dom = drawQueuePage(t, queueRead(townRow("hq-town1", "ok")))
	var empty string
	queueReadBack(t, dom, "__qEmpty()", &empty)
	if !strings.Contains(empty, "1 bead waits in the town store") {
		t.Errorf("the empty dispatchable view reads %q, want it to name the town store's waiting bead", empty)
	}
	if got := chipNamed(t, queueChips(t, dom), "town store, not dispatched"); got.Count != "1" {
		t.Errorf("a town whose only ready beads are its own store's draws %+v, want the town store chip with count 1", got)
	}
}

// TestWorkReadyCountRigsOnly reads the drawn Work panel back to the other half
// of gt-50dkq: leaving the town store out of the dispatch count leaves every
// rig's bead in it, the one the dispatcher's last tick passed over included.
func TestWorkReadyCountRigsOnly(t *testing.T) {
	t.Parallel()

	q := queueRead(readyRow("gt-rig1", "ok"), skippedRow("gt-rig2", "2026-10-29T11:58:00Z", "no seat: over the town total", 4), townRow("hq-town1", "ok"))
	q["ready_rigs"] = map[string]any{"gastown": 2, "hq": 1}
	dom := drawQueuePage(t, q)

	if got := chipNamed(t, queueChips(t, dom), "ready to dispatch"); got.Count != "2" {
		t.Errorf("the ready to dispatch chip counts %q, want 2: the rig's shaped bead and the one its tick passed over", got.Count)
	}
	// The tile counts the shaped beads of the rig stores, as it did before.
	if got := workSummary(t, dom)["dispatchable"]; got != "1" {
		t.Errorf("the dispatchable tile counts %q, want 1 (the rig's shaped bead)", got)
	}
	var texts []string
	for _, r := range queueRows(t, dom) {
		texts = append(texts, r.Text)
	}
	for _, id := range []string{"gt-rig1", "gt-rig2"} {
		found := false
		for _, text := range texts {
			found = found || strings.Contains(text, id)
		}
		if !found {
			t.Errorf("the dispatchable view %q does not list the rig's bead %s", texts, id)
		}
	}
}
