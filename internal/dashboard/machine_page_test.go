package dashboard

import (
	"strings"
	"testing"
)

// The strip's host reading: CPU idle leads it, the Dolt server's cores and the
// load average follow. Leading with the load average is what read a 24-core
// host at load 30 — half its CPU idle — as a drowning machine (gt-auiji).
//
// How the host is read and how it is colored is Go's, and it is run there:
// hostState decides the color against a failing reading, and both branches it
// can reach are driven in TestHostStateColoursTheHostByItsIdleShare. What is
// left to pin on this side is the drawing — which reading leads it, what an
// unread host shows, and that one unread reading never takes another with it —
// because the page has no JavaScript runtime in this tree, as no pane's does.
func TestTheStripDrawsTheHostsReadingTheReaderTook(t *testing.T) {
	t.Parallel()

	draw := pageFunc(t, "renderStrip")
	// The reading of record leads it, and a host that reported none is a dash:
	// an unread host is never a calm one.
	if !strings.Contains(draw, `m.cpu_idle == null ? "–" : Math.round(m.cpu_idle) + "%"`) {
		t.Error("the strip does not lead with the host's idle share, or dashes it when the host reported none")
	}
	// The color is the verdict the reader decided, drawn as it stands: the
	// strip holds no threshold of its own, and the viewer's core count — which
	// is another machine's — decides nothing (gt-auiji).
	if !strings.Contains(draw, `cls: m.cpu_idle == null ? "" : (m.host_state || "v")`) {
		t.Error("the strip does not draw the host_state the reader decided")
	}
	for _, own := range []string{"idle <", "idle >", "hardwareConcurrency"} {
		if strings.Contains(draw, own) {
			t.Errorf("the strip judges the host itself (%q) rather than drawing the reader's verdict", own)
		}
	}
	// The load average is the host's secondary number and each of its three
	// readings is guarded on its own, so one the host did not report never
	// takes the others down with it.
	for _, want := range []string{
		`m.dolt_cores != null ? "dolt " + m.dolt_cores.toFixed(1) + " cores" : null`,
		`m.load1 == null ? null : "load " + m.load1.toFixed(1) + (m.cores ? " on " + m.cores + " cores" : "")`,
		`m.load5 == null ? null : "5m " + m.load5.toFixed(1)`,
		`m.load15 == null ? null : "15m " + m.load15.toFixed(1)`,
	} {
		if !strings.Contains(draw, want) {
			t.Errorf("the strip carries no %q", want)
		}
	}
	if d, l := strings.Index(draw, "m.dolt_cores"), strings.Index(draw, "m.load1"); d > l {
		t.Error("the strip shows the load average ahead of the Dolt server's cores")
	}
	// The core count is the reader's, so the load average is read against the
	// host that took it rather than the one the page is open on.
	if !strings.Contains(draw, `" on " + m.cores + " cores"`) {
		t.Error("the strip reads the load average against a core count of its own")
	}
	// The tiles are gone, so the reading has one place to be drawn.
	if strings.Contains(string(indexHTML), `id="tiles"`) || strings.Contains(string(indexHTML), "hostTile") {
		t.Error("index.html still draws the tiles the strip replaced")
	}
}
