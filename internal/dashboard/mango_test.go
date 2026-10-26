package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
)

// The panel's clock: the fixture's generated_at is this instant, so an age in a
// test is the age the test meant.
var mangoNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// mangoFixture writes a metrics directory holding panel.json, and returns its
// path. An empty panel writes no file, leaving the directory as it is.
func mangoFixture(t *testing.T, panel string) string {
	t.Helper()
	dir := t.TempDir()
	if panel != "" {
		if err := os.WriteFile(filepath.Join(dir, mangoPanelFile), []byte(panel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// mangoPanelMap is a valid panel's fields around the targets given, for tests
// that add keys of their own beside them.
func mangoPanelMap(gen string, targets ...map[string]any) map[string]any {
	return map[string]any{
		"schema_version": mangoSchema,
		"generated_at":   gen,
		"items":          412,
		"snapshot_hours": 168,
		"targets":        targets,
	}
}

// mangoJSON marshals a panel map, failing the test on a value JSON cannot carry.
func mangoJSON(t *testing.T, p map[string]any) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mangoPanelJSON builds a valid panel around the targets given, generated at
// gen, with one landing count and one snapshot-hour count.
func mangoPanelJSON(t *testing.T, gen string, targets ...map[string]any) string {
	t.Helper()
	return mangoJSON(t, mangoPanelMap(gen, targets...))
}

// mangoConditionRow is one best-box condition as the writer writes it.
func mangoConditionRow(text string, share float64, narrows bool) map[string]any {
	return map[string]any{"text": text, "share": share, "narrows": narrows}
}

// mangoWeekRow is one week of the trend as the writer writes it.
func mangoWeekRow(week string, landings int, share float64) map[string]any {
	return map[string]any{"week": week, "landings": landings, "rejected_share": share}
}

// mangoEventRow is one regime event as the writer writes it.
func mangoEventRow(date, what string) map[string]any {
	return map[string]any{"date": date, "what": what}
}

func mangoTargetMap(target string, positives int, verdict, box string) map[string]any {
	return map[string]any{
		"target": target, "positives": positives, "skipped": false, "best_box": box,
		"holdout_n": 96, "holdout_rate": 0.42, "baseline": 0.25, "verdict": verdict,
	}
}

// The panel the writer produces — the file in testdata, built from the plan's
// own example — reads fresh, with its rows, its counts and what the analysis
// covered.
func TestMangoReadsTheCollectedPanel(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := ReadMango(mangoFixture(t, string(raw)), mangoNow)
	if m.State != MangoFresh || m.Note != "fresh" {
		t.Fatalf("state %q note %q, want %q fresh", m.State, m.Note, MangoFresh)
	}
	if m.GeneratedAt == nil || !m.GeneratedAt.Equal(mangoNow) {
		t.Errorf("generated_at %v, want %v", m.GeneratedAt, mangoNow)
	}
	if m.Items != 412 || m.SnapshotHours != 168 {
		t.Errorf("items %d snapshot hours %d, want 412 and 168", m.Items, m.SnapshotHours)
	}
	if want := (MangoCounts{Earning: 1, NoSignal: 1, NotYet: 1}); m.Counts != want {
		t.Errorf("counts %+v, want %+v", m.Counts, want)
	}
	if len(m.Targets) != 3 {
		t.Fatalf("%d targets, want 3", len(m.Targets))
	}
	if got := m.Targets[0]; got.Target != "rejected_before_landing" || got.Verdict != MangoEarning ||
		got.Positives != 48 || got.BestBox != "model = opus and attempts in q4" ||
		got.HoldoutN != 50 || got.HoldoutRate != 0.42 || got.Baseline != 0.25 {
		t.Errorf("an earning target reads %+v", got)
	}
	if got := m.Targets[2]; !got.Skipped || got.Verdict != MangoNotYet || got.BestBox != "" {
		t.Errorf("a skipped target reads %+v", got)
	}
}

// The writer's newer per-target keys — label, holdout positives, Wilson
// interval, thin flag and reason — reach the page from the golden panel as the
// writer wrote them, and the target that carries no box carries none of them.
func TestMangoReadsTheNewTargetKeys(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := ReadMango(mangoFixture(t, string(raw)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if len(m.Targets) != 3 {
		t.Fatalf("%d targets, want 3", len(m.Targets))
	}

	earning, replicated := m.Targets[0], m.Targets[1]
	if earning.Label != "Landings rejected before they land" {
		t.Errorf("the earning target's label reads %q", earning.Label)
	}
	if earning.HoldoutPositives == nil || *earning.HoldoutPositives != 21 {
		t.Errorf("the earning target's holdout positives read %v", earning.HoldoutPositives)
	}
	if earning.CILow == nil || *earning.CILow != 0.2937479723456693 ||
		earning.CIHigh == nil || *earning.CIHigh != 0.5576680331222217 {
		t.Errorf("the earning target's interval reads %v..%v", earning.CILow, earning.CIHigh)
	}
	if earning.Thin == nil || *earning.Thin {
		t.Errorf("the earning target's thin flag reads %v, want false", earning.Thin)
	}
	if earning.Reason != "" {
		t.Errorf("a run target carries the reason %q", earning.Reason)
	}

	if replicated.Label != "manual_rescue" {
		t.Errorf("the no-signal target's label reads %q, want the id the writer has no title for", replicated.Label)
	}
	if replicated.HoldoutPositives == nil || *replicated.HoldoutPositives != 5 {
		t.Errorf("the no-signal target's holdout positives read %v", replicated.HoldoutPositives)
	}
	if replicated.Thin == nil || !*replicated.Thin {
		t.Errorf("the no-signal target's thin flag reads %v, want true", replicated.Thin)
	}

	skipped := m.Targets[2]
	if skipped.Label != "Beads stopped by the overrun guard" {
		t.Errorf("the skipped target's label reads %q", skipped.Label)
	}
	if skipped.Reason != mangoReasonFewPositives {
		t.Errorf("the skipped target's reason reads %q", skipped.Reason)
	}
	if skipped.HoldoutPositives != nil || skipped.CILow != nil || skipped.CIHigh != nil || skipped.Thin != nil {
		t.Errorf("a target with no box reads a box: %+v", skipped)
	}
}

// A panel from the writer before these keys existed reads exactly as it did:
// every row is the keys it always had, and the new keys are absent from the
// page's payload rather than present as zeroes the page would draw.
func TestMangoReadsAPanelWithoutTheNewKeys(t *testing.T) {
	t.Parallel()

	m := ReadMango(mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
		mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3"))), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if len(m.Targets) != 1 {
		t.Fatalf("%d targets, want 1", len(m.Targets))
	}
	got := m.Targets[0]
	want := MangoTarget{
		Target: "manual_rescue", Positives: 37, BestBox: "attempts in q3",
		HoldoutN: 96, HoldoutRate: 0.42, Baseline: 0.25, Verdict: MangoNoSignal,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a panel without the new keys reads %+v, want %+v", got, want)
	}
	if m.Counts != (MangoCounts{NoSignal: 1}) {
		t.Errorf("counts %+v", m.Counts)
	}

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"label", "holdout_positives", "ci_low", "ci_high", "thin", "reason"} {
		if _, ok := payload[key]; ok {
			t.Errorf("an absent %q reached the page's payload: %s", key, b)
		}
	}
}

// The optional keys the reader cannot place are dropped one at a time, never as
// an error state for the panel: the row keeps every other field and the panel
// still reads fresh.
func TestMangoDropsOptionalKeysItCannotPlace(t *testing.T) {
	t.Parallel()

	// The row every case starts from: a no-signal box with 5 positives in 49,
	// an interval inside 0..1, and the thin flag.
	base := func() map[string]any {
		row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3..q4")
		row["holdout_n"] = 49
		row["holdout_positives"] = 5
		row["ci_low"], row["ci_high"] = 0.04, 0.22
		row["thin"] = true
		return row
	}

	for _, tc := range []struct {
		name string
		set  map[string]any
		want func(t *testing.T, got MangoTarget)
	}{
		{
			name: "holdout positives above the holdout",
			set:  map[string]any{"holdout_positives": 50},
			want: func(t *testing.T, got MangoTarget) {
				if got.HoldoutPositives != nil {
					t.Errorf("a count above the holdout reads %d", *got.HoldoutPositives)
				}
			},
		},
		{
			name: "negative holdout positives",
			set:  map[string]any{"holdout_positives": -1},
			want: func(t *testing.T, got MangoTarget) {
				if got.HoldoutPositives != nil {
					t.Errorf("a negative count reads %d", *got.HoldoutPositives)
				}
			},
		},
		{
			name: "interval below zero",
			set:  map[string]any{"ci_low": -0.01},
			want: func(t *testing.T, got MangoTarget) {
				if got.CILow != nil || got.CIHigh != nil {
					t.Errorf("a negative bound leaves the interval %v..%v", got.CILow, got.CIHigh)
				}
			},
		},
		{
			name: "interval above one",
			set:  map[string]any{"ci_high": 1.01},
			want: func(t *testing.T, got MangoTarget) {
				if got.CILow != nil || got.CIHigh != nil {
					t.Errorf("a bound over one leaves the interval %v..%v", got.CILow, got.CIHigh)
				}
			},
		},
		{
			name: "interval reversed",
			set:  map[string]any{"ci_low": 0.6},
			want: func(t *testing.T, got MangoTarget) {
				if got.CILow != nil || got.CIHigh != nil {
					t.Errorf("an interval whose low is above its high reads %v..%v", got.CILow, got.CIHigh)
				}
			},
		},
		{
			name: "unknown reason",
			set:  map[string]any{"reason": "too_old"},
			want: func(t *testing.T, got MangoTarget) {
				if got.Reason != "" {
					t.Errorf("an unknown reason reads %q", got.Reason)
				}
			},
		},
		{
			name: "count and interval the writer could have measured",
			set:  map[string]any{"holdout_positives": 49, "ci_low": 0, "ci_high": 1},
			want: func(t *testing.T, got MangoTarget) {
				if got.HoldoutPositives == nil || *got.HoldoutPositives != 49 {
					t.Errorf("a count at the holdout reads %v", got.HoldoutPositives)
				}
				if got.CILow == nil || *got.CILow != 0 || got.CIHigh == nil || *got.CIHigh != 1 {
					t.Errorf("an interval at the bounds reads %v..%v", got.CILow, got.CIHigh)
				}
			},
		},
		{
			name: "reason the page has a sentence for",
			set:  map[string]any{"reason": mangoReasonNoHoldoutPositives},
			want: func(t *testing.T, got MangoTarget) {
				if got.Reason != mangoReasonNoHoldoutPositives {
					t.Errorf("the writer's reason reads %q", got.Reason)
				}
			},
		},
		{
			name: "thin flag false stays present",
			set:  map[string]any{"thin": false},
			want: func(t *testing.T, got MangoTarget) {
				if got.Thin == nil || *got.Thin {
					t.Errorf("a false thin flag reads %v, want a present false", got.Thin)
				}
			},
		},
	} {
		row := base()
		for k, v := range tc.set {
			row[k] = v
		}
		m := ReadMango(mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339), row)), mangoNow)
		if m.State != MangoFresh {
			t.Errorf("%s: state %q note %q, want %q", tc.name, m.State, m.Note, MangoFresh)
			continue
		}
		got := m.Targets[0]
		if got.Target != "manual_rescue" || got.Positives != 37 || got.BestBox != "attempts in q3..q4" ||
			got.HoldoutN != 49 || got.HoldoutRate != 0.42 || got.Verdict != MangoNoSignal {
			t.Errorf("%s: the drop took the rest of the row: %+v", tc.name, got)
		}
		if m.Counts != (MangoCounts{NoSignal: 1}) {
			t.Errorf("%s: counts %+v", tc.name, m.Counts)
		}
		tc.want(t, got)
	}
}

// The label is another process's text like every other string in the panel: the
// invisible runes are stripped, an over-long title is marked at the cap, and the
// page escapes the markup that is left.
func TestMangoStripsAndCapsTheTargetLabel(t *testing.T) {
	t.Parallel()

	hostile := "bell:\x07 esc:\x1b[31m rlo:‮ invisible:​ <b>bold</b> " + strings.Repeat("é", 80)
	row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")
	row["label"] = hostile
	m := ReadMango(mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339), row)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	got := m.Targets[0].Label
	for _, r := range got {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("the label kept the invisible rune %U: %q", r, got)
		}
	}
	if !strings.HasPrefix(got, "bell: esc:[31m rlo: invisible: <b>bold</b> ") {
		t.Errorf("the label lost text it should have kept: %q", got)
	}
	if n := len([]rune(got)); n != mangoLabelMax+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("the label is %d runes and does not end at the %d-rune cap: %q", n, mangoLabelMax, got)
	}
}

// A panel generated within the last 36 hours is fresh, one older than that is
// stale: the analysis is rebuilt daily, so a day and a half is when its absence
// is news.
func TestMangoReadsAFreshOrStalePanel(t *testing.T) {
	t.Parallel()

	panel := func(age time.Duration) string {
		return mangoPanelJSON(t, mangoNow.Add(-age).Format(time.RFC3339),
			mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3..q4"))
	}
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{
		{time.Minute, MangoFresh},
		{35 * time.Hour, MangoFresh},
		{mangoStale, MangoFresh},
		{mangoStale + time.Minute, MangoStale},
		{37 * time.Hour, MangoStale},
		{10 * 24 * time.Hour, MangoStale},
	} {
		m := ReadMango(mangoFixture(t, panel(tc.age)), mangoNow)
		if m.State != tc.want {
			t.Errorf("a panel %v old reads %q, want %q", tc.age, m.State, tc.want)
		}
		if m.State == MangoStale && m.Note != "stale" {
			t.Errorf("a stale panel notes %q", m.Note)
		}
		if len(m.Targets) != 1 {
			t.Errorf("a panel %v old lost its targets: %+v", tc.age, m.Targets)
		}
	}
}

// Every input the panel cannot use is a named state rather than a blank pane or
// a crash: no directory, no panel yet, a panel too big to be the writer's, one
// that is not JSON, one from a schema this panel does not know, one with no
// usable generated_at, and one naming a verdict the page has no row for.
func TestMangoNamesEveryStateItCannotRead(t *testing.T) {
	t.Parallel()

	valid := mangoPanelJSON(t, mangoNow.Format(time.RFC3339), mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3"))
	oversize := mangoFixture(t, `{"schema_version":1,"generated_at":"2026-10-24T12:00:00Z","targets":[],"pad":"`+strings.Repeat("x", mangoPanelMaxBytes)+`"}`)
	noDir := filepath.Join(t.TempDir(), "gone")

	for _, tc := range []struct {
		name  string
		dir   string
		state string
		note  string
	}{
		{"no directory", noDir, MangoMissing, mangoNoPanel},
		{"directory with no panel", mangoFixture(t, ""), MangoMissing, mangoNoPanel},
		{"oversize panel", oversize, MangoError, mangoOversize},
		{"panel that is not JSON", mangoFixture(t, "{not json"), MangoError, "the panel is not valid JSON"},
		{"unknown schema", mangoFixture(t, `{"schema_version":7,"generated_at":"2026-10-24T12:00:00Z","targets":[]}`), MangoError, "unknown schema version 7"},
		{"no generated_at", mangoFixture(t, `{"schema_version":1,"targets":[]}`), MangoError, "the panel has no generated_at timestamp"},
		{"generated_at that is not a time", mangoFixture(t, mangoPanelJSON(t, "yesterday")), MangoError, "the panel has no generated_at timestamp"},
		{"unknown verdict", mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
			mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3"),
			mangoTargetMap("another", 12, "maybe", "attempts in q4"))), MangoError, `unknown target verdict "maybe"`},
	} {
		m := ReadMango(tc.dir, mangoNow)
		if m.State != tc.state || m.Note != tc.note {
			t.Errorf("%s: state %q note %q, want %q %q", tc.name, m.State, m.Note, tc.state, tc.note)
		}
		// A state the reader could not read past still leaves the page a page:
		// the field it always draws is there.
		if m.Targets == nil {
			t.Errorf("%s: targets are null in the page's payload", tc.name)
		}
		// Nothing the reader rejected reaches the page's fields: a state it
		// cannot read carries no coverage, no rows and no counts from the file
		// it could not use.
		if m.GeneratedAt != nil || m.Items != 0 || m.SnapshotHours != 0 ||
			len(m.Targets) != 0 || m.Counts != (MangoCounts{}) {
			t.Errorf("%s: an unread state carries panel data: %+v", tc.name, m)
		}
	}

	// The valid panel this test built is the one the other states are compared
	// against, so a reader that rejected everything would not pass as strict.
	if m := ReadMango(mangoFixture(t, valid), mangoNow); m.State != MangoFresh {
		t.Errorf("the valid panel reads %q note %q", m.State, m.Note)
	}
}

// The size cap is checked from the file's stat before its bytes are parsed. The
// fixture is bigger than the cap and is not JSON either, so the answer names the
// cap only if the size was read before the bytes were parsed; the file is
// readable by every user, so the answer does not depend on who runs the test.
func TestMangoRefusesAnOversizePanelBeforeParsingIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, mangoPanelFile)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", mangoPanelMaxBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	m := ReadMango(dir, mangoNow)
	if m.State != MangoError || m.Note != mangoOversize {
		t.Errorf("state %q note %q, want %q %q", m.State, m.Note, MangoError, mangoOversize)
	}
}

// The panel is text from another process. Control characters and invisible
// format characters are stripped, an over-long field is cut to the cap, and an
// HTML tag is left as the text it is — the page escapes it by writing the cell
// with textContent.
func TestMangoStripsAndCapsPanelText(t *testing.T) {
	t.Parallel()

	hostile := "bell:\x07 esc:\x1b[31m nul:\x00 rlo:‮invisible:​ <b>bold</b> " + strings.Repeat("x", 400)
	m := ReadMango(mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
		mangoTargetMap("rej\x07ected", 48, "EARNING", hostile))), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	row := m.Targets[0]
	for name, s := range map[string]string{"target": row.Target, "best box": row.BestBox} {
		for _, r := range s {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Errorf("%s kept the invisible rune %U: %q", name, r, s)
			}
		}
		if strings.ContainsAny(s, "\n\r\t") {
			t.Errorf("%s kept a line break: %q", name, s)
		}
	}
	if n := len([]rune(row.BestBox)); n > mangoBoxMax+1 || !strings.HasSuffix(row.BestBox, "…") {
		t.Errorf("the best box is %d runes and was not marked past the %d-rune cap", n, mangoBoxMax)
	}
	if !strings.Contains(row.BestBox, "<b>bold</b>") {
		t.Errorf("the reader mangled punctuation it was not asked to touch: %q", row.BestBox)
	}
	if row.Target != "rejected" {
		t.Errorf("stripping changed more than the invisible runes: %q", row.Target)
	}
	// The verdict is the writer's word, so the page's comparison is made against
	// the lowercased form.
	if row.Verdict != MangoEarning || m.Counts.Earning != 1 {
		t.Errorf("an upper-cased verdict reads %+v with counts %+v", row, m.Counts)
	}
}

// A read opens the panel for reading only: a read-only file reads, and the
// directory is the same afterwards — no rewrite, no temp file.
func TestMangoOpensThePanelForReadingOnly(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := mangoFixture(t, string(raw))
	path := filepath.Join(dir, mangoPanelFile)
	// A write-mode open of a 0444 file is refused for any user but root, so an
	// implementation that opened the panel to write it would fail here.
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	m := ReadMango(dir, mangoNow) // fresh
	ReadMango(dir, mangoNow.Add(48*time.Hour))
	if m.State != MangoFresh {
		t.Fatalf("a read-only panel reads %q note %q", m.State, m.Note)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Error("a read changed the panel's bytes")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o444 {
		t.Errorf("a read changed the panel's mode to %v", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != mangoPanelFile {
		t.Errorf("a read left the metrics directory different: %v", entries)
	}
}

// A panel path that is not a regular file is an error state rather than a file
// read: a symlink named like the panel is refused, not followed, the way cloud.go
// refuses a link named like the patrol's report.
func TestMangoRefusesANonRegularPanel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "elsewhere.json"), []byte(mangoPanelJSON(t, mangoNow.Format(time.RFC3339))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere.json"), filepath.Join(dir, mangoPanelFile)); err != nil {
		t.Fatal(err)
	}
	if m := ReadMango(dir, mangoNow); m.State != MangoError {
		t.Errorf("a symlinked panel reads %q note %q, want %q", m.State, m.Note, MangoError)
	}
}

// The best box's conditions and the target's last positive day reach the page
// from the golden panel: the box's own order, each share and narrows flag as
// written, and neither key on a target that was never run. Guards
// `row.Conditions = mangoConditions(t.Conditions)` and the `mangoDay` check on
// last_positive in mangoTargets.
func TestMangoReadsTheConditionsAndLastPositive(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := ReadMango(mangoFixture(t, string(raw)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if len(m.Targets) != 3 {
		t.Fatalf("%d targets, want 3", len(m.Targets))
	}

	earning := m.Targets[0]
	want := []MangoCondition{
		{Text: "model in {opus}", Share: 0.58, Narrows: true},
		{Text: "attempts in [3, 4]", Share: 0.41, Narrows: true},
	}
	if !reflect.DeepEqual(earning.Conditions, want) {
		t.Errorf("the earning target's conditions read %+v, want %+v", earning.Conditions, want)
	}
	if earning.LastPositive != "2026-10-20" {
		t.Errorf("the earning target's last positive reads %q", earning.LastPositive)
	}

	noSignal := m.Targets[1]
	if len(noSignal.Conditions) != 2 {
		t.Fatalf("the no-signal target's conditions read %+v, want two", noSignal.Conditions)
	}
	if c := noSignal.Conditions[1]; c.Text != "files_touched in {none}" || c.Share != 0.94 || c.Narrows {
		t.Errorf("a condition the box does not narrow reads %+v", c)
	}
	if noSignal.LastPositive != "2026-10-21" {
		t.Errorf("the no-signal target's last positive reads %q", noSignal.LastPositive)
	}

	if skipped := m.Targets[2]; skipped.Conditions != nil || skipped.LastPositive != "" {
		t.Errorf("a target with no box reads conditions %+v and last positive %q", skipped.Conditions, skipped.LastPositive)
	}
}

// The panel's weekly series and regime events reach the page from the golden
// panel in the writer's oldest-first order, every field intact. Guards the
// `m.Weekly, m.Events = mangoWeeks(p.Weekly), mangoEvents(p.Events)` assignment.
func TestMangoReadsTheWeeklySeriesAndEvents(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := ReadMango(mangoFixture(t, string(raw)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	want := []MangoWeek{
		{Week: "2026-10-05", Landings: 27, RejectedShare: 0.25},
		{Week: "2026-10-12", Landings: 34, RejectedShare: 0.125},
	}
	if !reflect.DeepEqual(m.Weekly, want) {
		t.Errorf("the weekly series reads %+v, want %+v", m.Weekly, want)
	}
	wantEvents := []MangoEvent{{Date: "2026-10-10", What: "flash seats replaced by Sonnet seats"}}
	if !reflect.DeepEqual(m.Events, wantEvents) {
		t.Errorf("the events read %+v, want %+v", m.Events, wantEvents)
	}
}

// A panel from the writer before the trend keys existed reads as before, and
// one the reader never read still hands the page an empty list: weekly and
// events marshal as [] rather than null, and a target carries neither
// conditions nor a last positive. Guards the Weekly and Events initializers in
// ReadMango and mangoWeeks' and mangoEvents' empty returns.
func TestMangoReadsAPanelWithoutTheTrend(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"panel without the keys", mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
			mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")))},
		{"no panel at all", mangoFixture(t, "")},
	} {
		m := ReadMango(tc.dir, mangoNow)
		if m.Weekly == nil || len(m.Weekly) != 0 {
			t.Errorf("%s: weekly reads %+v, want an empty list", tc.name, m.Weekly)
		}
		if m.Events == nil || len(m.Events) != 0 {
			t.Errorf("%s: events read %+v, want an empty list", tc.name, m.Events)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"weekly", "events"} {
			if got := string(payload[key]); got != "[]" {
				t.Errorf("%s: %s marshals as %s, want []", tc.name, key, got)
			}
		}
	}

	m := ReadMango(mangoFixture(t, mangoPanelJSON(t, mangoNow.Format(time.RFC3339),
		mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3"))), mangoNow)
	b, err := json.Marshal(m.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"conditions", "last_positive"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("an absent %q reached the page's payload: %s", key, b)
		}
	}
}

// Every row the reader cannot place is dropped on its own and the panel still
// reads fresh: a bad share, a date that is not a calendar day, negative
// landings and a share outside 0..1 each cost their row, never the rest of the
// panel. Guards the `!mangoFraction(c.Share)` check in mangoConditions, the
// `mangoDay` check on last_positive, and the week, landings and share checks in
// mangoWeeks and the date check in mangoEvents.
func TestMangoDropsTheRowsItCannotPlace(t *testing.T) {
	t.Parallel()

	oneTarget := func(mutate func(row map[string]any)) string {
		row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")
		mutate(row)
		return mangoPanelJSON(t, mangoNow.Format(time.RFC3339), row)
	}

	for _, tc := range []struct {
		name  string
		panel string
		want  func(t *testing.T, m *Mango)
	}{
		{
			name: "a condition share below zero",
			panel: oneTarget(func(row map[string]any) {
				row["conditions"] = []map[string]any{
					mangoConditionRow("kept", 0.4, true),
					mangoConditionRow("dropped", -0.01, true),
				}
			}),
			want: func(t *testing.T, m *Mango) {
				if got := m.Targets[0].Conditions; len(got) != 1 || got[0].Text != "kept" {
					t.Errorf("conditions read %+v, want only the kept one", got)
				}
			},
		},
		{
			name: "a condition share above one",
			panel: oneTarget(func(row map[string]any) {
				row["conditions"] = []map[string]any{mangoConditionRow("dropped", 1.01, true)}
			}),
			want: func(t *testing.T, m *Mango) {
				if got := m.Targets[0].Conditions; len(got) != 0 {
					t.Errorf("conditions read %+v, want none", got)
				}
			},
		},
		{
			name: "shares at the bounds are proportions",
			panel: oneTarget(func(row map[string]any) {
				row["conditions"] = []map[string]any{
					mangoConditionRow("none", 0, false),
					mangoConditionRow("all", 1, true),
				}
			}),
			want: func(t *testing.T, m *Mango) {
				if got := m.Targets[0].Conditions; len(got) != 2 ||
					got[0].Share != 0 || got[1].Share != 1 || got[0].Narrows {
					t.Errorf("conditions at the bounds read %+v", got)
				}
			},
		},
		{
			name: "a last positive that is not a calendar day",
			panel: oneTarget(func(row map[string]any) {
				row["last_positive"] = "2026-10-5"
			}),
			want: func(t *testing.T, m *Mango) {
				if got := m.Targets[0].LastPositive; got != "" {
					t.Errorf("a last positive that is not a day reads %q", got)
				}
			},
		},
		{
			name: "a last positive that is a calendar day",
			panel: oneTarget(func(row map[string]any) {
				row["last_positive"] = "2026-10-20"
			}),
			want: func(t *testing.T, m *Mango) {
				if got := m.Targets[0].LastPositive; got != "2026-10-20" {
					t.Errorf("a last positive day reads %q", got)
				}
			},
		},
		{
			name: "a week that is not a calendar day",
			panel: withTrend(t, []map[string]any{
				mangoWeekRow("2026-10-5", 27, 0.25),
				mangoWeekRow("2026-10-12", 34, 0.125),
			}, nil),
			want: func(t *testing.T, m *Mango) {
				if len(m.Weekly) != 1 || m.Weekly[0].Week != "2026-10-12" {
					t.Errorf("weekly reads %+v, want only the later week", m.Weekly)
				}
			},
		},
		{
			name: "a week with negative landings",
			panel: withTrend(t, []map[string]any{
				mangoWeekRow("2026-10-05", -1, 0.25),
				mangoWeekRow("2026-10-12", 34, 0.125),
			}, nil),
			want: func(t *testing.T, m *Mango) {
				if len(m.Weekly) != 1 || m.Weekly[0].Landings != 34 {
					t.Errorf("weekly reads %+v, want only the positive week", m.Weekly)
				}
			},
		},
		{
			name: "a week share below zero",
			panel: withTrend(t, []map[string]any{
				mangoWeekRow("2026-10-05", 27, -0.01),
			}, nil),
			want: func(t *testing.T, m *Mango) {
				if len(m.Weekly) != 0 {
					t.Errorf("weekly reads %+v, want none", m.Weekly)
				}
			},
		},
		{
			name: "a week share above one",
			panel: withTrend(t, []map[string]any{
				mangoWeekRow("2026-10-05", 27, 1.01),
			}, nil),
			want: func(t *testing.T, m *Mango) {
				if len(m.Weekly) != 0 {
					t.Errorf("weekly reads %+v, want none", m.Weekly)
				}
			},
		},
		{
			name: "a week share at the bounds",
			panel: withTrend(t, []map[string]any{
				mangoWeekRow("2026-10-05", 27, 0),
				mangoWeekRow("2026-10-12", 34, 1),
			}, nil),
			want: func(t *testing.T, m *Mango) {
				if len(m.Weekly) != 2 {
					t.Errorf("weekly at the bounds reads %+v", m.Weekly)
				}
			},
		},
		{
			name: "an event date that is not a calendar day",
			panel: withTrend(t, nil, []map[string]any{
				mangoEventRow("2026-13-01", "dropped"),
				mangoEventRow("2026-10-10", "kept"),
			}),
			want: func(t *testing.T, m *Mango) {
				if len(m.Events) != 1 || m.Events[0].What != "kept" {
					t.Errorf("events read %+v, want only the kept one", m.Events)
				}
			},
		},
	} {
		m := ReadMango(mangoFixture(t, tc.panel), mangoNow)
		if m.State != MangoFresh {
			t.Errorf("%s: state %q note %q, want %q", tc.name, m.State, m.Note, MangoFresh)
			continue
		}
		if m.Items != 412 || m.SnapshotHours != 168 {
			t.Errorf("%s: the drop took the panel's coverage: %+v", tc.name, m)
		}
		tc.want(t, m)
	}
}

// withTrend is a valid panel at mangoNow carrying the weekly and event rows
// given beside one target, each list left out when nil.
func withTrend(t *testing.T, weekly, events []map[string]any) string {
	t.Helper()
	p := mangoPanelMap(mangoNow.Format(time.RFC3339),
		mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3"))
	if weekly != nil {
		p["weekly"] = weekly
	}
	if events != nil {
		p["events"] = events
	}
	return mangoJSON(t, p)
}

// A list over its cap keeps its newest entries: 27 weeks keep the last 26, 17
// events the last 16. Guards `out = out[len(out)-mangoWeeklyMax:]` in
// mangoWeeks and `out = out[len(out)-mangoEventsMax:]` in mangoEvents.
func TestMangoKeepsTheNewestRowsPastTheCap(t *testing.T) {
	t.Parallel()

	day := func(i int) string {
		return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(mangoDayLayout)
	}
	var weeks []map[string]any
	for i := 0; i < mangoWeeklyMax+1; i++ {
		weeks = append(weeks, mangoWeekRow(day(i), i, 0.25))
	}
	var events []map[string]any
	for i := 0; i < mangoEventsMax+1; i++ {
		events = append(events, mangoEventRow(day(i), "event"))
	}
	m := ReadMango(mangoFixture(t, withTrend(t, weeks, events)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if len(m.Weekly) != mangoWeeklyMax {
		t.Fatalf("%d weeks, want the %d-row cap", len(m.Weekly), mangoWeeklyMax)
	}
	if first, last := m.Weekly[0], m.Weekly[len(m.Weekly)-1]; first.Landings != 1 || last.Landings != mangoWeeklyMax {
		t.Errorf("the kept weeks read %d..%d, want the newest (%d..%d)", first.Landings, last.Landings, 1, mangoWeeklyMax)
	}
	if len(m.Events) != mangoEventsMax {
		t.Fatalf("%d events, want the %d-event cap", len(m.Events), mangoEventsMax)
	}
	if got := m.Events[len(m.Events)-1].Date; got != day(mangoEventsMax) {
		t.Errorf("the kept events end at %s, want the newest %s", got, day(mangoEventsMax))
	}
}

// A box with more conditions than the cap contributes its first 16 in the box's
// own order, the writer's own rule, and a dropped share does not shorten the
// list below the cap. Guards the `len(out) == mangoConditionsMax` break in
// mangoConditions.
func TestMangoCapsConditionsAtTheBoxsFirst(t *testing.T) {
	t.Parallel()

	build := func(n int, badFirst bool) string {
		var conditions []map[string]any
		if badFirst {
			conditions = append(conditions, mangoConditionRow("dropped", 2, true))
		}
		for i := 0; i < n; i++ {
			conditions = append(conditions, mangoConditionRow(fmt.Sprintf("condition %d", i), 0.1, true))
		}
		row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")
		row["conditions"] = conditions
		return mangoPanelJSON(t, mangoNow.Format(time.RFC3339), row)
	}

	m := ReadMango(mangoFixture(t, build(mangoConditionsMax+1, false)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if got := m.Targets[0].Conditions; len(got) != mangoConditionsMax {
		t.Fatalf("%d conditions, want the %d cap", len(got), mangoConditionsMax)
	}
	if got := m.Targets[0].Conditions[mangoConditionsMax-1].Text; got != fmt.Sprintf("condition %d", mangoConditionsMax-1) {
		t.Errorf("the last kept condition reads %q, want the box's first %d in order", got, mangoConditionsMax)
	}

	m = ReadMango(mangoFixture(t, build(mangoConditionsMax, true)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if got := m.Targets[0].Conditions; len(got) != mangoConditionsMax {
		t.Errorf("%d conditions after one bad share, want the cap filled from the rest", len(got))
	}
}

// A condition's text and an event's what are another process's text: the
// invisible runes are stripped, an over-long one is marked at the 120-rune cap,
// and the markup that is left is drawn as text by the page. Guards
// `cloudText(c.Text, mangoFreeTextMax)` in mangoConditions and
// `cloudText(e.What, mangoFreeTextMax)` in mangoEvents.
func TestMangoStripsAndCapsTheTrendText(t *testing.T) {
	t.Parallel()

	hostile := "bell:\x07 esc:\x1b[31m rlo:‮ invisible:​ <b>bold</b> " + strings.Repeat("é", 130)
	row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")
	row["conditions"] = []map[string]any{mangoConditionRow(hostile, 0.4, true)}
	p := mangoPanelMap(mangoNow.Format(time.RFC3339), row)
	p["events"] = []map[string]any{mangoEventRow("2026-10-10", hostile)}
	m := ReadMango(mangoFixture(t, mangoJSON(t, p)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}
	if n := len(m.Targets[0].Conditions); n != 1 || len(m.Events) != 1 {
		t.Fatalf("%d conditions and %d events, want one of each", n, len(m.Events))
	}
	for name, s := range map[string]string{
		"a condition's text": m.Targets[0].Conditions[0].Text,
		"an event's what":    m.Events[0].What,
	} {
		for _, r := range s {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				t.Errorf("%s kept the invisible rune %U: %q", name, r, s)
			}
		}
		if !strings.HasPrefix(s, "bell: esc:[31m rlo: invisible: <b>bold</b> ") {
			t.Errorf("%s lost text it should have kept: %q", name, s)
		}
		if n := len([]rune(s)); n != mangoFreeTextMax+1 || !strings.HasSuffix(s, "…") {
			t.Errorf("%s is %d runes and does not end at the %d-rune cap: %q", name, n, mangoFreeTextMax, s)
		}
	}
}

// mangoObject is a JSON object from a marshalled payload, failing the test when
// v is not one.
func mangoObject(t *testing.T, v any) map[string]any {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%T is not a JSON object", v)
	}
	return obj
}

// mangoFirstObject is the first element of a JSON array of objects, failing the
// test when v is not one.
func mangoFirstObject(t *testing.T, v any) map[string]any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("%T is not a non-empty JSON array", v)
	}
	return mangoObject(t, arr[0])
}

// The page payload names the new keys as the writer's file does, so the reader
// and the page speak one schema. Guards the json tags on Mango.Weekly,
// MangoWeek, MangoEvent, MangoTarget.Conditions, MangoCondition and
// MangoTarget.LastPositive.
func TestMangoPayloadCarriesTheWritersKeyNames(t *testing.T) {
	t.Parallel()

	row := mangoTargetMap("manual_rescue", 37, MangoNoSignal, "attempts in q3")
	row["conditions"] = []map[string]any{mangoConditionRow("attempts in [3, 4]", 0.4, true)}
	row["last_positive"] = "2026-10-20"
	p := mangoPanelMap(mangoNow.Format(time.RFC3339), row)
	p["weekly"] = []map[string]any{mangoWeekRow("2026-10-05", 27, 0.25)}
	p["events"] = []map[string]any{mangoEventRow("2026-10-10", "flash seats")}
	m := ReadMango(mangoFixture(t, mangoJSON(t, p)), mangoNow)
	if m.State != MangoFresh {
		t.Fatalf("state %q note %q", m.State, m.Note)
	}

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatal(err)
	}
	keys := func(at string, obj map[string]any, want ...string) {
		t.Helper()
		for _, k := range want {
			if _, ok := obj[k]; !ok {
				t.Errorf("%s carries no %q: %v", at, k, obj)
			}
		}
	}
	keys("the panel", payload, "weekly", "events")
	keys("a week", mangoFirstObject(t, payload["weekly"]), "week", "landings", "rejected_share")
	keys("an event", mangoFirstObject(t, payload["events"]), "date", "what")
	target := mangoFirstObject(t, payload["targets"])
	keys("a target", target, "conditions", "last_positive")
	keys("a condition", mangoFirstObject(t, target["conditions"]), "text", "share", "narrows")
}
