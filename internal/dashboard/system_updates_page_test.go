package dashboard

import (
	"slices"
	"strings"
	"testing"
)

// The System updates pane draws the feed's joined section: one row per landed
// commit, newest first, a chip per pipeline stage. The feed hands the rows over
// joined and sorted (system_updates.go), so these tests drive the page's own
// render function and read back what it drew.

func systemUpdatesState(unavailable bool, rows ...any) map[string]any {
	return map[string]any{"system_updates": map[string]any{"rows": rows, "unavailable": unavailable}}
}

func systemUpdateStage(tier, verdict string) map[string]any {
	return map[string]any{"tier": tier, "verdict": verdict}
}

func systemUpdateRun(workflow, status string) map[string]any {
	return map[string]any{"workflow": workflow, "status": status}
}

// systemUpdateRowText is one row's six chips as "stage=colour", in draw order.
func systemUpdateRowText(t *testing.T, dom *pageDOM, row int) string {
	t.Helper()
	var out []string
	for i := 0; i < 6; i++ {
		chip := dom.descend(t, "systemupdates", row, i+1)
		out = append(out, strings.Fields(chip.Text)[0]+"="+strings.TrimPrefix(chip.Class, "tag "))
	}
	return strings.Join(out, " ")
}

// One row per commit, newest first, six stage chips in pipeline order, coloured
// green for a pass, red for a failure, amber while it runs, grey for a stage the
// commit never reached.
func TestSystemUpdatesPageDrawsAStageChipRowPerCommit(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.call(t, "renderSystemUpdates", systemUpdatesState(false,
		map[string]any{
			"sha":   "abcdef123456",
			"sweep": map[string]any{"stages": []any{systemUpdateStage("shell", "GREEN"), systemUpdateStage("integration", "RED"), systemUpdateStage("race", "GREEN")}},
			"runs":  []any{systemUpdateRun("post-land.yml", "running"), systemUpdateRun("images.yml", "success")},
			"roll":  map[string]any{"outcome": "ok"},
		},
		map[string]any{
			"sha":   "123456789abc",
			"sweep": map[string]any{"stages": []any{systemUpdateStage("shell", "GREEN")}},
			"runs":  []any{systemUpdateRun("post-land.yml", "success"), systemUpdateRun("images.yml", "failure")},
		}))
	if got := dom.element(t, "systemupdates").Kids; got != 2 {
		t.Fatalf("the pane drew %d rows, want one per commit", got)
	}
	for row, want := range []string{
		"post-land=warnc shell=good integration=badc race=good images=good roll=good",
		"post-land=good shell=good integration=absent race=absent images=badc roll=absent",
	} {
		if got := systemUpdateRowText(t, dom, row); got != want {
			t.Errorf("row %d reads %q, want %q", row, got, want)
		}
	}
	for row, sha := range []string{"abcdef123456", "123456789abc"} {
		if got := dom.descend(t, "systemupdates", row, 0).Text; got != sha {
			t.Errorf("row %d names %q, want the feed's %q", row, got, sha)
		}
	}
}

// A stage missing while a source behind the section could not be read is
// unknown, not absent: the run may have happened and its record been lost.
func TestSystemUpdatesPageDrawsAnUnreadableSourceAsUnknown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		unavailable bool
		cls, word   string
	}{{false, "absent", "not run"}, {true, "warnc", "not read"}} {
		dom := newPageDOM(t)
		dom.call(t, "renderSystemUpdates", systemUpdatesState(tc.unavailable, map[string]any{"sha": "abcdef123456"}))
		for i := 0; i < 6; i++ {
			chip := dom.descend(t, "systemupdates", 0, i+1)
			if !strings.Contains(" "+chip.Class+" ", " "+tc.cls+" ") || !strings.Contains(chip.Title, tc.word) {
				t.Errorf("unavailable=%v chip %d wears %q titled %q, want %q and %q", tc.unavailable, i, chip.Class, chip.Title, tc.cls, tc.word)
			}
		}
	}
}

// The pane is a new fold directly above Tier sweeps, which stays put, so Beads
// keeps sitting directly under Tier sweeps. The push table and the redraw both
// draw it.
func TestSystemUpdatesPageSitsDirectlyAboveTierSweeps(t *testing.T) {
	t.Parallel()

	left, right := pageColumnKeys(t)
	if got := paneAbove(t, left, "tiersweep"); got != "systemupdates" {
		t.Errorf("the pane above Tier sweeps is %q, want systemupdates", got)
	}
	if got := paneAbove(t, left, "beads"); got != "tiersweep" {
		t.Errorf("the pane above Beads is %q, want Tier sweeps", got)
	}
	if slices.Contains(right, "systemupdates") {
		t.Error("the System updates pane is also in the right column")
	}
	dom := newPageDOM(t)
	if got := pageExpr(t, dom, `PANES.systemupdates.sec === "systemupdates" && PANES.systemupdates.body.join(",") === "systemupdates" && renderAll.toString().indexOf("renderSystemUpdates(state)") >= 0`); got != "true" {
		t.Error("the page's push table and redraw do not draw the System updates pane")
	}
}
