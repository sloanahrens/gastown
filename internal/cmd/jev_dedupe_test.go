package cmd

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

var updateJevDedupeGolden = flag.Bool("update-jev-dedupe-golden", false, "rewrite testdata/jev_dedupe_recent.golden")

const jevDedupeGoldenPath = "jev_dedupe_recent.golden"

// dedupeLog is four logged findings: one from before the battery existed, a
// full answer set, a no-candidate line with the Nouls and Score but no Choice,
// and a failed call with null answers.
const dedupeLog = `{"ts":"2026-10-20T19:00:00Z","finding":"gt-old","candidate":"gt-old","confidence":0.91,"candidates_offered":["gt-old","gt-x"],"outcome":"answered"}
{"ts":"2026-10-20T19:15:00Z","finding":"gt-aaaaa","candidate":"gt-old","confidence":0.83,"candidates_offered":["gt-old","gt-x"],"outcome":"answered","answers":{"duplicate":{"choice":"gt-old","confidence":0.83},"any_duplicate":{"noul":0.87},"fix_worthy":{"noul":0.92},"report_only":{"noul":0.04},"test_only":{"noul":0.11},"severity":{"score":3,"confidence":0.72}},"question_version":1,"model":"jev-1.13.0"}
{"ts":"2026-10-20T19:30:00Z","finding":"gt-bbbbb","candidate":"none","confidence":0,"candidates_offered":[],"outcome":"no_candidates","answers":{"any_duplicate":{"noul":0},"fix_worthy":{"noul":0.55},"report_only":{"noul":1},"test_only":{"noul":0},"severity":{"score":1,"confidence":0.6}},"question_version":1,"model":"jev-1.13.0"}
{"ts":"2026-10-20T19:45:00Z","finding":"gt-ccccc","candidate":"none","confidence":0,"candidates_offered":["gt-old"],"outcome":"ask_failed","answers":null,"question_version":1,"model":""}
`

func writeDedupeLog(t *testing.T, town, body string) string {
	t.Helper()
	path := dedupe.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --recent 3 prints a header and the last three lines, oldest first, in a
// fixed layout that shows every answer.
func TestJevDedupeRecentGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeDedupeLog(t, town, dedupeLog)

	var out bytes.Buffer
	if err := printRecentDedupe(&out, path, 3); err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", jevDedupeGoldenPath)
	if *updateJevDedupeGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-jev-dedupe-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
	if rows := strings.Split(strings.TrimSpace(out.String()), "\n"); len(rows) != 4 {
		t.Errorf("printed %d rows, want a header and three lines", len(rows))
	}
}

// A failure that logged none reads differently from an answered none.
func TestJevDedupeRecentShowsTheOutcome(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := dedupe.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"ts":"2026-10-19T12:00:00Z","finding":"gt-bad","candidate":"none","confidence":0,"candidates_offered":["gt-old"],"outcome":"ask_failed"}` + "\n" +
		`{"ts":"2026-10-19T12:01:00Z","finding":"gt-ok","candidate":"none","confidence":0.8,"candidates_offered":["gt-old"],"outcome":"answered"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := printRecentDedupe(&out, path, 5); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(rows) != 3 || !strings.Contains(rows[0], "OUTCOME") ||
		!strings.Contains(rows[1], "gt-bad") || !strings.Contains(rows[1], "ask_failed") ||
		!strings.Contains(rows[2], "gt-ok") || !strings.Contains(rows[2], "answered") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestJevDedupeRecentWithNoLog(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printRecentDedupe(&out, dedupe.LogPath(t.TempDir()), 5); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "no suggestions logged yet" {
		t.Errorf("output = %q, want the empty-log line", got)
	}
}

// A pass with no price configured stops before it reaches Jev or hm, and
// the error names the key to set.
func TestJevDedupePassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer
	err := runJevDedupePass(&out, &errw, town)
	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(dedupe.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused pass wrote a log: %v", statErr)
	}
}

// The manual pass reads patrols.jev_dedupe whether or not the patrol is on.
func TestJevDedupeSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dedupe":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevDedupeSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevDedupeSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevDedupeSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (dedupe.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}
