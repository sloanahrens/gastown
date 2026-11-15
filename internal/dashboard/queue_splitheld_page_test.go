package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Beads panel's "ready to dispatch" set is what the operator reads as work
// a seat can take. A bead another route holds — the split route's, waiting for
// a person to cut it smaller, or one a hold label keeps off the dispatcher —
// must not be in it, and the split-held ones get a chip that counts them
// (gt-bgumb7). The read below is the split route's own line as the dispatcher's
// tick writes it: the bead id the tick prefixes, the route (specdispatch.
// SkipSplitOver), the estimate and the trigger.
const splitHeldReason = "gt-spl1t: split: estimated 20.0 min (sp:8, rung all), at or over the 15-minute trigger; held for splitting"

// rowTag reports whether the row drew a tag leaf reading exactly text: the
// span the page builds for a row's own verdict, not any text the tick's reason
// happens to carry.
func rowTag(row queueRowDrawn, text string) bool {
	for _, l := range row.Leaves {
		if l.Tag == "span" && l.Class == "tag" && l.Text == text {
			return true
		}
	}
	return false
}

// TestQueueSplitHeldBeadIsNotReadyToDispatch draws a ready list holding a bead
// the split route holds beside a bead waiting for a seat and one the dispatcher
// would slot: the ready-to-dispatch count and its row list leave the split-held
// bead out, the split chip counts it, and the header names it as the piece it
// is in (gt-bgumb7).
func TestQueueSplitHeldBeadIsNotReadyToDispatch(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		readyRow("gt-now1", "ok"),
		skippedRow("gt-seat1", "2026-10-29T11:58:00Z", "no seat: over the town total", 4),
		skippedRow("gt-spl1t", "2026-10-29T11:58:00Z", splitHeldReason, 2),
	))

	chips := queueChips(t, dom)
	if got := chipNamed(t, chips, "ready to dispatch"); got.Count != "2" || !got.On {
		t.Errorf("the ready to dispatch chip reads %+v, want count 2 and open: the split-held bead is not the dispatcher's to slot", got)
	}
	if got := chipNamed(t, chips, "split"); got.Count != "1" {
		t.Errorf("the split chip counts %q, want 1: it is the chip that counts the beads the split route holds", got.Count)
	}

	tiles := queueHeader(t, dom)
	if got, ok := headerTile(tiles, "held for splitting"); !ok || got.Count != "1" {
		t.Errorf("the header reads the split piece as %+v, want one bead held for splitting", got)
	}

	// The dispatchable view is the set the operator works from: the split-held
	// bead is not drawn in it, and its own chip draws exactly it.
	if got, want := queueRowIDs(t, dom), []string{"gt-now1", "gt-seat1"}; !slices.Equal(got, want) {
		t.Errorf("the ready to dispatch view lists %q, want %q: a split-held bead is not ready to dispatch", got, want)
	}
	clickQueueChip(t, dom, "split")
	if got := queueRowIDs(t, dom); !slices.Equal(got, []string{"gt-spl1t"}) {
		t.Errorf("the split chip lists %q, want the bead the split route holds", got)
	}
	// The row's tag says what holds it, and it is not drawn as an ordinary skip:
	// the tag is the row's own verdict, the dim line below it the tick's text.
	rows := queueRows(t, dom)
	if len(rows) != 1 {
		t.Fatalf("the split chip draws %d rows, want 1", len(rows))
	}
	if got := rowTag(rows[0], "held for splitting"); !got {
		t.Errorf("the split-held row draws no %q tag: %+v", "held for splitting", rows[0].Leaves)
	}
	if strings.Contains(rows[0].Text, "skipped: split:") {
		t.Errorf("the split-held row reads %q, want it held for splitting rather than an ordinary skip", rows[0].Text)
	}
	clickQueueChip(t, dom, "split")
	if got, want := queueRowIDs(t, dom), []string{"gt-now1", "gt-seat1", "gt-spl1t"}; !slices.Equal(got, want) {
		t.Errorf("a second click on the split chip lists %q, want the filter cleared to %q", got, want)
	}
}

// TestQueueHoldLabelledBeadIsNotReadyToDispatch drives every label the page
// declares as one that holds a bead for a person (HELD_LABELS): each keeps its
// bead out of the ready-to-dispatch count, and the bead is counted under the
// held chip instead (gt-bgumb7). The list is read back off the page, so a label
// added to it without being honoured fails here, and the two labels the bead
// names — operator-only and held — must be in it.
func TestQueueHoldLabelledBeadIsNotReadyToDispatch(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(readyRow("gt-now1", "ok")))
	var labels []string
	queueReadBack(t, dom, "HELD_LABELS", &labels)
	for _, want := range []string{"operator-only", "held"} {
		if !slices.Contains(labels, want) {
			t.Fatalf("HELD_LABELS = %q, want it to name %q: a bead labelled it is held by a person, not ready to dispatch", labels, want)
		}
	}

	for _, label := range labels {
		// A bead that lints clean and wears only the hold label: the label is
		// what keeps it off the dispatcher, not its text.
		row := readyRow("gt-held1", "ok")
		row["labels"] = []string{label}
		page := drawQueuePage(t, queueRead(row))
		if got := chipNamed(t, queueChips(t, page), "ready to dispatch"); got.Count != "0" {
			t.Errorf("a bead labelled %q draws a ready to dispatch count of %q, want 0: the dispatcher slings no held bead", label, got.Count)
		}
		if got := chipNamed(t, queueChips(t, page), "held"); got.Count != "1" {
			t.Errorf("a bead labelled %q draws %q on the held chip, want 1", label, got.Count)
		}
		clickQueueChip(t, page, "held")
		rows := queueRows(t, page)
		if len(rows) != 1 || !strings.Contains(rows[0].Text, "held") {
			t.Errorf("a bead labelled %q draws %+v under the held chip, want it held", label, rows)
		}
	}

	// A label is typed by hand, so the page matches it as the dispatcher's own
	// rule does: case and surrounding space are not part of it.
	row := readyRow("gt-held2", "ok")
	row["labels"] = []string{"  Operator-Only  "}
	page := drawQueuePage(t, queueRead(row))
	if got := chipNamed(t, queueChips(t, page), "ready to dispatch"); got.Count != "0" {
		t.Errorf("a bead labelled %q draws a ready to dispatch count of %q, want 0", row["labels"], got.Count)
	}
}

// TestQueueSplitHeldNeedsTheTicksReason draws the two readings that must not
// count as split-held (gt-bgumb7): a bead wearing the split label with no tick
// reporting it, because the page does not re-derive the route's threshold from
// the label, and a bead the dispatcher's tick passed over for another route,
// whose reason is not the split route's line. Both stay ready work (the first)
// or a guard's skip (the second).
func TestQueueSplitHeldNeedsTheTicksReason(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		row   map[string]any
		piece string
	}{
		{"the split label alone", func() map[string]any {
			r := readyRow("gt-lab1", "ok")
			r["labels"] = []string{"split"}
			return r
		}(), "start now"},
		{"another route's skip line", skippedRow("gt-hold1", "2026-10-29T11:58:00Z", "gt-hold1: held: overlap with gt-okckk", 3), "skipped by a guard"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			dom := drawQueuePage(t, queueRead(c.row))
			if got := chipNamed(t, queueChips(t, dom), "split"); got.Count != "0" {
				t.Errorf("the split chip counts %q, want 0: only the tick's own split line holds a bead", got.Count)
			}
			if got := chipNamed(t, queueChips(t, dom), "ready to dispatch"); got.Count != "1" {
				t.Errorf("the ready to dispatch chip counts %q, want 1", got.Count)
			}
			if got, ok := headerTile(queueHeader(t, dom), c.piece); !ok || got.Count != "1" {
				t.Errorf("the bead reads %+v, want one bead under %q", got, c.piece)
			}
		})
	}
}

// TestQueueEmptyDispatchableViewNamesSplitHeld draws a ready list whose only
// bead the split route holds: the dispatchable view is empty, and the message
// names the split hold rather than reporting that no filter matched
// (gt-bgumb7).
func TestQueueEmptyDispatchableViewNamesSplitHeld(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(skippedRow("gt-spl1t", "2026-10-29T11:58:00Z", splitHeldReason, 2)))
	var empty string
	queueReadBack(t, dom, "__qEmpty()", &empty)
	if !strings.Contains(empty, "nothing is dispatchable right now") || !strings.Contains(empty, "1 bead is held for splitting") {
		t.Errorf("the empty dispatchable view reads %q, want it to name the bead held for splitting", empty)
	}
}

// TestQueueSplitPieceSumsToTheReadyTotal draws a ready list whose every piece
// is a held one and reads the header back: the pieces still add up to the ready
// total, so the split piece is a piece of the partition and not a count beside
// it (gt-xwul0, gt-bgumb7).
func TestQueueSplitPieceSumsToTheReadyTotal(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-spl1t", "2026-10-29T11:58:00Z", splitHeldReason, 2),
		skippedRow("gt-spl2t", "2026-10-29T11:58:00Z", "gt-spl2t: split: estimated 45.0 min (sp:13, rung all), at or over the 30-minute trigger; held for splitting", 5),
		readyRow("gt-now1", "ok"),
	))

	sum := 0
	for _, tile := range queueHeader(t, dom) {
		n, err := strconv.Atoi(tile.Count)
		if err != nil {
			t.Fatalf("the %q tile reads %q, want a count", tile.Label, tile.Count)
		}
		if tile.Label == "ready" {
			continue
		}
		sum += n
	}
	// The header's tiles are the pieces and then the landing total; the ready
	// total tile is the one counted above and left out of this sum.
	if sum != 3 {
		t.Errorf("the pieces and the landing total sum to %d, want 3 pieces plus 0 landing", sum)
	}
	if got, ok := headerTile(queueHeader(t, dom), "held for splitting"); !ok || got.Count != "2" {
		t.Errorf("the split piece reads %+v, want 2 beads held for splitting", got)
	}
}
