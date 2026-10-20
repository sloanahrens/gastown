package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// submissionLog is two logged submissions: one full line and one whose
// answers are empty.
const submissionLog = `{"ts":"2026-10-20T19:16:00Z","bead":"gt-aaaaa","branch":"polecat/opal/gt-aaaaa","head":"h1","target":"main","submitted":"2026-10-20T19:15:00Z","agent":"claude-haiku","answers":{"tests_cover_change":{"noul":0.83},"beyond_goal":{"noul":0.07},"commits_match_change":{"noul":0.91},"touches_town_machinery":{"noul":0.12},"will_pass_review":{"noul":0.66},"risk":{"score":1,"confidence":0.72}},"question_version":1,"model":"jev-1.13.0"}
{"ts":"2026-10-20T19:31:00Z","bead":"gt-bbbbb","branch":"polecat/opal/gt-bbbbb","head":"h2","target":"main","submitted":"2026-10-20T19:30:00Z","agent":"","answers":{},"question_version":1,"model":"jev-1.13.0"}
`

func writeSubmissionLog(t *testing.T, town, body string) string {
	t.Helper()
	path := submission.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --recent prints a header and the last lines, oldest first, one row each.
func TestJevSubmissionRecentPrintsLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeSubmissionLog(t, town, submissionLog)

	var out bytes.Buffer
	if err := printRecentSubmission(&out, path, 2); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(rows) != 3 {
		t.Fatalf("printed %d rows, want a header and two lines:\n%s", len(rows), out.String())
	}
	if !strings.Contains(rows[0], "BEAD") || !strings.Contains(rows[0], "RISK") {
		t.Errorf("header = %q", rows[0])
	}
	if !strings.Contains(rows[1], "gt-aaaaa") || !strings.Contains(rows[1], "0.66") || !strings.Contains(rows[1], "1 (0.72)") {
		t.Errorf("first row = %q, want the submission's answers", rows[1])
	}
	if !strings.Contains(rows[2], "gt-bbbbb") {
		t.Errorf("second row = %q", rows[2])
	}
}

func TestJevSubmissionRecentWithNoLog(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printRecentSubmission(&out, submission.LogPath(t.TempDir()), 5); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "no submissions logged yet" {
		t.Errorf("output = %q, want the empty-log line", got)
	}
}

// A pass with no price configured stops before it reaches Jev, and the error
// names the key to set.
func TestJevSubmissionPassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer
	err := runJevSubmissionPass(&out, &errw, town)
	if err == nil || !strings.Contains(err.Error(), "price per billion tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(submission.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused pass wrote a log: %v", statErr)
	}
}

// The manual pass reads patrols.jev_submission whether or not the patrol is
// on.
func TestJevSubmissionSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_submission":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevSubmissionSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevSubmissionSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevSubmissionSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (submission.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}
