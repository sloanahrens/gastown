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
// The tile's own body is asserted rather than measured: the page has no
// JavaScript runtime in this tree, so a tile's rule is pinned by the branches
// it writes, which is the seam every other pane is tested at. What those
// branches are *fed* is Go's, and that is measured: see
// TestSampleMachineReportsAFailedHost.
func TestMachineTileLeadsWithCPUIdleBesideTheLoad(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "hostTile")
	// The reading of record is the host's own idle CPU, and the tile is a
	// dash — uncoloured, never green — when the host reported none.
	if !strings.Contains(draw, `const idle = m.cpu_idle;`) {
		t.Error("hostTile leads with no CPU idle reading")
	}
	if !strings.Contains(draw, `idle == null ? "–" : Math.round(idle) + "% idle"`) {
		t.Error("hostTile does not show the idle share, and a dash when the host reported none")
	}
	// The two supporting readings, the Dolt server's cores first: it is what
	// the town is burning on the host, and the load average is the number
	// macOS inflates with thread counts.
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
	// Every branch of the colour rule, including the one a host that read
	// nothing takes: the tile is amber below 20% idle and red below 5%.
	if !strings.Contains(draw, `idle == null ? "" : idle < 5 ? "bad" : idle < 20 ? "warn" : ""`) {
		t.Error("hostTile does not colour the tile by the host's idle reading, with no colour for an unread host")
	}
	// Drawn with the other tiles, and the load average is on no tile of its
	// own: it is the host's secondary number, not a reading of its own.
	if !strings.Contains(string(indexHTML), "tiles.append(hostTile(s.machine || {}))") {
		t.Error("renderTiles does not draw the host tile")
	}
	if strings.Contains(string(indexHTML), `tile("Load"`) {
		t.Error("index.html still leads a tile with the load average")
	}
	// The colour was once the load against navigator.hardwareConcurrency, the
	// viewer's core count: another machine's, and unrelated to this host's.
	if strings.Contains(string(indexHTML), "hardwareConcurrency") {
		t.Error("index.html still colours the host by the viewer's core count")
	}
}
