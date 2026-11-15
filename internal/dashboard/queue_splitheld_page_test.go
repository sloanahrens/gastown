package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The Beads panel's "ready to dispatch" set is what the operator reads as work
// a seat can take. A bead another route holds — the split route's, waiting for
// a person to cut it smaller, or one a hold label keeps off the dispatcher —
// must not be in it, and the split-held ones get a chip that counts them
// (gt-bgumb7). The page's two readings of those holds are copies of the Go
// rules — the split route's skip prefix and the dispatcher's hold labels — so
// the tests here build their readings from the Go constants and check the page
// agrees with them (TestQueueSplitHeldReadsTheDispatchersReasonLine,
// TestQueueHeldLabelsTrackTheDispatchersRules).

// heldHoldLabel is the "held" spelling internal/daemon's stale-hold alert reads
// as a hold on a bead (hold_stale_attention.go). It is not one of
// dispatch.HumanHoldLabels, and the dashboard cannot import the daemon (the
// daemon imports the dashboard), so the page and the alert share this one
// spelling by hand.
const heldHoldLabel = "held"

// splitRouteLine is the line the split route writes for one bead, in the shape
// specSplitHeldLine writes it: the bead id the tick prefixes, then the route's
// own reason prefix, the estimate and the trigger. Built from
// specdispatch.SkipSplitOver so a fixture cannot drift from the prefix the
// dispatcher actually writes.
func splitRouteLine(beadID, estimate string) string {
	return beadID + ": " + specdispatch.SkipSplitOver + estimate + "; held for splitting"
}

// splitHeldReason is the split route's line for gt-spl1t: the estimate and the
// trigger as the route writes them.
var splitHeldReason = splitRouteLine("gt-spl1t", "estimated 20.0 min (sp:8, rung all), at or over the 15-minute trigger")

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

// TestQueueSplitHeldReadsTheDispatchersReasonLine ties the page's split-held
// reading to the dispatcher's own skip vocabulary (gt-bgumb7): each row carries
// a line built here from the specdispatch constant that starts it, and only the
// split route's line puts the bead in the split piece. A prefix reworded on
// either side fails here rather than counting split-held beads as ready work.
func TestQueueSplitHeldReadsTheDispatchersReasonLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prefix string
		split  bool
	}{
		{"the split route", specdispatch.SkipSplitOver, true},
		{"a hold", specdispatch.SkipHeld, false},
		{"no seat", specdispatch.SkipNoSeat, false},
		{"a rig hold", specdispatch.SkipRigHold, false},
		{"a startup backoff", specdispatch.SkipStartupBackoff, false},
		{"a live sandbox seat", specdispatch.SkipLiveSeat, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			reason := "gt-spl1t: " + c.prefix + "the tick's text"
			dom := drawQueuePage(t, queueRead(skippedRow("gt-spl1t", "2026-10-29T11:58:00Z", reason, 2)))
			want := "0"
			if c.split {
				want = "1"
			}
			if got := chipNamed(t, queueChips(t, dom), "split"); got.Count != want {
				t.Errorf("a reason beginning %q draws a split count of %q, want %s: the page holds a bead only on the dispatcher's own split line", c.prefix, got.Count, want)
			}
		})
	}
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
			r["labels"] = []string{specdispatch.SplitLabel}
			return r
		}(), "start now"},
		{"another route's skip line", skippedRow("gt-hold1", "2026-10-29T11:58:00Z", "gt-hold1: "+specdispatch.SkipHeld+"overlap with gt-okckk", 3), "skipped by a guard"},
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

// TestQueueHeldLabelsTrackTheDispatchersRules ties HELD_LABELS, the page's copy
// of the labels that hold a ready bead for a person, to the rules that hold one
// (gt-bgumb7). The Go side names the labels through
// dispatch.HumanHoldLabels and the operator, crew-only and held spellings; the
// page names the same ones in its script. Neither list may name a label the
// other does not: a label the page lacks counts a held bead as ready work, and
// a label no rule holds takes dispatchable work out of it.
func TestQueueHeldLabelsTrackTheDispatchersRules(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(readyRow("gt-now1", "ok")))
	var pageLabels []string
	queueReadBack(t, dom, "HELD_LABELS", &pageLabels)

	ruleLabels := append(dispatch.HumanHoldLabels(), dispatch.OperatorLabel, specdispatch.CrewOnlyLabel, heldHoldLabel)
	slices.Sort(ruleLabels)
	pageLabels = slices.Clone(pageLabels)
	slices.Sort(pageLabels)

	for _, label := range ruleLabels {
		if !slices.Contains(pageLabels, label) {
			t.Errorf("HELD_LABELS = %q, want it to name %q: a Go rule holds a bead wearing it, so the page would count it as ready to dispatch", pageLabels, label)
		}
	}
	for _, label := range pageLabels {
		if !slices.Contains(ruleLabels, label) {
			t.Errorf("HELD_LABELS names %q, which no Go rule holds: the page would take dispatchable work out of the ready set", label)
		}
	}
}

// TestQueueHoldLabelledBeadIsNotReadyToDispatch drives every label the page
// declares as one that holds a bead for a person (HELD_LABELS): each keeps its
// bead out of the ready-to-dispatch count, counts it under the held chip, and
// draws a row whose tag names that label (gt-bgumb7).
func TestQueueHoldLabelledBeadIsNotReadyToDispatch(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(readyRow("gt-now1", "ok")))
	var labels []string
	queueReadBack(t, dom, "HELD_LABELS", &labels)
	for _, want := range []string{dispatch.OperatorOnlyLabel, heldHoldLabel} {
		if !slices.Contains(labels, want) {
			t.Fatalf("HELD_LABELS = %q, want it to name %q: a bead labelled it is held by a person, not ready to dispatch", labels, want)
		}
	}

	for _, label := range labels {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			// A bead that lints clean and wears only the hold label: the label
			// is what keeps it off the dispatcher, not its text.
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
			if len(rows) != 1 {
				t.Fatalf("the held chip draws %d rows for a bead labelled %q, want 1", len(rows), label)
			}
			// The row's own verdict is a "held" tag, and its title names the
			// label that holds the bead: the bead id carries "held" too, so a
			// search of the row's text would pass without the tag.
			tag, ok := leafSaying(rows[0], "tag", "held")
			if !ok || tag.Text != "held" {
				t.Fatalf("a bead labelled %q draws %+v under the held chip, want a \"held\" tag", label, rows[0].Leaves)
			}
			if !strings.Contains(tag.Title, strings.ToLower(strings.TrimSpace(label))) {
				t.Errorf("the held row's tag reads %q, want it to name the label %q that holds the bead", tag.Title, label)
			}
		})
	}
}

// TestQueueHeldLabelIgnoresCaseAndSpace draws a bead whose hold label was typed
// by hand: the page matches it as the dispatcher's own rule does, so case and
// surrounding space are not part of the label (gt-bgumb7).
func TestQueueHeldLabelIgnoresCaseAndSpace(t *testing.T) {
	t.Parallel()

	row := readyRow("gt-held2", "ok")
	row["labels"] = []string{"  " + strings.ToUpper(dispatch.OperatorOnlyLabel) + "  "}
	page := drawQueuePage(t, queueRead(row))
	if got := chipNamed(t, queueChips(t, page), "ready to dispatch"); got.Count != "0" {
		t.Errorf("a bead labelled %q draws a ready to dispatch count of %q, want 0", row["labels"], got.Count)
	}
	if got := chipNamed(t, queueChips(t, page), "held"); got.Count != "1" {
		t.Errorf("a bead labelled %q draws %q on the held chip, want 1", row["labels"], got.Count)
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

// TestQueueSplitPieceSumsToTheReadyTotal draws a ready list holding two beads
// the split route holds and one the dispatcher would slot, and reads the header
// back: the piece tiles sum to the ready total, so the split piece is a piece
// of the partition and not a count beside it (gt-xwul0, gt-bgumb7).
func TestQueueSplitPieceSumsToTheReadyTotal(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, queueRead(
		skippedRow("gt-spl1t", "2026-10-29T11:58:00Z", splitHeldReason, 2),
		skippedRow("gt-spl2t", "2026-10-29T11:58:00Z",
			splitRouteLine("gt-spl2t", "estimated 46.0 min (sp:13, rung all), at or over the 30-minute trigger"), 5),
		readyRow("gt-now1", "ok"),
	))

	tiles := queueHeader(t, dom)
	ready, ok := headerTile(tiles, "ready")
	if !ok {
		t.Fatalf("the header draws no ready tile: %+v", tiles)
	}
	want, err := strconv.Atoi(ready.Count)
	if err != nil {
		t.Fatalf("the ready tile reads %q, want a count", ready.Count)
	}

	var pieceLabels []string
	queueReadBack(t, dom, "READY_PIECES.map(p => p.label)", &pieceLabels)
	sum := 0
	for _, tile := range tiles {
		if !slices.Contains(pieceLabels, tile.Label) {
			continue // the ready and landing totals the pieces are read against
		}
		n, err := strconv.Atoi(tile.Count)
		if err != nil {
			t.Fatalf("the %q tile reads %q, want a count", tile.Label, tile.Count)
		}
		sum += n
	}
	if sum != want {
		t.Errorf("the piece tiles sum to %d, want the ready total %d: the split piece is a piece of the partition, not a count beside it", sum, want)
	}
	if got, ok := headerTile(tiles, "held for splitting"); !ok || got.Count != "2" {
		t.Errorf("the split piece reads %+v, want 2 beads held for splitting", got)
	}
}
