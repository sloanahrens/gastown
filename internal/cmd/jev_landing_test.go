package cmd

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/landing"
)

var updateJevLandingGolden = flag.Bool("update-jev-landing-golden", false, "rewrite testdata/jev_landing_recent.golden")

const jevLandingGoldenPath = "jev_landing_recent.golden"

// landingLog is four logged landings: a full line, a crew landing with no
// agent, a line from before the model field existed, and one with no answers.
const landingLog = `{"ts":"2026-10-20T19:01:00Z","bead":"gt-old","rig":"gastown","head":"h0","landed_at":"2026-10-20T19:00:00Z","agent":"deepseek-flash","answers":{"risk":{"score":1,"confidence":0.5}},"question_version":1}
{"ts":"2026-10-20T19:16:00Z","bead":"gt-aaaaa","rig":"gastown","head":"h1","landed_at":"2026-10-20T19:15:00Z","agent":"claude-haiku","answers":{"needs_review_now":{"noul":0.83},"touches_town_machinery":{"noul":0.91},"beyond_goal":{"noul":0.07},"tests_cover_change":{"noul":0.66},"risk":{"score":3,"confidence":0.72}},"question_version":1,"model":"jev-1.13.0"}
{"ts":"2026-10-20T19:31:00Z","bead":"gt-bbbbb","rig":"gastown","head":"h2","landed_at":"2026-10-20T19:30:00Z","answers":{"needs_review_now":{"noul":0.1},"touches_town_machinery":{"noul":0},"beyond_goal":{"noul":1},"tests_cover_change":{"noul":0.5},"risk":{"score":0,"confidence":0.99}},"question_version":2,"model":"jev-1.14.0"}
{"ts":"2026-10-20T19:46:00Z","bead":"gt-ccccc","rig":"gastown","head":"h3","landed_at":"2026-10-20T19:45:00Z","agent":"claude-haiku","answers":{},"question_version":2,"model":"jev-1.14.0"}
`

func writeLandingLog(t *testing.T, town, body string) string {
	t.Helper()
	path := landing.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --recent 3 prints a header and the last three lines, oldest first, in a
// fixed layout.
func TestJevLandingRecentGolden(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeLandingLog(t, town, landingLog)

	var out bytes.Buffer
	if err := printRecentLanding(&out, path, 3); err != nil {
		t.Fatal(err)
	}

	golden := filepath.Join("testdata", jevLandingGoldenPath)
	if *updateJevLandingGolden {
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("output differs from %s (rewrite with -update-jev-landing-golden):\n%s\nwant:\n%s", golden, out.String(), want)
	}
	if rows := strings.Split(strings.TrimSpace(out.String()), "\n"); len(rows) != 4 {
		t.Errorf("printed %d rows, want a header and three lines", len(rows))
	}
}

func TestJevLandingRecentWithNoLog(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printRecentLanding(&out, landing.LogPath(t.TempDir()), 5); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "no landings logged yet" {
		t.Errorf("output = %q, want the empty-log line", got)
	}
}

// A pass with no price configured stops before it reaches Jev, and the error
// names the key to set.
func TestJevLandingPassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer
	err := runJevLandingPass(&out, &errw, town)
	if err == nil || !strings.Contains(err.Error(), "price per billion tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(landing.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused pass wrote a log: %v", statErr)
	}
}

// The manual pass reads patrols.jev_landing whether or not the patrol is on.
func TestJevLandingSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_landing":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevLandingSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevLandingSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevLandingSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (landing.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}
