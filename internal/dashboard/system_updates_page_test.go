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
// could not be read: the whole join, the roll's own record, the sweep, or the
// pipeline read that held no earlier run.
type systemUpdatesSources struct {
	unavailable  bool
	rollUnknown  bool
	sweepUnknown bool
	runsUnknown  bool
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
	if src.runsUnknown {
		su["runs_unknown"] = true
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

// An unreadable roll, sweep or pipeline-run source marks its own stages
// unknown, not absent, without touching the other sources': the section's
// whole-join flag is not the only signal that a record was lost.
func TestSystemUpdatesPageDrawsAnUnreadableRecordSourceAsUnknown(t *testing.T) {
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
		{"run source", systemUpdatesSources{runsUnknown: true},
			"post-land=warnc shell=absent integration=absent race=absent images=warnc roll=absent"},
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

// The header says what the pane holds: the rows it drew, and when the row cap
// cut commits, how many older ones are not shown. The count is of commits the
// join knows, so it reads "older not shown", never "undeployed" (gt-3bhxmy).
func TestSystemUpdatesPageDrawsTheCutCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rows int
		more int
		set  bool
		want string
	}{
		{name: "three cut", rows: 6, more: 3, set: true, want: "6 commits · 3 older not shown"},
		{name: "one older not shown", rows: 1, more: 1, set: true, want: "1 commit · 1 older not shown"},
		{name: "nothing cut", rows: 6, more: 0, set: true, want: "6 commits"},
		{name: "more absent", rows: 6, want: "6 commits"},
		{name: "one row", rows: 1, want: "1 commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows := make([]any, tc.rows)
			for i := range rows {
				rows[i] = map[string]any{"sha": "abcdef123456"}
			}
			state := systemUpdatesState(systemUpdatesSources{}, rows...)
			if tc.set {
				state["system_updates"].(map[string]any)["more"] = tc.more
			}
			dom := newPageDOM(t)
			dom.call(t, "renderSystemUpdates", state)
			if got := dom.element(t, "systemupdatesnote").Text; got != tc.want {
				t.Errorf("%s: the header reads %q, want %q", tc.name, got, tc.want)
			}
		})
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

// systemUpdateRowLeaf reads back a row's leaf whose text names the given stage,
// so a test is not tied to where the row's own time sits among the chips.
func systemUpdateRowLeaf(t *testing.T, dom *pageDOM, row int, stage string) drawn {
	t.Helper()

	n := dom.descend(t, "systemupdates", row).Kids
	for i := 0; i < n; i++ {
		leaf := dom.descend(t, "systemupdates", row, i)
		if strings.HasPrefix(leaf.Text, stage+" ") {
			return leaf
		}
	}
	t.Fatalf("row %d draws no %s chip", row, stage)
	return drawn{}
}

// Each row draws when it last changed right after its sha — the viewer's own
// 24-hour clock, with the feed's full stamp and its age in the title — and every
// stage chip's title names that stage's own time, not the row's, so a row whose
// order looks wrong can be read against the times its stages reported
// (gt-okg1kq).
func TestSystemUpdatesPageDrawsEachRowsTime(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	const (
		rowAt     = "2026-11-24T18:50:00Z"
		sweepAt   = "2026-11-24T18:47:00Z"
		postLand  = "2026-11-24T18:44:00Z"
		imagesEnd = "2026-11-24T18:49:00Z"
		rollEnd   = "2026-11-24T18:50:00Z"
	)
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{},
		map[string]any{
			"sha":   "ee9028c0c814",
			"at":    rowAt,
			"sweep": map[string]any{"at": sweepAt, "stages": []any{systemUpdateStage("shell", "GREEN"), systemUpdateStage("integration", "GREEN"), systemUpdateStage("race", "GREEN")}},
			"runs": []any{
				map[string]any{"workflow": "post-land.yml", "status": "success", "started": "2026-11-24T18:43:00Z", "ended": postLand},
				map[string]any{"workflow": "images.yml", "status": "success", "started": "2026-11-24T18:48:00Z", "ended": imagesEnd},
			},
			"roll": map[string]any{"outcome": "ok", "started_at": "2026-11-24T18:48:30Z", "ended_at": rollEnd},
		}))

	// The row is the sha, its time, the six stage chips and the pair chip.
	row := dom.descend(t, "systemupdates", 0)
	if row.Kids != 9 {
		t.Fatalf("the row drew %d children, want the sha, its time, six stage chips and the pair chip", row.Kids)
	}
	at := dom.descend(t, "systemupdates", 0, 1)
	wantClock := pageExpr(t, dom, "clock("+quoteJS(rowAt)+").slice(0, 5)")
	if at.Text != wantClock {
		t.Errorf("the row's time reads %q, want the viewer's local %q", at.Text, wantClock)
	}
	if !strings.Contains(at.Title, rowAt) {
		t.Errorf("the row's time is titled %q, want the feed's full stamp %q in it", at.Title, rowAt)
	}
	if wantAge := pageExpr(t, dom, "age("+quoteJS(rowAt)+")"); !strings.Contains(at.Title, wantAge+" ago") {
		t.Errorf("the row's time is titled %q, want how long ago it changed (%q) in it", at.Title, wantAge+" ago")
	}

	for stage, want := range map[string]string{
		"post-land":   postLand,
		"shell":       sweepAt,
		"integration": sweepAt,
		"race":        sweepAt,
		"images":      imagesEnd,
		"roll":        rollEnd,
	} {
		chip := systemUpdateRowLeaf(t, dom, 0, stage)
		if !strings.Contains(chip.Title, want) {
			t.Errorf("the %s chip is titled %q, want that stage's own time %q in it", stage, chip.Title, want)
		}
	}
}

// A time the feed uses for "not yet" draws nothing: a running run reports the
// epoch as its end, so its chip names when it started and never 1970, and a row
// whose at is the zero time or missing draws no time at all (gt-okg1kq).
func TestSystemUpdatesPageDrawsNoEpochTime(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	const started = "2026-11-24T18:44:00Z"
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{},
		map[string]any{
			"sha":  "ee9028c0c814",
			"runs": []any{map[string]any{"workflow": "post-land.yml", "status": "running", "started": started, "ended": "1970-01-01T00:00:00Z"}},
		},
		map[string]any{"sha": "aaaaaaaaaaaa", "at": "0001-01-01T00:00:00Z"},
		map[string]any{"sha": "123456789abc"}))

	chip := systemUpdateRowLeaf(t, dom, 0, "post-land")
	if !strings.Contains(chip.Title, started) {
		t.Errorf("row 0: the post-land chip is titled %q, want the running run's start %q in it", chip.Title, started)
	}
	n := dom.descend(t, "systemupdates", 0).Kids
	for i := 0; i < n; i++ {
		leaf := dom.descend(t, "systemupdates", 0, i)
		if strings.Contains(leaf.Text, "1970") || strings.Contains(leaf.Title, "1970") {
			t.Errorf("row 0: leaf %d reads %q titled %q, want the epoch time never drawn", i, leaf.Text, leaf.Title)
		}
	}
	// Rows 1 and 2 carry the zero time and no at, so their first chip follows
	// their sha directly and no leaf names year 1.
	for _, row := range []int{1, 2} {
		if second := dom.descend(t, "systemupdates", row, 1); !strings.HasPrefix(second.Text, "post-land ") {
			t.Errorf("row %d has no real at, so its first chip should follow its sha; leaf 1 reads %q", row, second.Text)
		}
		for i := 0; i < dom.descend(t, "systemupdates", row).Kids; i++ {
			leaf := dom.descend(t, "systemupdates", row, i)
			if strings.Contains(leaf.Text, "0001") || strings.Contains(leaf.Title, "0001") {
				t.Errorf("row %d: leaf %d reads %q titled %q, want the zero time never drawn", row, i, leaf.Text, leaf.Title)
			}
		}
	}
}

// A stage the row holds no record for names no time, and a sweep's at belongs
// only to the tiers that sweep recorded: a chip for a stage that never ran is
// not dated by another stage's clock (gt-okg1kq).
func TestSystemUpdatesPageDrawsNoTimeForAStageWithoutARecord(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	const sweepAt = "2026-11-24T18:47:00Z"
	dom.call(t, "renderSystemUpdates", systemUpdatesState(systemUpdatesSources{},
		map[string]any{
			"sha":   "ee9028c0c814",
			"sweep": map[string]any{"at": sweepAt, "stages": []any{systemUpdateStage("shell", "GREEN")}},
		}))

	if chip := systemUpdateRowLeaf(t, dom, 0, "shell"); !strings.Contains(chip.Title, sweepAt) {
		t.Errorf("the shell chip is titled %q, want the sweep's time %q in it", chip.Title, sweepAt)
	}
	for _, stage := range []string{"integration", "race", "post-land", "images", "roll"} {
		chip := systemUpdateRowLeaf(t, dom, 0, stage)
		if strings.Contains(chip.Title, sweepAt) || strings.Contains(chip.Title, " ago") {
			t.Errorf("the %s chip is titled %q, want no time for a stage the row holds no record of", stage, chip.Title)
		}
	}
}
