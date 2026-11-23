package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The System updates pane draws the feed's joined section: one row per landed
// commit, newest first, a chip per pipeline stage. The feed hands the rows over
// joined and sorted (system_updates.go), so these tests drive the page's own
// render function and read back what it drew.

// systemUpdatesSources is the section's account of which source behind the rows
// could not be read: the whole join, the roll's own record, or the sweep.
type systemUpdatesSources struct {
	unavailable  bool
	rollUnknown  bool
	sweepUnknown bool
}

func systemUpdatesState(src systemUpdatesSources, rows ...any) map[string]any {
	su := map[string]any{"rows": rows}
	if src.unavailable {
		su["unavailable"] = true
	}
	if src.rollUnknown {
		su["roll_unknown"] = true
	}
	if src.sweepUnknown {
		su["sweep_unknown"] = true
	}
	return map[string]any{"system_updates": su}
}

func systemUpdateStage(tier, verdict string) map[string]any {
	return map[string]any{"tier": tier, "verdict": verdict}
}

func systemUpdateRun(workflow, status string) map[string]any {
	return map[string]any{"workflow": workflow, "status": status}
}

// systemUpdateGoodRow is one commit every stage of the pipeline passed, so a test
// can break one stage and read the chip it changed.
func systemUpdateGoodRow() map[string]any {
	return map[string]any{
		"sha":   "abcdef123456",
		"sweep": map[string]any{"stages": []any{systemUpdateStage("shell", "GREEN"), systemUpdateStage("integration", "GREEN"), systemUpdateStage("race", "GREEN")}},
		"runs":  []any{systemUpdateRun("post-land.yml", "success"), systemUpdateRun("images.yml", "success")},
		"roll":  map[string]any{"outcome": "ok"},
	}
}

// systemUpdateRowText is one row's six stage chips as "stage=colour", in draw
// order. The pair chip between images and roll is the seventh and is read by
// the pair tests, not here.
func systemUpdateRowText(t *testing.T, dom *pageDOM, row int) string {
	t.Helper()
	var out []string
	for i := 1; i <= 7; i++ {
		chip := dom.descend(t, "systemupdates", row, i)
		if strings.HasPrefix(chip.Text, "pair ") {
			continue
		}
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
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{},
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
		dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{unavailable: tc.unavailable}, map[string]any{"sha": "abcdef123456"}))
		for i := 1; i <= 7; i++ {
			chip := dom.descend(t, "systemupdates", 0, i)
			if strings.HasPrefix(chip.Text, "pair ") {
				continue
			}
			if !strings.Contains(" "+chip.Class+" ", " "+tc.cls+" ") || !strings.Contains(chip.Title, tc.word) {
				t.Errorf("unavailable=%v chip %d wears %q titled %q, want %q and %q", tc.unavailable, i, chip.Class, chip.Title, tc.cls, tc.word)
			}
		}
	}
}

// An unreadable roll or sweep source marks its own stages unknown, not absent,
// without touching the other source's: the section's whole-join flag is not the
// only signal that a record was lost.
func TestSystemUpdatesPageDrawsAnUnreadableRollOrSweepAsUnknown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		src  systemUpdatesSources
		want string
	}{
		{"roll source", systemUpdatesSources{rollUnknown: true},
			"post-land=absent shell=absent integration=absent race=absent images=absent roll=warnc"},
		{"sweep source", systemUpdatesSources{sweepUnknown: true},
			"post-land=absent shell=warnc integration=warnc race=warnc images=absent roll=absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", systemUpdatesState(tc.src, map[string]any{"sha": "abcdef123456"}))
			if got := systemUpdateRowText(t, dom, 0); got != tc.want {
				t.Errorf("%s: row reads %q, want %q", tc.name, got, tc.want)
			}
			for _, i := range []int{1, 2, 3, 4, 5, 7} {
				chip := dom.descend(t, "systemupdates", 0, i)
				if strings.Contains(chip.Class, "warnc") && !strings.Contains(chip.Title, "not read") {
					t.Errorf("%s: chip %d is unknown but titled %q, want a lost source", tc.name, i, chip.Title)
				}
			}
		})
	}
}

// A status, verdict or outcome the pane does not recognise is failed, never
// passed: an unknown reading is not a clean one.
func TestSystemUpdatesPageDrawsAnUnknownStatusAsFailed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		row  func() map[string]any
		chip int
		want string
	}{
		{"run status", func() map[string]any {
			r := systemUpdateGoodRow()
			r["runs"] = []any{systemUpdateRun("post-land.yml", "mystery"), systemUpdateRun("images.yml", "success")}
			return r
		}, 0, "post-land=badc"},
		{"sweep verdict", func() map[string]any {
			r := systemUpdateGoodRow()
			r["sweep"] = map[string]any{"stages": []any{systemUpdateStage("shell", "MURKY"), systemUpdateStage("integration", "GREEN"), systemUpdateStage("race", "GREEN")}}
			return r
		}, 1, "shell=badc"},
		{"roll outcome", func() map[string]any {
			r := systemUpdateGoodRow()
			r["roll"] = map[string]any{"outcome": "mystery"}
			return r
		}, 5, "roll=badc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, tc.row()))
			if got := strings.Split(systemUpdateRowText(t, dom, 0), " ")[tc.chip]; got != tc.want {
				t.Errorf("%s: chip reads %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// The pane says which of the two empty states it is in — no read yet versus a
// read with no landed commits — and counts the rows it drew.
func TestSystemUpdatesPageDrawsTheEmptyStateAndCountNote(t *testing.T) {
	t.Parallel()

	unread := newPageDOM(t)
	unread.call(t, "renderSystemUpdates", map[string]any{})
	if got := unread.element(t, "systemupdates").Kids; got != 1 {
		t.Fatalf("no section drew %d lines, want the reading placeholder", got)
	}
	if got := unread.descend(t, "systemupdates", 0).Text; got != "reading the system updates…" {
		t.Errorf("no section reads %q, want the reading placeholder", got)
	}
	if got := unread.element(t, "systemupdatesnote").Text; got != "" {
		t.Errorf("no section shows the note %q, want none", got)
	}

	empty := newPageDOM(t)
	empty.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}))
	if got := empty.descend(t, "systemupdates", 0).Text; got != "no landed commits yet" {
		t.Errorf("an empty read says %q, want no landed commits", got)
	}

	for _, tc := range []struct {
		rows int
		want string
	}{{1, "1 commit"}, {2, "2 commits"}} {
		dom := newPageDOM(t)
		rows := make([]any, tc.rows)
		for i := range rows {
			rows[i] = map[string]any{"sha": "abcdef123456"}
		}
		dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{}, rows...))
		if got := dom.element(t, "systemupdatesnote").Text; got != tc.want {
			t.Errorf("%d rows show the note %q, want %q", tc.rows, got, tc.want)
		}
	}
}

// The pane is a fold in the left column, directly above Beads
// (TestBeadsPaneSitsDirectlyUnderSystemUpdates reads that pair). The page's
// push table points the section at the pane, and its redraw draws it.
func TestSystemUpdatesPageIsALeftColumnFold(t *testing.T) {
	t.Parallel()

	left, right := pageColumnKeys(t)
	if !slices.Contains(left, "systemupdates") {
		t.Errorf("the left column holds %v, with no System updates pane", left)
	}
	if slices.Contains(right, "systemupdates") {
		t.Error("the System updates pane is also in the right column")
	}

	dom := newPageDOM(t)
	if got := pageExpr(t, dom, `PANES.systemupdates.sec + "|" + PANES.systemupdates.body.join(",")`); got != "systemupdates|systemupdates" {
		t.Errorf("the push table points %q, want the systemupdates section and body", got)
	}
	raw, err := json.Marshal(systemUpdatesState(systemUpdatesSources{}, map[string]any{"sha": "abcdef123456"}))
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	// The section folds like the rest of the grid, so a reader opens it before
	// the redraw draws it, the way it starts closed on a fresh load.
	dom.press(t, "systemupdates", "click")
	if _, err := dom.vm.RunString("state = " + string(raw) + "; renderAll();"); err != nil {
		t.Fatalf("drawing the reading: %v", err)
	}
	if got := dom.element(t, "systemupdates").Kids; got != 1 {
		t.Errorf("the redraw drew %d rows, want the one row of the reading", got)
	}
}
