package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// writeDedupeLog writes one log line per finding ID, oldest first.
func writeDedupeLog(t *testing.T, town string, findings ...string) string {
	t.Helper()
	path := dedupe.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, f := range findings {
		b.WriteString(`{"ts":"` + time.Date(2026, 10, 19, 12, i, 0, 0, time.UTC).Format(time.RFC3339) +
			`","finding":"` + f + `","candidate":"gt-old","confidence":0.91,"candidates_offered":["gt-old","gt-x"]}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJevDedupeRecentPrintsTheLastLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDedupeLog(t, town, "gt-one", "gt-two", "gt-three")

	var out bytes.Buffer
	if err := printRecentDedupe(&out, dedupe.LogPath(town), 2); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "FINDING") || !strings.Contains(got, "CANDIDATE") {
		t.Errorf("no header row in:\n%s", got)
	}
	if strings.Contains(got, "gt-one") {
		t.Errorf("printed a line older than the last 2:\n%s", got)
	}
	for _, want := range []string{"gt-two", "gt-three", "0.91", "gt-old, gt-x"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
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
