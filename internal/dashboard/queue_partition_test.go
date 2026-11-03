package dashboard

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Beads panel's header and its chips read one partition of the ready list
// (gt-xwul0): a bead is in exactly one piece, the pieces add up to the ready
// total, and each piece says what it counts. The tests below draw the page over
// a reading holding one bead of every piece and read the drawn header and chips
// back, so a renderer that mentions a piece in its source but draws nothing
// still fails.

// queueHeaderTile is one tile of the Beads panel's header (the wsum row): the
// count, the words it counts, and the tooltip that says what the piece means.
type queueHeaderTile struct {
	Count string `json:"count"`
	Label string `json:"label"`
	Title string `json:"title"`
}

// queueHeader reads the header row the panel drew, in the order it drew it.
func queueHeader(t *testing.T, dom *pageDOM) []queueHeaderTile {
	t.Helper()

	var out []queueHeaderTile
	queueReadBack(t, dom, `(() => {
		const sum = __byId["beads"].children.__arr.find(c => String(c.className).split(" ").indexOf("wsum") >= 0);
		if (!sum) return [];
		return sum.children.__arr.map(x => ({count: String(x.children[0].textContent), label: String(x.children[1].textContent), title: String(x.title || "")}));
	})()`, &out)
	if len(out) == 0 {
		t.Fatal("the panel drew no queue header")
	}
	return out
}

// headerTile finds the header tile that counts under label, exactly. A tile the
// page renamed fails the test that reads it rather than reading as absent.
func headerTile(tiles []queueHeaderTile, label string) (queueHeaderTile, bool) {
	for _, x := range tiles {
		if x.Label == label {
			return x, true
		}
	}
	return queueHeaderTile{}, false
}

// partitionQueue is a ready list holding one bead of every piece the panel
// partitions ready work into: one the dispatcher would slot, one waiting for a
// seat, one its last tick passed over for a guard's reason, one needing
// shaping, one held by an assignee and one in the town store.
func partitionQueue() map[string]any {
	return queueRead(
		readyRow("gt-now", "ok"),
		skippedRow("gt-seat", "2026-10-29T11:58:00Z", "no seat: over the town total", 4),
		skippedRow("gt-guard", "2026-10-29T11:58:00Z", "hold: ~/gt/seat-refill.hold is present", 3),
		readyRow("gt-fix", "fix"),
		readyRow("gt-held", "held"),
		townRow("hq-town", "ok"),
	)
}

// partitionPieces is the six pieces in the order both the header and the chips
// must draw them, with the word each one counts under in each row: the header
// says "need shaping" and the chip kept its longer "needs shaping".
var partitionPieces = []struct {
	piece, chip string
	count       int
}{
	{"start now", "start now", 1},
	{"waiting for a seat", "waiting for a seat", 1},
	{"skipped by a guard", "skipped by a guard", 1},
	{"need shaping", "needs shaping", 1},
	{"held", "held", 1},
	{"town store", "town store", 1},
}

// chipExact finds one chip by its whole label. The town store draws two chips
// with that prefix — the store chip and the partition's piece chip — so a test
// naming the piece must not settle for the first chip that starts with it.
func chipExact(t *testing.T, chips []queueChipDrawn, label string) queueChipDrawn {
	t.Helper()

	for _, c := range chips {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("the queue draws no chip labelled exactly %q; it draws %+v", label, chips)
	return queueChipDrawn{}
}

// clickQueueChipExact presses the chip whose label is exactly label: the
// town store's piece chip is the second chip that starts with "town store".
func clickQueueChipExact(t *testing.T, dom *pageDOM, label string) {
	t.Helper()

	if _, err := dom.vm.RunString(`(() => {
		let hit = null;
		const box = __qBox();
		if (box) __walk(box, n => { if (!hit && (" " + n.className + " ").indexOf(" chip ") >= 0 && String(n._text || "") === ` + quoteJS(label) + `) hit = n; });
		if (!hit) throw new Error("the queue draws no chip with this label");
		__click(hit);
	})()`); err != nil {
		t.Fatalf("pressing the %q chip: %v", label, err)
	}
}

// TestQueuePartitionCoversEveryReadyBeadOnce draws a ready list holding one
// bead of every piece and reads the header back: the six piece counts are the
// fixture's own, they sum to the ready total, and the chips count the same
// beads. A bead counted in two pieces (or in none) makes the sum wrong, and a
// chip that counts differently from the header splits the one partition into
// two (gt-xwul0).
func TestQueuePartitionCoversEveryReadyBeadOnce(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, partitionQueue())
	tiles := queueHeader(t, dom)
	chips := queueChips(t, dom)

	sum := 0
	for _, w := range partitionPieces {
		tile, ok := headerTile(tiles, w.piece)
		if !ok {
			t.Fatalf("the header counts no %q piece: %+v", w.piece, tiles)
		}
		n, err := strconv.Atoi(tile.Count)
		if err != nil {
			t.Fatalf("the %q piece reads %q, want a count", w.piece, tile.Count)
		}
		if n != w.count {
			t.Errorf("the %q piece counts %d, want %d", w.piece, n, w.count)
		}
		sum += n
	}
	if sum != 6 {
		t.Errorf("the six pieces sum to %d, want the ready total 6: one bead of each, none counted twice", sum)
	}
	if ready, ok := headerTile(tiles, "ready"); !ok || ready.Count != "6" {
		t.Errorf("the header reads the ready total as %+v, want 6 beside the pieces", ready)
	}

	// The chips read the same six counts: the header and the chips are one
	// partition, not two counts of the same list.
	for _, w := range partitionPieces {
		got := chipNamed(t, chips, w.chip)
		if w.chip == "town store" {
			got = chipExact(t, chips, w.chip)
		}
		if got.Count != strconv.Itoa(w.count) {
			t.Errorf("the %q chip counts %s, want %d", w.chip, got.Count, w.count)
		}
	}
}

// TestQueueChipsSumToTheReadyTotal draws the same partition and reads the chips
// back: their six counts sum to the ready total, each chip filters the list to
// exactly its own beads, and a second click clears it. The town store's piece
// chip lists the town's ready beads, as the store chip beside it does; the
// shape chips never count them (gt-xwul0).
func TestQueueChipsSumToTheReadyTotal(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, partitionQueue())
	chips := queueChips(t, dom)

	cases := []struct {
		chip  string
		exact bool // the town store draws a store chip with the same prefix
		rows  []string
	}{
		{"start now", false, []string{"gt-now"}},
		{"waiting for a seat", false, []string{"gt-seat"}},
		{"skipped by a guard", false, []string{"gt-guard"}},
		{"needs shaping", false, []string{"gt-fix"}},
		{"held", false, []string{"gt-held"}},
		{"town store", true, []string{"hq-town"}},
	}
	sum := 0
	for _, c := range cases {
		got := chipNamed(t, chips, c.chip)
		if c.exact {
			got = chipExact(t, chips, c.chip)
		}
		n, err := strconv.Atoi(got.Count)
		if err != nil {
			t.Fatalf("the %q chip reads a count that is not a number", c.chip)
		}
		sum += n
	}
	if sum != 6 {
		t.Errorf("the six chips sum to %d, want the ready total 6", sum)
	}

	// A cleared shape filter lists every ready bead outside the town store, in
	// the reading's own order.
	unfiltered := []string{"gt-now", "gt-seat", "gt-guard", "gt-fix", "gt-held"}
	for _, c := range cases {
		if c.exact {
			clickQueueChipExact(t, dom, c.chip)
		} else {
			clickQueueChip(t, dom, c.chip)
		}
		if got := queueRowIDs(t, dom); !slices.Equal(got, c.rows) {
			t.Errorf("the %q chip lists %q, want %q", c.chip, got, c.rows)
		}
		if c.exact {
			clickQueueChipExact(t, dom, c.chip)
		} else {
			clickQueueChip(t, dom, c.chip)
		}
		if got := queueRowIDs(t, dom); !slices.Equal(got, unfiltered) {
			t.Errorf("a second click on %q lists %q, want the shape filter cleared", c.chip, got)
		}
	}

	// The store chip the view has always drawn still lists the town's beads:
	// the piece chip adds a count, it does not replace the store's own view.
	clickQueueChip(t, dom, "town store, not dispatched")
	if got := queueRowIDs(t, dom); !slices.Equal(got, []string{"hq-town"}) {
		t.Errorf("the town store's chip lists %q, want the town's bead", got)
	}
}

// TestQueueHeaderNamesWhatItCounts draws the partition and reads the header
// back as a whole: the tiles are exactly the six pieces in the partition's
// order and then the ready and landing totals, each piece's tooltip names the
// rule it counts, and a piece counting nothing is left out rather than drawn
// as a zero (gt-xwul0). Reading the whole row, not one label at a time, is what
// catches a second tile counting a piece the header already names.
func TestQueueHeaderNamesWhatItCounts(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, partitionQueue())
	tiles := queueHeader(t, dom)

	want := []struct{ count, piece, rule string }{
		{"1", "start now", "slot"},
		{"1", "waiting for a seat", "seat"},
		{"1", "skipped by a guard", "passed over"},
		{"1", "need shaping", "shaping"},
		{"1", "held", "assignee"},
		{"1", "town store", "town's own store"},
		{"6", "ready", "nothing blocking"},
		{"0", "landing", "landing worker"},
	}
	if len(tiles) != len(want) {
		t.Fatalf("the header draws %d tiles %+v, want exactly the six pieces and the two totals %+v", len(tiles), tiles, want)
	}
	for i, w := range want {
		got := tiles[i]
		if got.Label != w.piece || got.Count != w.count {
			t.Errorf("header tile %d reads %s %q, want %s %q", i, got.Count, got.Label, w.count, w.piece)
		}
		if !strings.Contains(strings.ToLower(got.Title), w.rule) {
			t.Errorf("the %q tile's tooltip reads %q, want it to name its rule (%q)", w.piece, got.Title, w.rule)
		}
	}

	// A piece counting nothing is left out, not drawn as a zero: a ready list
	// with one start-now bead names that piece alone, then the totals.
	dom = drawQueuePage(t, queueRead(readyRow("gt-now", "ok")))
	tiles = queueHeader(t, dom)
	want = []struct{ count, piece, rule string }{
		{"1", "start now", "slot"},
		{"1", "ready", "nothing blocking"},
		{"0", "landing", "landing worker"},
	}
	if len(tiles) != len(want) {
		t.Fatalf("a one-piece header draws %+v, want the non-empty piece and the two totals", tiles)
	}
	for i, w := range want {
		if got := tiles[i]; got.Label != w.piece || got.Count != w.count {
			t.Errorf("header tile %d reads %s %q, want %s %q", i, got.Count, got.Label, w.count, w.piece)
		}
	}
}

// TestQueueTownPieceClearsWithTheRig draws the partition, opens the town store's
// piece chip and then picks a single rig: the all-stores view's town piece is
// the one filter a single rig cannot satisfy and whose chip that view no longer
// draws, so picking a rig clears it rather than leaving the list empty with no
// chip to clear (gt-xwul0).
func TestQueueTownPieceClearsWithTheRig(t *testing.T) {
	t.Parallel()

	dom := drawQueuePage(t, partitionQueue())
	clickQueueChipExact(t, dom, "town store")
	if got := queueRowIDs(t, dom); !slices.Equal(got, []string{"hq-town"}) {
		t.Fatalf("the town store piece chip lists %q, want the town's bead", got)
	}

	clickQueueChip(t, dom, "gastown")
	if got, want := queueRowIDs(t, dom), []string{"gt-now", "gt-seat", "gt-guard", "gt-fix", "gt-held"}; !slices.Equal(got, want) {
		t.Errorf("after picking a rig the list reads %q, want every ready bead in it %q", got, want)
	}
	for _, c := range queueChips(t, dom) {
		if c.Label == "town store" {
			t.Errorf("the single-rig view still draws the town store's piece chip %+v", c)
		}
	}
}
