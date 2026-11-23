package dashboard

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Maintenance jobs pane draws the Jobs section the hub folds `gt jobs
// --json` into (jobs.go): one row per job with a chip coloured by the worst
// thing true of it, its last run's age and duration and its headline stat, the
// newest runs under a row the reader opened, and a line saying so when the read
// behind the list failed or went old. The tests drive the page's own render
// function over a reading and read back the elements it drew.

// jobsPaneRead is one jobs read's status as the server sends it: fresh unless
// the test says otherwise.
func jobsPaneRead(extra map[string]any) map[string]any {
	read := map[string]any{
		"at":        time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		"every_sec": 300,
	}
	for k, v := range extra {
		read[k] = v
	}
	return read
}

// jobsPaneState is a reading holding the Jobs section and the status of the
// read behind it. A nil read leaves the status out, the way a source that has
// not reported looks.
func jobsPaneState(read map[string]any, rows ...any) map[string]any {
	st := map[string]any{"jobs": map[string]any{"rows": rows}}
	if read != nil {
		st["reads"] = map[string]any{"jobs": read}
	}
	return st
}

// jobPaneRow is one job as the section carries it: a name, a state and the last
// run's outcome.
func jobPaneRow(name, state, outcome string) map[string]any {
	return map[string]any{"name": name, "state": state, "outcome": outcome}
}

// drawJobs makes st the page's reading and draws the pane from it, the way the
// page draws it when a reading arrives. The page's own click handler redraws
// from the same `state`, so a test that clicks reads what the reader would.
func drawJobs(t *testing.T, dom *pageDOM, st map[string]any) {
	t.Helper()

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	if _, err := dom.vm.RunString("state = " + string(raw) + "; renderJobs(state);"); err != nil {
		t.Fatalf("drawing the jobs pane: %v", err)
	}
}

// drawJobsOnPush makes st the page's reading and runs the page's push, which
// draws a pane only when a reading it names has moved.
func drawJobsOnPush(t *testing.T, dom *pageDOM, st map[string]any) {
	t.Helper()

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	if _, err := dom.vm.RunString("state = " + string(raw) + "; renderAll();"); err != nil {
		t.Fatalf("drawing the page: %v", err)
	}
}

// clickJobRow delivers the browser's click on one drawn row.
func clickJobRow(t *testing.T, dom *pageDOM, row int) {
	t.Helper()

	if _, err := dom.vm.RunString(`__click(__byId["jobs"].children[` + strconv.Itoa(row) + `])`); err != nil {
		t.Fatalf("clicking job row %d: %v", row, err)
	}
}

// pressJobRow delivers the browser's keydown on one drawn row.
func pressJobRow(t *testing.T, dom *pageDOM, row int, key string) {
	t.Helper()

	js := `__fireOn(__byId["jobs"].children[` + strconv.Itoa(row) + `], "keydown", {key: ` +
		strconv.Quote(key) + `, preventDefault: function () {}})`
	if _, err := dom.vm.RunString(js); err != nil {
		t.Fatalf("pressing %q on job row %d: %v", key, row, err)
	}
}

// jobRowChip reads back one row's chip: its class, its words and its title.
func jobRowChip(t *testing.T, dom *pageDOM, row int) drawn {
	t.Helper()
	return dom.descend(t, "jobs", row, 1)
}

// The pane draws one row per job, and the row's chip wears the worst thing true
// of the job, never green for a job the report does not vouch for: ok is green,
// a failed run and an overdue job are red, and a job that never ran or whose
// state nobody named is amber or grey. Run: go test ./internal/dashboard/ -run
// TestJobsPane
func TestJobsPaneDrawsARowPerJobWithItsColour(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, state, outcome, cls, words string
	}{
		{"ok", "ok", "ok", "good", "✓ ok"},
		{"failed", "ok", "failed", "badc", "✗ failed"},
		{"overdue", "overdue", "ok", "badc", "✗ overdue"},
		{"never ran", "never_run", "", "warnc", "◌ never run"},
		{"unknown", "unknown", "", "absent", "? unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil), jobPaneRow("dolt_gc", tc.state, tc.outcome)))
			if got := dom.element(t, "jobs").Kids; got != 1 {
				t.Fatalf("the pane drew %d rows for one job, want 1", got)
			}
			if got := dom.descend(t, "jobs", 0, 0).Text; got != "dolt_gc" {
				t.Errorf("the row names %q, want the job's dolt_gc", got)
			}
			chip := jobRowChip(t, dom, 0)
			if chip.Class != "tag "+tc.cls {
				t.Errorf("%s job: chip wears %q, want %q", tc.name, chip.Class, "tag "+tc.cls)
			}
			if chip.Text != tc.words {
				t.Errorf("%s job: chip reads %q, want %q", tc.name, chip.Text, tc.words)
			}
		})
	}

	dom := newPageDOM(t)
	drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil),
		jobPaneRow("dolt_gc", "ok", "ok"),
		jobPaneRow("node_disk", "never_run", ""),
		jobPaneRow("ci_prune", "overdue", "ok")))
	if got := dom.element(t, "jobs").Kids; got != 3 {
		t.Errorf("the pane drew %d rows for three jobs, want 3", got)
	}
}

// A row draws the last run's age, how long it took, and the headline stat in
// the unit the stat's name gives: a byte count in GiB, a percentage as itself,
// and milliseconds as a duration.
func TestJobsPaneDrawsLastRunDurationAndHeadline(t *testing.T) {
	t.Parallel()

	lastRun := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	dom := newPageDOM(t)
	drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil),
		map[string]any{
			"name": "dolt_gc", "state": "ok", "outcome": "ok",
			"last_run":    lastRun,
			"duration_ms": 3000,
			"headline":    map[string]any{"name": "reclaimed_bytes", "value": 5 * 1024 * 1024 * 1024},
		},
		map[string]any{
			"name": "node_disk", "state": "ok", "outcome": "ok",
			"headline": map[string]any{"name": "used_percent", "value": 42},
		},
		map[string]any{
			"name": "ci_prune", "state": "ok", "outcome": "ok",
			"headline": map[string]any{"name": "duration_ms", "value": 125000},
		},
		map[string]any{
			"name": "mystery", "state": "ok", "outcome": "ok",
			"headline": map[string]any{"name": "reviews", "value": 7},
		}))
	for _, tc := range []struct {
		row  int
		cell int
		want string
	}{
		{0, 2, "2h00m"}, {0, 3, "3s"}, {0, 4, "5.0 GiB"},
		{1, 2, "never ran"}, {1, 4, "42%"},
		{2, 4, "2m"},
		{3, 4, "7 reviews"},
	} {
		if got := dom.descend(t, "jobs", tc.row, tc.cell).Text; got != tc.want {
			t.Errorf("row %d cell %d reads %q, want %q", tc.row, tc.cell, got, tc.want)
		}
	}
}

// A click on a row opens the job's newest runs under it, each with its age,
// its duration and its outcome, and the same click closes it again. Enter and
// Space open it too, the way a button answers the keyboard.
func TestJobsPaneOpensTheNewestRunsOnAClick(t *testing.T) {
	t.Parallel()

	at := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339) }
	runs := []any{
		map[string]any{"started_at": at(-3 * time.Hour), "ended_at": at(-2 * time.Hour), "outcome": "ok"},
		map[string]any{"started_at": at(-5 * time.Hour), "ended_at": at(-4 * time.Hour), "outcome": "failed"},
		map[string]any{"started_at": at(-7 * time.Hour), "ended_at": at(-6 * time.Hour)},
		map[string]any{"started_at": at(-9 * time.Hour), "ended_at": at(-8 * time.Hour), "outcome": "refused"},
		map[string]any{"started_at": at(-11 * time.Hour), "ended_at": at(-10 * time.Hour), "outcome": "mystery"},
	}
	row := jobPaneRow("dolt_gc", "ok", "ok")
	row["runs"] = runs
	dom := newPageDOM(t)
	drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil), row))

	const closedKids = 4 // name, chip, last run, duration; no headline on this row
	if got := dom.descend(t, "jobs", 0).Kids; got != closedKids {
		t.Fatalf("a closed row holds %d children, want %d", got, closedKids)
	}
	if got := pageExpr(t, dom, `String(__byId["jobs"].children[0].getAttribute("aria-expanded"))`); got != "false" {
		t.Errorf("a closed row's aria-expanded reads %q, want false", got)
	}

	clickJobRow(t, dom, 0)
	if got := pageExpr(t, dom, `String(__byId["jobs"].children[0].getAttribute("aria-expanded"))`); got != "true" {
		t.Errorf("an opened row's aria-expanded reads %q, want true", got)
	}
	if got := dom.descend(t, "jobs", 0, closedKids).Kids; got != 5 {
		t.Fatalf("the opened row holds %d runs, want the newest 5", got)
	}
	for i, want := range []struct{ words, text string }{
		{"2h00m ago", "60m"}, {"4h00m ago", "60m"}, {"6h00m ago", "60m"}, {"8h00m ago", "60m"}, {"10h00m ago", "60m"},
	} {
		line := dom.descend(t, "jobs", 0, closedKids, i)
		if got := line.Text; !strings.Contains(got, want.words) || !strings.Contains(got, want.text) {
			t.Errorf("run %d reads %q, want its age %q and duration %q", i, got, want.words, want.text)
		}
	}
	for i, want := range []struct{ cls, words string }{
		{"tag good", "✓ ok"},
		{"tag badc", "✗ failed"},
		{"tag absent", "– no-op"},
		{"tag warnc", "◌ refused"},
		{"tag absent", "? unknown"},
	} {
		chip := dom.descend(t, "jobs", 0, closedKids, i, 2)
		if chip.Class != want.cls || chip.Text != want.words {
			t.Errorf("run %d chip is %q reading %q, want %q reading %q", i, chip.Class, chip.Text, want.cls, want.words)
		}
	}

	clickJobRow(t, dom, 0)
	if got := dom.descend(t, "jobs", 0).Kids; got != closedKids {
		t.Errorf("the second click left the row holding %d children, want it closed at %d", got, closedKids)
	}

	pressJobRow(t, dom, 0, "Enter")
	if got := dom.descend(t, "jobs", 0).Kids; got != closedKids+1 {
		t.Errorf("Enter left the row holding %d children, want the runs opened", got)
	}
	pressJobRow(t, dom, 0, " ")
	if got := dom.descend(t, "jobs", 0).Kids; got != closedKids {
		t.Errorf("Space left the row holding %d children, want it closed", got)
	}
}

// A state or an outcome the page does not name is drawn unknown, never as a job
// that is well, and a job that never ran stays amber even when its last run's
// outcome is ok.
func TestJobsPaneRefusesAnUnnamedStateOrOutcome(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, state, outcome, cls, words string
	}{
		{"an unnamed state", "mystery", "ok", "absent", "? unknown"},
		{"an unnamed outcome", "ok", "mystery", "absent", "? unknown"},
		{"unknown state with an ok outcome", "unknown", "ok", "absent", "? unknown"},
		{"never run with an ok outcome", "never_run", "ok", "warnc", "◌ never run"},
		{"ok state, deferred run", "ok", "deferred", "warnc", "◌ deferred"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil), jobPaneRow("dolt_gc", tc.state, tc.outcome)))
			chip := jobRowChip(t, dom, 0)
			if chip.Class != "tag "+tc.cls || chip.Text != tc.words {
				t.Errorf("%s: chip is %q reading %q, want %q reading %q", tc.name, chip.Class, chip.Text, "tag "+tc.cls, tc.words)
			}
		})
	}
}

// The pane is a fold in the left column, above System updates and Beads, and
// the page's push table points the section at it: a reading that arrives draws
// the pane, and a reader who opens the section draws it too.
func TestJobsPaneIsALeftColumnFold(t *testing.T) {
	t.Parallel()

	left, right := pageColumnKeys(t)
	if !slices.Contains(left, "jobs") {
		t.Errorf("the left column holds %v, with no Maintenance jobs pane", left)
	}
	if slices.Contains(right, "jobs") {
		t.Error("the Maintenance jobs pane is also in the right column")
	}

	dom := newPageDOM(t)
	if got := pageExpr(t, dom, `PANES.jobs.sec + "|" + PANES.jobs.body.join(",")`); got != "jobs|jobs" {
		t.Errorf("the push table points %q, want the jobs section and body", got)
	}
	raw, err := json.Marshal(jobsPaneState(jobsPaneRead(nil), jobPaneRow("dolt_gc", "ok", "ok")))
	if err != nil {
		t.Fatalf("encoding the reading: %v", err)
	}
	dom.press(t, "jobs", "click")
	if _, err := dom.vm.RunString("state = " + string(raw) + "; renderAll();"); err != nil {
		t.Fatalf("drawing the reading: %v", err)
	}
	if got := dom.element(t, "jobs").Kids; got != 1 {
		t.Errorf("the redraw drew %d rows, want the one row of the reading", got)
	}
}

// A read that failed or went old says so above the list, and the kept rows are
// greyed rather than drawn as current. A first read that failed with no good
// list says that, not "no jobs". Run: go test ./internal/dashboard/ -run
// TestJobsPaneStale
func TestJobsPaneStaleSaysSo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                                string
		read                                map[string]any
		line, lineText, noteClass, noteText string
	}{
		{"an aged read", jobsPaneRead(map[string]any{"stale": true}),
			"surow warnc", "this job list is stale", "r warnc", "2 jobs · stale"},
		{"a failed read", jobsPaneRead(map[string]any{"error": true, "stale": true}),
			"surow badc", "the read failed; this is the last good list", "r badc", "2 jobs · read failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			drawJobs(t, dom, jobsPaneState(tc.read,
				jobPaneRow("dolt_gc", "ok", "ok"), jobPaneRow("node_disk", "never_run", "")))
			if got := dom.element(t, "jobs").Kids; got != 3 {
				t.Fatalf("the pane drew %d children, want the line and the two rows", got)
			}
			line := dom.descend(t, "jobs", 0)
			if line.Class != tc.line || line.Text != tc.lineText {
				t.Errorf("%s: the pane leads with %q reading %q, want %q reading %q", tc.name, line.Class, line.Text, tc.line, tc.lineText)
			}
			if got := dom.element(t, "jobs").Class; !strings.Contains(got, "stale") {
				t.Errorf("%s: the pane wears %q, want the kept rows greyed", tc.name, got)
			}
			note := dom.element(t, "jobsnote")
			if note.Class != tc.noteClass || note.Text != tc.noteText {
				t.Errorf("%s: the note is %q reading %q, want %q reading %q", tc.name, note.Class, note.Text, tc.noteClass, tc.noteText)
			}
		})
	}
}

// A first read that failed leaves no list to keep, and the pane says that
// rather than drawing an empty list as a town with no jobs.
func TestJobsPaneAFailedFirstReadSaysNoList(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	drawJobs(t, dom, jobsPaneState(jobsPaneRead(map[string]any{"error": true, "stale": true})))
	if got := dom.element(t, "jobs").Kids; got != 1 {
		t.Fatalf("the pane drew %d children, want the one line", got)
	}
	line := dom.descend(t, "jobs", 0)
	if line.Class != "surow badc" || line.Text != "the read failed; no job list has been read" {
		t.Errorf("the pane reads %q in %q, want the failed-read line", line.Text, line.Class)
	}
}

// A pane with no section yet says the jobs have not been read, or that they are
// being read, rather than drawing "no jobs" for a reading that never arrived.
func TestJobsPaneWithNoListSaysSo(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		st   map[string]any
		want string
	}{
		{"never read", map[string]any{
			"jobs": nil,
			"reads": map[string]any{"jobs": map[string]any{
				"at": "0001-01-01T00:00:00Z", "every_sec": 300, "stale": true,
			}},
		}, "the jobs have not been read"},
		{"no read yet", map[string]any{}, "reading the jobs…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			drawJobs(t, dom, tc.st)
			line := dom.descend(t, "jobs", 0)
			if line.Text != tc.want {
				t.Errorf("%s: the pane reads %q, want %q", tc.name, line.Text, tc.want)
			}
		})
	}
}

// The pane follows the read's own status on a push: with the same job list and
// only the read gone stale, the push redraws the pane and says so. A read list
// that left out readInput(s, "jobs") would skip the draw and leave the stale
// list reading as current.
func TestJobsPaneStaleFollowsTheReadOnAPush(t *testing.T) {
	t.Parallel()

	dom := newPageDOM(t)
	dom.press(t, "jobs", "click")
	fresh := jobsPaneState(jobsPaneRead(nil), jobPaneRow("dolt_gc", "ok", "ok"))
	drawJobsOnPush(t, dom, fresh)
	if got := dom.element(t, "jobs").Kids; got != 1 {
		t.Fatalf("the first push drew %d children, want the one row", got)
	}

	aged := jobsPaneState(jobsPaneRead(map[string]any{"stale": true}), jobPaneRow("dolt_gc", "ok", "ok"))
	drawJobsOnPush(t, dom, aged)
	if got := dom.element(t, "jobs").Kids; got != 2 {
		t.Fatalf("the push after the read aged drew %d children, want the stale line and the row", got)
	}
	if got := dom.descend(t, "jobs", 0).Text; got != "this job list is stale" {
		t.Errorf("the push read %q, want the stale line", got)
	}
}

// The pane header's icon follows the worst chip the pane drew: red above amber,
// and the heading's muted dash while every job is well or the list is empty. A
// grey unknown job earns no colour, as the System updates pane's icon does.
func TestJobsPaneStaleHeaderIconFollowsTheWorst(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		rows  []any
		class string
		glyph string
	}{
		{"every job ok", []any{jobPaneRow("dolt_gc", "ok", "ok")}, "suico", "–"},
		{"a job that never ran", []any{jobPaneRow("dolt_gc", "ok", "ok"), jobPaneRow("node_disk", "never_run", "")}, "suico warnc", "◌"},
		{"a failed job", []any{jobPaneRow("dolt_gc", "ok", "ok"), jobPaneRow("ci_prune", "ok", "failed")}, "suico badc", "✗"},
		{"an overdue job above a failed one", []any{jobPaneRow("ci_prune", "ok", "failed"), jobPaneRow("node_disk", "overdue", "ok")}, "suico badc", "✗"},
		{"an unknown job", []any{jobPaneRow("mystery", "unknown", "")}, "suico", "–"},
		{"no jobs", nil, "suico", "–"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dom := newPageDOM(t)
			drawJobs(t, dom, jobsPaneState(jobsPaneRead(nil), tc.rows...))
			ico := dom.element(t, "jobsico")
			if ico.Class != tc.class || ico.Text != tc.glyph {
				t.Errorf("%s: the icon wears %q reading %q, want %q reading %q", tc.name, ico.Class, ico.Text, tc.class, tc.glyph)
			}
		})
	}
}
