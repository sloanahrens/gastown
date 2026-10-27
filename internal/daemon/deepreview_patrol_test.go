package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
		"records=2",
		"unparsed=0",
		"reviewed_until_old=2026-10-26T02:00:00Z",
		"reviewed_until_new=2026-10-26T04:00:00Z",
	} {
		if i >= len(lines) || lines[i] != want {
			t.Fatalf("plan note line %d = %q, want %q (note: %s)", i, lineAt(lines, i), want, notes[0])
		}
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
		line := lineAt(lines, 7+i)
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("plan note line %d is not the landings.jsonl shape: %v (%q)", 7+i, err, line)
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

// TestShadowPlanNoteRendersTheShellPlanShape pins the note to the shape
// scripts/deep-review.sh plan prints: the seven key lines, then one landings
// line per reviewed landing.
func TestShadowPlanNoteRendersTheShellPlanShape(t *testing.T) {
	t.Parallel()
	since := deepreview.Timestamp{Time: shadowNow.Add(-24 * time.Hour), Text: "2026-10-25T12:00:00Z"}
	until := deepreview.Timestamp{Time: shadowNow, Text: "2026-10-26T12:00:00Z"}
	landing := deepreview.Record{
		Bead: "gt-a", Head: "aaaa", LandedCommit: "c1",
		LandedAt:  deepreview.Timestamp{Time: shadowNow.Add(-time.Hour), Text: "2026-10-26T11:00:00Z"},
		RiskPaths: []string{"internal/daemon/a.go"},
	}
	cases := []struct {
		name string
		plan deepreview.Plan
		want []string
	}{
		{
			name: "quiet",
			plan: deepreview.Plan{Since: since, ReviewedUntil: since, Quiet: true, Records: 4, Landings: []deepreview.Record{}},
			want: []string{"mode=landings", "quiet=true", "landings=0", "records=4", "unparsed=0",
				"reviewed_until_old=2026-10-25T12:00:00Z", "reviewed_until_new=2026-10-25T12:00:00Z"},
		},
		{
			name: "landings",
			plan: deepreview.Plan{Since: since, ReviewedUntil: until, Records: 4, Unparsed: 1, Landings: []deepreview.Record{landing}},
			want: []string{"mode=landings", "quiet=false", "landings=1", "records=4", "unparsed=1",
				"reviewed_until_old=2026-10-25T12:00:00Z", "reviewed_until_new=2026-10-26T12:00:00Z",
				`{"bead":"gt-a","head":"aaaa","landed_commit":"c1","landed_at":"2026-10-26T11:00:00Z","risk_paths":["internal/daemon/a.go"]}`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			note, err := shadowPlanNote(shadowModeLandings, tc.plan)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got, want := strings.Split(note, "\n"), tc.want; !slices.Equal(got, want) {
				t.Errorf("note =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if strings.HasSuffix(note, "\n") {
				t.Error("the note ends in a newline, want the block to end on its last line")
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
