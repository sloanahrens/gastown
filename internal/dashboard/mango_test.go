package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		got.HoldoutN != 96 || got.HoldoutRate != 0.42 || got.Baseline != 0.25 {
		t.Errorf("an earning target reads %+v", got)
	}
	if got := m.Targets[2]; !got.Skipped || got.Verdict != MangoNotYet || got.BestBox != "" {
		t.Errorf("a skipped target reads %+v", got)
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
			mangoTargetMap("manual_rescue", 37, "maybe", "attempts in q3"))), MangoError, `unknown target verdict "maybe"`},
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
	}

	// The valid panel this test built is the one the other states are compared
	// against, so a reader that rejected everything would not pass as strict.
	if m := ReadMango(mangoFixture(t, valid), mangoNow); m.State != MangoFresh {
		t.Errorf("the valid panel reads %q note %q", m.State, m.Note)
	}
}

// The size cap is checked before the read, not after: the panel is unreadable to
// this process, so a reader that opened it first would report a read error
// rather than the cap.
func TestMangoRefusesAnOversizePanelBeforeReadingIt(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0000 file, so the read could not fail")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, mangoPanelFile)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", mangoPanelMaxBytes+1)), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Fatal("the fixture is readable, so this test cannot tell a stat from a read")
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

// A read opens the panel for reading only: a read-only file in a read-only
// directory reads, and the directory is the same afterwards — no rewrite, no
// temp file.
func TestMangoOpensThePanelForReadingOnly(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := mangoFixture(t, string(raw))
	path := filepath.Join(dir, mangoPanelFile)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

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
