package dashboard

import (
	"strings"
	"testing"
)

// The host tile the bead is about: CPU idle leads it, the Dolt server's cores
// and the load average follow. Leading with the load average is what read a
// 24-core host at load 30 — half its CPU idle — as a drowning machine
// (gt-auiji).
func TestMachineTileLeadsWithCPUIdleBesideTheLoad(t *testing.T) {
	t.Parallel()
	page := string(indexHTML)
	if !strings.Contains(page, `tile("Host CPU"`) {
		t.Error("index.html has no Host CPU tile, so the host's idle CPU is on no tile")
	}
	if strings.Contains(page, `tile("Load"`) {
		t.Error("index.html still leads a tile with the load average")
	}
	for _, want := range []string{"m.cpu_idle", "m.dolt_cores", "m.load1"} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html reads no %s, so the host tile carries no %s reading", want, want)
		}
	}
}
