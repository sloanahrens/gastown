package dashboard

import (
	"strings"
	"testing"
)

// The host tile the bead is about: CPU idle leads it, the Dolt server's cores
// and the load average follow. Leading with the load average is what read a
// 24-core host at load 30 — half its CPU idle — as a drowning machine
// (gt-auiji).
//
// How the tile is fed and how it is coloured is Go's, and it is run there:
// hostState decides the colour against a failing reading, and both branches
// it can reach are driven in TestHostStateColoursTheHostByItsIdleShare. What
// is left to pin on this side is the drawing — which reading leads it, what
// an unread host shows, and that the tile is drawn with the others — because
// the page has no JavaScript runtime in this tree, as no pane's does.
func TestHostTileDrawsTheReadingTheReaderTook(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "hostTile")
	// The reading of record leads the tile, and a host that reported none is
	// a dash: an unread host is never a calm one.
	if !strings.Contains(draw, `idle == null ? "–" : Math.round(idle) + "% idle"`) {
		t.Error("hostTile does not lead with the host's idle share, or dashes it when the host reported none")
	}
	// The colour is the verdict the reader decided, drawn as it stands: the
	// tile holds no threshold of its own, and the viewer's core count — which
	// is another machine's — decides nothing. Both of those are what put a
	// thread-inflated load average in charge of the tile (gt-auiji).
	if !strings.Contains(draw, "m.host_state") {
		t.Error("hostTile does not draw the host_state the reader decided")
	}
	for _, own := range []string{"idle <", "idle >", "hardwareConcurrency"} {
		if strings.Contains(draw, own) {
			t.Errorf("hostTile judges the host itself (%q) rather than drawing the reader's verdict", own)
		}
	}
	// The two supporting readings sit beside it, the Dolt server's cores
	// first: it is what the town is burning on the host, and the load average
	// is the number macOS inflates with thread counts.
	for _, want := range []string{
		`m.dolt_cores != null ? "dolt " + m.dolt_cores.toFixed(1) + " cores" : null`,
		`m.load1 != null ? "load " + m.load1.toFixed(1) + (m.cores ? " on " + m.cores + " cores" : "") : null`,
		`m.load5 != null ? "5m " + m.load5.toFixed(1) + " · 15m " + m.load15.toFixed(1) : null`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("hostTile carries no %q", want)
		}
	}
	if d, l := strings.Index(draw, "m.dolt_cores"), strings.Index(draw, "m.load1"); d > l {
		t.Error("hostTile shows the load average ahead of the Dolt server's cores")
	}
	// Drawn with the other tiles, and the load average is on no tile of its
	// own: it is the host's secondary number, not a reading of its own.
	if !strings.Contains(string(indexHTML), "tiles.append(hostTile(s.machine || {}))") {
		t.Error("renderTiles does not draw the host tile")
	}
	if strings.Contains(string(indexHTML), `tile("Load"`) {
		t.Error("index.html still leads a tile with the load average")
	}
}
