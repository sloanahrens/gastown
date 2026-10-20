package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/dispatch"
)

// writeDispatchLog writes one log line per bead ID, oldest first.
func writeDispatchLog(t *testing.T, town string, ids ...string) string {
	t.Helper()
	path := dispatch.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i, id := range ids {
		ts := time.Date(2026, 10, 20, 12, i, 0, 0, time.UTC).Format(time.RFC3339)
		b.WriteString(`{"ts":"` + ts + `","bead":"` + id + `","attached_at":"` + ts + `","agent":"claude-sonnet",` +
			`"answers":{"lands_first_try":0.91,"fits_twenty_minutes":0.5,"acceptance_tickable":0.7,` +
			`"needs_strong_model":0.2,"touches_daemon":0.1,"size":{"score":1,"confidence":0.6}},` +
			`"question_version":1,"model":"jev-1.13.0"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJevDispatchRecentPrintsTheLastLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDispatchLog(t, town, "gt-one", "gt-two", "gt-three")

	var out bytes.Buffer
	if err := printRecentDispatch(&out, dispatch.LogPath(town), 2); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "BEAD") || !strings.Contains(got, "LANDS") {
		t.Errorf("no header row in:\n%s", got)
	}
	if strings.Contains(got, "gt-one") {
		t.Errorf("printed a line older than the last 2:\n%s", got)
	}
	for _, want := range []string{"gt-two", "gt-three", "claude-sonnet", "0.91", "jev-1.13.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestJevDispatchRecentWithNoLog(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printRecentDispatch(&out, dispatch.LogPath(t.TempDir()), 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no predictions logged yet") {
		t.Errorf("output = %q, want the empty-log message", out.String())
	}
}

func TestJevDispatchPassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer

	err := runJevDispatchPass(&out, &errw, town)

	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(dispatch.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused pass wrote a log: %v", statErr)
	}
}

// The manual pass reads patrols.jev_dispatch whether or not the patrol is on.
func TestJevDispatchSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dispatch":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevDispatchSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevDispatchSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevDispatchSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (dispatch.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}
