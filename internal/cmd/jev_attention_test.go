package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	jevattention "github.com/steveyegge/gastown/internal/jev/attention"
)

// writeAttentionLog writes the given raw lines to the attention log of town.
func writeAttentionLog(t *testing.T, town string, lines ...string) string {
	t.Helper()
	path := jevattention.LogPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const attentionLineAnswered = `{"ts":"2026-10-20T12:00:00Z","key":"red-main:gastown","kind":"red-main","severity":"high","rig":"gastown","bead":"gt-1","sha":"abc","first_seen":"2026-10-20T11:59:00Z","answers":{"needs_human":0.91,"clears_on_its_own":0.2,"routine_action_fixes":0.3,"repeats_open_item":0.4,"urgency":{"score":2,"confidence":0.7}},"question_version":1,"model":"jev-1.13.0"}`

func TestJevAttentionRecentPrintsTheLastLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeAttentionLog(t, town,
		strings.Replace(attentionLineAnswered, "red-main:gastown", "old-item", 1),
		attentionLineAnswered,
	)

	var out bytes.Buffer
	if err := printRecentAttention(&out, path, 1); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "KEY") || !strings.Contains(got, "NEEDS-HUMAN") {
		t.Errorf("no header row in:\n%s", got)
	}
	if strings.Contains(got, "old-item") {
		t.Errorf("printed a line older than the last 1:\n%s", got)
	}
	for _, want := range []string{"red-main:gastown", "0.91", "2 (0.70)", "jev-1.13.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// A failed call logs null answers, which print as dashes, not as zeros.
func TestJevAttentionRecentShowsAFailedCallAsDashes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeAttentionLog(t, town,
		`{"ts":"2026-10-20T12:00:00Z","key":"failed","kind":"red-main","severity":"high","rig":"","bead":"","sha":"","first_seen":"2026-10-20T11:59:00Z","answers":null,"question_version":1,"model":""}`,
	)

	var out bytes.Buffer
	if err := printRecentAttention(&out, path, 5); err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(rows) != 2 {
		t.Fatalf("output has %d rows, want a header and one row:\n%s", len(rows), out.String())
	}
	var dashes int
	for _, f := range strings.Fields(rows[1]) {
		if f == "-" {
			dashes++
		}
	}
	if !strings.Contains(rows[1], "failed") || dashes != 5 || strings.Contains(rows[1], "0.00") {
		t.Errorf("row = %q, want the five answer columns as dashes", rows[1])
	}
}

func TestJevAttentionRecentWithNoLog(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := printRecentAttention(&out, jevattention.LogPath(t.TempDir()), 5); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "no answers logged yet" {
		t.Errorf("output = %q, want the empty-log line", got)
	}
}

// A pass with no price configured stops before it reaches Jev, and the error
// names the key to set.
func TestJevAttentionPassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer
	err := runJevAttentionPass(&out, &errw, town)
	if err == nil || !strings.Contains(err.Error(), "usd_per_billion_tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(jevattention.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused pass wrote a log: %v", statErr)
	}
}

// The manual pass reads patrols.jev_attention whether or not the patrol is on.
func TestJevAttentionSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_attention":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevAttentionSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevAttentionSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevAttentionSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (jevattention.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}
