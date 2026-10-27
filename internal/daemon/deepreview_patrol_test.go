package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/deepreview"
)

// shadowNow is the tick every shadow test runs at. It sits after every ledger
// timestamp in the fixtures below.
var shadowNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// shadowLedger is the landings file a shadow test reads: two risky landings
// after the cursor line, the second one newest.
const shadowLedger = `
{"bead":"gt-a","head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T03:00:00Z","risk_paths":["internal/daemon/a.go"]}
{"bead":"gt-b","head":"bbbb","landed_commit":"c2","landed_at":"2026-10-26T04:00:00Z","risk_paths":["internal/daemon/b.go"]}
`

// shadowCursor is the newest run bead's note in the happy path: the window
// starts here, so both fixture landings are reviewed.
const shadowCursor = "cursor: 2026-10-26T02:00:00Z"

// shadowEntry is a fresh shadow entry. It is a function, not a package value,
// so a parallel test cannot see another test's Vars map.
func shadowEntry() ScheduledSlingEntry {
	return ScheduledSlingEntry{
		Name:        "deep-review-shadow",
		Rig:         "gastown",
		Formula:     "mol-deep-review",
		IntervalStr: "168h",
		Dispatch:    scheduledDispatchShadow,
		Vars:        map[string]string{shadowModeVar: shadowModeLandings},
	}
}

// writeShadowLedger writes the ledger file shadowLedgerPath resolves for rig.
func writeShadowLedger(t *testing.T, townRoot, rig, content string) {
	t.Helper()
	path, err := shadowLedgerPath(townRoot, ScheduledSlingEntry{Rig: rig})
	if err != nil {
		t.Fatalf("shadow ledger path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for the ledger: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
}

// TestShadowRunWritesItsPlanToTheRunBeadsNotesOnly is the acceptance reading:
// the plan is the run's only content write, its only bead, and nothing is
// slung. The bead's labels, description and close reason carry none of the
// plan, and no other bead or destination is touched.
func TestShadowRunWritesItsPlanToTheRunBeadsNotesOnly(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	// The previous run is older than the interval, so this tick is due; its
	// notes carry the cursor line the plan starts from.
	f.beads = []scheduledBead{{
		ID:        "gt-prev",
		Status:    "closed",
		CreatedAt: shadowNow.Add(-2 * 7 * 24 * time.Hour),
		Notes:     shadowCursor,
	}}
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err != nil {
		t.Fatalf("shadow run: %v", err)
	}

	if len(f.created) != 1 {
		t.Fatalf("created %d beads, want 1: %v", len(f.created), f.created)
	}
	if len(f.slungIDs) != 0 || len(f.slung) != 0 {
		t.Errorf("the shadow run slung %v, want no sling", f.slungIDs)
	}
	if len(f.admitted) != 0 {
		t.Errorf("the shadow run probed capacity for %v, want no probe", f.admitted)
	}

	run := f.runs[0]
	if want := []string{scheduledSlingLabel(e)}; len(run.Labels) != 1 || run.Labels[0] != want[0] {
		t.Errorf("run bead labels = %v, want only the identity label %v (a formula label would let the spec dispatcher take it)", run.Labels, want)
	}
	for _, key := range []string{"mode=", "quiet=", "reviewed_until_", "landed_at"} {
		if strings.Contains(run.Description, key) {
			t.Errorf("run bead description carries plan content %q: %q", key, run.Description)
		}
	}

	notes := f.notes["gt-shadow1"]
	if len(notes) != 1 {
		t.Fatalf("appended %d notes to the run bead, want 1: %v", len(notes), notes)
	}
	if len(f.notes) != 1 {
		t.Errorf("notes written to %d beads, want only the run bead: %v", len(f.notes), f.notes)
	}
	lines := strings.Split(notes[0], "\n")
	for i, want := range []string{
		"mode=landings",
		"quiet=false",
		"landings=2",
		"skipped=0",
		"records=2",
		"unparsed=0",
		"reviewed_until_old=2026-10-26T02:00:00Z",
		"reviewed_until_new=2026-10-26T04:00:00Z",
	} {
		if i >= len(lines) || lines[i] != want {
			t.Fatalf("plan note line %d = %q, want %q (note: %s)", i, lineAt(lines, i), want, notes[0])
		}
	}
	// The cursor line is last, in the same append as the rows it covers.
	if want, got := "cursor: 2026-10-26T04:00:00Z", lineAt(lines, len(lines)-1); got != want {
		t.Errorf("last note line = %q, want the cursor %q", got, want)
	}
	// The landing lines are read with the fields the ledger writes, landed_at
	// as a string, the way a consumer of landings.jsonl reads them.
	type noteLanding struct {
		Bead      string   `json:"bead"`
		LandedAt  string   `json:"landed_at"`
		RiskPaths []string `json:"risk_paths"`
	}
	for i, want := range []struct{ bead, landedAt string }{
		{"gt-a", "2026-10-26T03:00:00Z"},
		{"gt-b", "2026-10-26T04:00:00Z"},
	} {
		var rec noteLanding
		line := lineAt(lines, 8+i)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("plan note line %d is not the landings.jsonl shape: %v (%q)", 8+i, err, line)
		}
		if rec.Bead != want.bead || rec.LandedAt != want.landedAt || len(rec.RiskPaths) != 1 {
			t.Errorf("plan note landing %d = %+v, want bead %s at %s with one risk path", i, rec, want.bead, want.landedAt)
		}
	}

	if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-shadow1" {
		t.Errorf("closed %v, want only the run bead", f.closedIDs)
	}
	if len(f.closedWhy) != 1 || !strings.Contains(f.closedWhy[0], "notes") || strings.Contains(f.closedWhy[0], "mode=") {
		t.Errorf("close reason = %q, want the fixed reason naming the notes and carrying no plan", f.closedWhy)
	}
}

// lineAt is lines[i], or "" when the note is shorter than the test expects, so
// a short note fails on the value rather than on an index panic.
func lineAt(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

// shadowTiedLedger is a ledger whose middle two landings share one landed_at:
// the fixture a cut has to leave whole.
const shadowTiedLedger = `
{"bead":"gt-a","head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T03:00:00Z","risk_paths":["internal/daemon/a.go"]}
{"bead":"gt-b","head":"bbbb","landed_commit":"c2","landed_at":"2026-10-26T04:00:00Z","risk_paths":["internal/daemon/b.go"]}
{"bead":"gt-c","head":"cccc","landed_commit":"c3","landed_at":"2026-10-26T04:00:00Z","risk_paths":["internal/daemon/c.go"]}
{"bead":"gt-d","head":"dddd","landed_commit":"c4","landed_at":"2026-10-26T05:00:00Z","risk_paths":["internal/daemon/d.go"]}
`

// noteLine returns the note's line starting with prefix, or "" when it holds
// none, so a missing line fails on its value rather than on an index.
func noteLine(note, prefix string) string {
	for _, line := range strings.Split(note, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// TestShadowRunBoundsTheSliceAndMovesTheCursor is the slice bound and the
// cursor placement read together: the run reviews a bounded slice of the plan's
// landings, reports how many it left, and puts the cursor on the last one it
// reviewed so the next run starts there.
func TestShadowRunBoundsTheSliceAndMovesTheCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ledger   string
		vars     map[string]string
		landings int
		skipped  int
		cursor   string
	}{
		{name: "under the bound", ledger: shadowLedger, vars: map[string]string{}, landings: 2, skipped: 0, cursor: "2026-10-26T04:00:00Z"},
		{name: "row bound", ledger: shadowLedger, vars: map[string]string{shadowSliceFilesVar: "1"}, landings: 1, skipped: 1, cursor: "2026-10-26T03:00:00Z"},
		{
			name:   "a tie the bound would split is left whole",
			ledger: shadowTiedLedger,
			// Two rows fit, and the second is the first of a two-landing
			// group: reviewing it alone would leave the other one behind the
			// cursor for good.
			vars:     map[string]string{shadowSliceFilesVar: "2"},
			landings: 1, skipped: 3, cursor: "2026-10-26T03:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow}
			e := shadowEntry()
			e.Vars = tc.vars
			e.Vars[shadowModeVar] = shadowModeLandings
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			f.beads = []scheduledBead{{ID: "gt-prev", Status: "closed", CreatedAt: shadowNow.Add(-2 * 7 * 24 * time.Hour), Notes: shadowCursor}}
			writeShadowLedger(t, d.config.TownRoot, e.Rig, tc.ledger)

			if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err != nil {
				t.Fatalf("shadow run: %v", err)
			}
			note := noteAt(t, f, "gt-shadow1")
			if want, got := fmt.Sprintf("landings=%d", tc.landings), noteLine(note, "landings="); got != want {
				t.Errorf("%s, want %q", got, want)
			}
			if want, got := fmt.Sprintf("skipped=%d", tc.skipped), noteLine(note, "skipped="); got != want {
				t.Errorf("%s, want %q", got, want)
			}
			until := "reviewed_until_new=" + tc.cursor
			if got := noteLine(note, "reviewed_until_new="); got != until {
				t.Errorf("%s, want %q", got, until)
			}
			lines := strings.Split(note, "\n")
			if want, got := "cursor: "+tc.cursor, lines[len(lines)-1]; got != want {
				t.Errorf("last note line = %q, want the cursor %q", got, want)
			}
			// Eight key lines, one landing line per reviewed landing, and the
			// cursor line: anything else is a row the note invented or lost.
			if got, want := len(lines), 8+tc.landings+1; got != want {
				t.Errorf("note has %d lines, want %d (note: %s)", got, want, note)
			}
		})
	}
}

// TestShadowRunMovesNoCursorOnAQuietDay pins the quiet run's note and its log
// line: nothing was reviewed, so no cursor line is written and the next run
// asks for its window again rather than resuming from a pinned start. The log
// says the cursor did not move, the way the note does (gt-80jmz).
func TestShadowRunMovesNoCursorOnAQuietDay(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	logs := &bytes.Buffer{}
	d.logger = log.New(logs, "", 0)
	f.beads = []scheduledBead{{ID: "gt-prev", Status: "closed", CreatedAt: shadowNow.Add(-2 * 7 * 24 * time.Hour), Notes: shadowCursor}}
	// A ledger whose only record keeps no risk path: nothing to review.
	writeShadowLedger(t, d.config.TownRoot, e.Rig,
		`{"bead":"gt-a","head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T03:00:00Z","risk_paths":[]}`+"\n")

	if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err != nil {
		t.Fatalf("quiet run: %v", err)
	}
	note := noteAt(t, f, "gt-shadow1")
	if got := noteLine(note, "cursor: "); got != "" {
		t.Errorf("quiet note carries %q, want no cursor line (note: %s)", got, note)
	}
	if want, got := "landings=0", noteLine(note, "landings="); got != want {
		t.Errorf("%s, want %q", got, want)
	}
	if want, got := "reviewed_until_new=2026-10-26T02:00:00Z", noteLine(note, "reviewed_until_new="); got != want {
		t.Errorf("%s, want the window start %q", got, want)
	}
	if got, want := logs.String(), "scheduled_slings: "+e.Name+": shadow run gt-shadow1 reviewed no landings and moved no cursor; nothing was slung\n"; got != want {
		t.Errorf("log = %q, want %q", got, want)
	}
}

// noteAt is the single note appended to bead id, for a test that expects
// exactly one.
func noteAt(t *testing.T, f *fakeScheduledRunner, id string) string {
	t.Helper()
	notes := f.notes[id]
	if len(notes) != 1 {
		t.Fatalf("appended %d notes to %s, want 1: %v", len(notes), id, notes)
	}
	return notes[0]
}

// TestShadowRunMovesTheCursorOnlyAfterTheBeadIsWritten pins the ordering: a run
// whose note never landed moved nothing, so the next run reads the same window
// and reviews the same landings; a run whose note landed moved the cursor, and
// the next run starts there.
func TestShadowRunMovesTheCursorOnlyAfterTheBeadIsWritten(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// noteErr runs the first tick with a failing note append.
		noteErr bool
		// wantOld is what the second run starts from.
		wantOld string
	}{
		{name: "note written", wantOld: "2026-10-26T04:00:00Z"},
		{name: "note lost", noteErr: true, wantOld: "2026-10-26T02:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-run1", now: shadowNow}
			if tc.noteErr {
				f.noteErr = errors.New("dolt down")
			}
			e := shadowEntry()
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			f.beads = []scheduledBead{{ID: "gt-prev", Status: "closed", CreatedAt: shadowNow.Add(-2 * 7 * 24 * time.Hour), Notes: shadowCursor}}
			writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

			if tc.noteErr {
				if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err == nil {
					t.Fatal("the first run reported success, want the note error")
				}
				f.noteErr = nil
			} else if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err != nil {
				t.Fatalf("first run: %v", err)
			}
			// A later list hands the notes back the way bd would, so the
			// second run reads whatever cursor the first one recorded. A bead
			// retired as a failed run has none: the fake records the note
			// before failing the append, where bd writes nothing.
			for i := range f.beads {
				if strings.HasPrefix(f.beads[i].CloseReason, scheduledSlingFailureMarker) {
					continue
				}
				if notes := f.notes[f.beads[i].ID]; len(notes) > 0 {
					f.beads[i].Notes = strings.Join(notes, "\n")
				}
			}
			// The second tick runs an interval later: it is due only if the
			// first run's bead did not suppress it, which also pins the
			// failure marker a lost note leaves behind.
			f.createID, f.now = "gt-run2", shadowNow.Add(168*time.Hour)
			if err := d.runScheduledSlingEntry(context.Background(), e, f.now); err != nil {
				t.Fatalf("second run: %v", err)
			}
			note := noteAt(t, f, "gt-run2")
			if want, got := "reviewed_until_old="+tc.wantOld, noteLine(note, "reviewed_until_old="); got != want {
				t.Errorf("second run's %s, want %q", got, want)
			}
		})
	}
}

// TestShadowRunRefusesASecondRunWhileOneIsOpen checks the one-run rule on the
// shadow path: an open run bead means a run is in flight, so the tick creates
// no second bead and writes no second cursor.
func TestShadowRunRefusesASecondRunWhileOneIsOpen(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow2", now: shadowNow}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	f.beads = []scheduledBead{{
		ID:        "gt-inflight",
		Status:    "open",
		CreatedAt: shadowNow.Add(-time.Minute),
		Notes:     "cursor: 2026-10-26T02:00:00Z",
	}}
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err != nil {
		t.Fatalf("a skipped tick must not fail: %v", err)
	}
	if len(f.created) != 0 || len(f.notes) != 0 || len(f.closedIDs) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("a run in flight must leave the tick alone: created %v notes %v closed %v slung %v",
			f.created, f.notes, f.closedIDs, f.slungIDs)
	}
}

// TestShadowRunRefusesAPlanItCannotTrust is the no-bead half: a ledger, a mode
// or a rig the plan cannot use creates no bead, writes no note and slings
// nothing, so a shadow run never records a verdict it did not earn.
func TestShadowRunRefusesAPlanItCannotTrust(t *testing.T) {
	t.Parallel()
	drifted := `{"bead":42,"head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T03:00:00Z"}` + "\n"
	cases := []struct {
		name string
		// ledger is the file's content; nil writes no file at all, and a
		// pointer to "" writes an empty one.
		ledger *string
		// vars replaces the entry's vars; nil keeps the entry's default.
		vars map[string]string
		want string
	}{
		{name: "missing ledger", want: "reading landings ledger"},
		{name: "empty ledger", ledger: strPtr(""), want: "no readable landing record"},
		{name: "drifted ledger", ledger: strPtr(drifted), want: "cannot be trusted"},
		{name: "mode unset", ledger: strPtr(shadowLedger), vars: map[string]string{}, want: "is not ported"},
		{name: "mode both", ledger: strPtr(shadowLedger), vars: map[string]string{shadowModeVar: "both"}, want: "is not ported"},
		{name: "mode slice", ledger: strPtr(shadowLedger), vars: map[string]string{shadowModeVar: "slice"}, want: "is not ported"},
		{name: "landings_rig leaves the directory", ledger: strPtr(shadowLedger), vars: map[string]string{shadowModeVar: shadowModeLandings, shadowLandingsRigVar: "../evil"}, want: "invalid rig name"},
		{name: "landings_rig empty", ledger: strPtr(shadowLedger), vars: map[string]string{shadowModeVar: shadowModeLandings, shadowLandingsRigVar: ""}, want: "invalid rig name"},
		{name: "slice bound unparseable", ledger: strPtr(shadowLedger), vars: map[string]string{shadowModeVar: shadowModeLandings, shadowSliceBytesVar: "lots"}, want: "slice_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow}
			e := shadowEntry()
			if tc.vars != nil {
				e.Vars = tc.vars
			}
			d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
			if tc.ledger != nil {
				writeShadowLedger(t, d.config.TownRoot, e.Rig, *tc.ledger)
			}

			err := d.runScheduledSlingEntry(context.Background(), e, shadowNow)
			if err == nil {
				t.Fatal("the run succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			if len(f.created) != 0 || len(f.notes) != 0 || len(f.slungIDs) != 0 {
				t.Errorf("a refused plan must create nothing: created %v notes %v slung %v", f.created, f.notes, f.slungIDs)
			}
		})
	}
}

// TestShadowRunRetiresTheBeadWhenTheNoteFails checks the failure path: a bead
// whose plan never landed is closed with the failure marker, so the next tick
// retries instead of reading a completed run that suppresses the interval.
func TestShadowRunRetiresTheBeadWhenTheNoteFails(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow, noteErr: errors.New("dolt down")}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	err := d.runScheduledSlingEntry(context.Background(), e, shadowNow)
	if err == nil {
		t.Fatal("the run reported success, want the note write's error")
	}
	if len(f.closedIDs) != 1 || f.closedIDs[0] != "gt-shadow1" {
		t.Fatalf("closed %v, want the run bead retired", f.closedIDs)
	}
	if !strings.HasPrefix(f.closedWhy[0], scheduledSlingFailureMarker) {
		t.Errorf("close reason = %q, want the failure marker so the next tick retries", f.closedWhy[0])
	}
}

// TestShadowRunReportsACloseItCouldNotMake checks that a plan which landed in
// the notes is not lost to a failed close: the error names the bead and the run
// is not reported as a success.
func TestShadowRunReportsACloseItCouldNotMake(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow, closeErr: errors.New("dolt down")}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	err := d.runScheduledSlingEntry(context.Background(), e, shadowNow)
	if err == nil {
		t.Fatal("the run reported success, want the close error")
	}
	if !strings.Contains(err.Error(), "could not be closed") {
		t.Errorf("error = %v, want it to say the bead could not be closed", err)
	}
	if len(f.notes["gt-shadow1"]) != 1 {
		t.Errorf("the plan note = %v, want it written before the close", f.notes["gt-shadow1"])
	}
}

// TestShadowRunReportsARenderItCouldNotMake drives the encoder's failure: a
// landing that cannot be rendered is an error before any bead exists, never a
// note with a line missing from it.
func TestShadowRunReportsARenderItCouldNotMake(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	d.seams.shadowEncodeJSON = func(any) ([]byte, error) { return nil, errors.New("encoder refused") }
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	err := d.runScheduledSlingEntry(context.Background(), e, shadowNow)
	if err == nil {
		t.Fatal("the run reported success, want the render error")
	}
	if !strings.Contains(err.Error(), "encoder refused") {
		t.Errorf("error = %v, want it to carry the encoder's error", err)
	}
	if len(f.created) != 0 || len(f.notes) != 0 {
		t.Errorf("a refused plan must create nothing: created %v notes %v", f.created, f.notes)
	}
}

// TestShadowRunReportsACreateItCouldNotMake checks that a bead the patrol
// cannot create stops the run: no note is written to an id that does not exist
// and nothing is slung.
func TestShadowRunReportsACreateItCouldNotMake(t *testing.T) {
	t.Parallel()
	f := &fakeScheduledRunner{createID: "gt-shadow1", now: shadowNow, createErr: errors.New("dolt down")}
	e := shadowEntry()
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{e}, f)
	writeShadowLedger(t, d.config.TownRoot, e.Rig, shadowLedger)

	if err := d.runScheduledSlingEntry(context.Background(), e, shadowNow); err == nil {
		t.Fatal("the run reported success, want the create error")
	}
	if len(f.notes) != 0 || len(f.closedIDs) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("a failed create must stop the run: notes %v closed %v slung %v", f.notes, f.closedIDs, f.slungIDs)
	}
}

// TestShadowPlanNoteRendersTheShellPlanShape pins the note to the shape
// scripts/deep-review.sh plan prints: the key lines, then one landings line per
// reviewed landing, then the cursor line the next run starts from.
func TestShadowPlanNoteRendersTheShellPlanShape(t *testing.T) {
	t.Parallel()
	since := deepreview.Timestamp{Time: shadowNow.Add(-24 * time.Hour), Text: "2026-10-25T12:00:00Z"}
	until := deepreview.Timestamp{Time: shadowNow, Text: "2026-10-26T12:00:00Z"}
	line := `{"bead":"gt-a","head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T11:00:00Z","risk_paths":["internal/daemon/a.go"]}`
	landing := deepreview.Record{
		Bead: "gt-a", Head: "aaaa", LandedCommit: "c1",
		LandedAt:  deepreview.Timestamp{Time: shadowNow.Add(-time.Hour), Text: "2026-10-26T11:00:00Z"},
		RiskPaths: []string{"internal/daemon/a.go"},
	}
	cases := []struct {
		name  string
		plan  deepreview.Plan
		slice shadowSlice
		want  []string
	}{
		{
			name:  "quiet",
			plan:  deepreview.Plan{Since: since, ReviewedUntil: since, Quiet: true, Records: 4, Landings: []deepreview.Record{}},
			slice: shadowSlice{Until: since},
			// A quiet run reviewed nothing and so moves no cursor.
			want: []string{"mode=landings", "quiet=true", "landings=0", "skipped=0", "records=4", "unparsed=0",
				"reviewed_until_old=2026-10-25T12:00:00Z", "reviewed_until_new=2026-10-25T12:00:00Z"},
		},
		{
			name:  "landings",
			plan:  deepreview.Plan{Since: since, ReviewedUntil: until, Records: 4, Unparsed: 1, Landings: []deepreview.Record{landing}},
			slice: shadowSlice{Lines: []string{line}, Until: landing.LandedAt},
			want: []string{"mode=landings", "quiet=false", "landings=1", "skipped=0", "records=4", "unparsed=1",
				"reviewed_until_old=2026-10-25T12:00:00Z", "reviewed_until_new=2026-10-26T11:00:00Z",
				line, "cursor: 2026-10-26T11:00:00Z"},
		},
		{
			name:  "bounded",
			plan:  deepreview.Plan{Since: since, ReviewedUntil: until, Records: 4, Landings: []deepreview.Record{landing}},
			slice: shadowSlice{Lines: []string{line}, Skipped: 3, Until: landing.LandedAt},
			want: []string{"mode=landings", "quiet=false", "landings=1", "skipped=3", "records=4", "unparsed=0",
				"reviewed_until_old=2026-10-25T12:00:00Z", "reviewed_until_new=2026-10-26T11:00:00Z",
				line, "cursor: 2026-10-26T11:00:00Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			note := shadowPlanNote(shadowModeLandings, tc.plan, tc.slice)
			if got := strings.Split(note, "\n"); !slices.Equal(got, tc.want) {
				t.Errorf("note =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
			if strings.HasSuffix(note, "\n") {
				t.Error("the note ends in a newline, want the block to end on its last line")
			}
		})
	}
}

// TestBoundLandings pins the cut: whole landed_at groups in plan order while
// they fit, the head group taken whole past the bound, and a group the bound
// would split left for the next run rather than half reviewed.
func TestBoundLandings(t *testing.T) {
	t.Parallel()
	at := func(second int) deepreview.Timestamp {
		ts := deepreview.Timestamp{Time: shadowNow.Add(time.Duration(second) * time.Second)}
		ts.Text = ts.Time.Format(time.RFC3339)
		return ts
	}
	rec := func(bead string, second int) deepreview.Record {
		return deepreview.Record{Bead: bead, LandedAt: at(second), RiskPaths: []string{"internal/daemon/a.go"}}
	}
	render := func(r deepreview.Record) (string, error) { return r.Bead, nil }
	cases := []struct {
		name     string
		landings []deepreview.Record
		rows     int
		bytes    int
		want     []string
	}{
		{name: "nothing to review", rows: 4, bytes: 100, want: nil},
		{name: "under the bounds", landings: []deepreview.Record{rec("gt-a", 1), rec("gt-b", 2)}, rows: 4, bytes: 100, want: []string{"gt-a", "gt-b"}},
		{name: "row bound", landings: []deepreview.Record{rec("gt-a", 1), rec("gt-b", 2), rec("gt-c", 3)}, rows: 2, bytes: 100, want: []string{"gt-a", "gt-b"}},
		{name: "byte bound", landings: []deepreview.Record{rec("gt-a", 1), rec("gt-bbbb", 2), rec("gt-c", 3)}, rows: 4, bytes: 9, want: []string{"gt-a"}},
		{
			name:     "a group the bound would split is left whole",
			landings: []deepreview.Record{rec("gt-a", 1), rec("gt-b", 2), rec("gt-c", 2), rec("gt-d", 3)},
			rows:     2, bytes: 100,
			want: []string{"gt-a"},
		},
		{
			name:     "the head group is taken past the bound",
			landings: []deepreview.Record{rec("gt-a", 1), rec("gt-b", 1), rec("gt-c", 2)},
			rows:     1, bytes: 100,
			want: []string{"gt-a", "gt-b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines, err := boundLandings(tc.landings, tc.rows, tc.bytes, render)
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			if !slices.Equal(lines, tc.want) {
				t.Errorf("slice = %v, want %v", lines, tc.want)
			}
		})
	}
}

// TestBoundLandingsCarriesTheRenderError checks that a landing the encoder
// cannot render stops the cut instead of leaving a row out of it.
func TestBoundLandingsCarriesTheRenderError(t *testing.T) {
	t.Parallel()
	landings := []deepreview.Record{{Bead: "gt-a", LandedAt: deepreview.Timestamp{Time: shadowNow}}}
	_, err := boundLandings(landings, 4, 100, func(deepreview.Record) (string, error) {
		return "", errors.New("encoder refused")
	})
	if err == nil || !strings.Contains(err.Error(), "encoder refused") {
		t.Fatalf("error = %v, want the render error carried", err)
	}
}

// TestShadowSliceBounds pins the bound vars: deepreview's defaults when unset,
// the entry's numbers when set, and a value that is not a positive whole number
// refused rather than defaulted.
func TestShadowSliceBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		vars      map[string]string
		wantRows  int
		wantBytes int
		wantErr   string
	}{
		{name: "defaults", vars: map[string]string{}, wantRows: deepreview.DefaultSliceFiles, wantBytes: deepreview.DefaultSliceBytes},
		{name: "set", vars: map[string]string{shadowSliceFilesVar: "3", shadowSliceBytesVar: "900"}, wantRows: 3, wantBytes: 900},
		{name: "zero rows", vars: map[string]string{shadowSliceFilesVar: "0"}, wantErr: "slice_files"},
		{name: "negative bytes", vars: map[string]string{shadowSliceBytesVar: "-1"}, wantErr: "slice_bytes"},
		{name: "not a number", vars: map[string]string{shadowSliceFilesVar: "forty"}, wantErr: "slice_files"},
		// Atoi answers ErrRange with the largest int, so the bound has to
		// refuse the error and not read the number.
		{name: "past the int range", vars: map[string]string{shadowSliceFilesVar: "99999999999999999999"}, wantErr: "slice_files"},
		{name: "empty", vars: map[string]string{shadowSliceBytesVar: ""}, wantErr: "slice_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := shadowEntry()
			e.Vars = tc.vars
			rows, bytes, err := shadowSliceBounds(e)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("bounds: %v", err)
			}
			if rows != tc.wantRows || bytes != tc.wantBytes {
				t.Errorf("bounds = %d rows, %d bytes, want %d and %d", rows, bytes, tc.wantRows, tc.wantBytes)
			}
		})
	}
}

// TestReadLandingsLedger pins the production reader: the file's bytes, and an
// error for a missing one rather than an empty ledger the plan would call quiet.
func TestReadLandingsLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "landings.jsonl")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readLandingsLedger(path)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if string(got) != "one\ntwo\n" {
		t.Errorf("ledger = %q, want the file's bytes", got)
	}

	if _, err := readLandingsLedger(filepath.Join(dir, "absent.jsonl")); err == nil {
		t.Fatal("a missing ledger read as empty, want an error")
	}
}

// TestListBeadsCarriesTheNotesTheLedgerNeeds pins the production notes reader:
// a run bead's notes arrive with the list a tick already makes, so the shadow
// run reads the cursor without a second call per bead.
func TestListBeadsCarriesTheNotesTheLedgerNeeds(t *testing.T) {
	t.Parallel()
	bd := beadsfake.New()
	withLabel, err := bd.Create(beads.CreateOptions{Title: "run", Labels: []string{"scheduled:deep-review-shadow"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := bd.Update(withLabel.ID, beads.UpdateOptions{AppendNotes: shadowCursor}); err != nil {
		t.Fatal(err)
	}
	if _, err := bd.Create(beads.CreateOptions{Title: "other", Labels: []string{"scheduled:other"}}); err != nil {
		t.Fatal(err)
	}
	r := &execScheduledSlingRunner{open: func(string) beads.Client { return bd }}

	runs, err := r.listBeads(context.Background(), "gastown", "scheduled:deep-review-shadow")
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != withLabel.ID {
		t.Fatalf("runs = %v, want only the labelled run", runs)
	}
	if runs[0].Notes != shadowCursor {
		t.Errorf("notes = %q, want the run bead's cursor line", runs[0].Notes)
	}
}

// TestShadowNotesStayBounded checks the bound: a long run history is read from
// its newest shadowNotesBeads beads, and the newest cursor line is inside them.
func TestShadowNotesStayBounded(t *testing.T) {
	t.Parallel()
	runs := make([]scheduledBead, 0, shadowNotesBeads*2)
	for i := 0; i < shadowNotesBeads*2; i++ {
		at := shadowNow.Add(time.Duration(i) * time.Hour)
		runs = append(runs, scheduledBead{
			ID:        "gt-run",
			Status:    "closed",
			CreatedAt: at,
			Notes:     "cursor: " + at.Format(time.RFC3339),
		})
	}
	// Newest first, as a list may return them: the join orders by age.
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}

	notes := shadowNotes(runs)
	lines := strings.Split(strings.TrimSuffix(notes, "\n"), "\n")
	if len(lines) != shadowNotesBeads {
		t.Fatalf("joined %d notes, want the bound %d", len(lines), shadowNotesBeads)
	}
	if want := "cursor: " + runs[0].CreatedAt.Format(time.RFC3339); lines[len(lines)-1] != want {
		t.Errorf("last joined note = %q, want the newest run's %q", lines[len(lines)-1], want)
	}
}

// TestScheduledSlingValidateDispatchShadow pins the shadow dispatch value:
// accepted without an agent, refused with one (nothing would run it), and a
// value the patrol does not know still names both it accepts.
func TestScheduledSlingValidateDispatchShadow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(*ScheduledSlingEntry)
		wantErr  string
		wantOKay bool
	}{
		{name: "shadow alone", mutate: func(*ScheduledSlingEntry) {}, wantOKay: true},
		{name: "shadow with agent", mutate: func(e *ScheduledSlingEntry) { e.Agent = "deepseek-pro" }, wantErr: "slings nothing"},
		{name: "shadow with run_agent", mutate: func(e *ScheduledSlingEntry) { e.RunAgent = "deepseek-pro" }, wantErr: "run_agent is valid only"},
		{name: "unknown dispatch", mutate: func(e *ScheduledSlingEntry) { e.Dispatch = "maybe" }, wantErr: "must be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := shadowEntry()
			tc.mutate(&e)
			err := scheduledSlingValidateDispatch(e)
			if tc.wantOKay {
				if err != nil {
					t.Fatalf("validate: %v, want the entry accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validate accepted the entry, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}
