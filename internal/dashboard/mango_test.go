package dashboard

import (
	"encoding/json"
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

// mangoPanelJSON builds a valid panel around the targets given, generated at
// gen, with one landing count and one snapshot-hour count.
func mangoPanelJSON(t *testing.T, gen string, targets ...map[string]any) string {
	t.Helper()
	p := map[string]any{
		"schema_version": mangoSchema,
		"generated_at":   gen,
		"items":          412,
		"snapshot_hours": 168,
		"targets":        targets,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
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
