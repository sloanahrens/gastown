package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/targets"
)

// The manual run reads patrols.jev_targets whether or not the battery is on.
func TestJevTargetsSettingsReadTheDaemonConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_targets":{"enabled":false,"max_usd_per_day":0.5,"usd_per_billion_tokens":4}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := jevTargetsSettings(town)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxUSDPerDay != 0.5 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want a 0.5 cap at 4 USD per billion tokens", got)
	}
}

func TestJevTargetsSettingsWithNoPatrolConfig(t *testing.T) {
	t.Parallel()
	got, err := jevTargetsSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != (targets.Settings{}) {
		t.Errorf("settings = %+v, want zero values", got)
	}
}

// A skipped file is named on stderr with its reason, and a run that skipped
// every file it reached exits non-zero while one that asked any file does not.
func TestReportJevTargetsSkippedFiles(t *testing.T) {
	t.Parallel()
	skip := []targets.SkippedFile{{Path: "internal/a/one.go", Reason: "file text unreadable"}}
	tests := []struct {
		name    string
		sum     targets.Summary
		wantErr bool
	}{
		{"all skipped", targets.Summary{Files: 1, Skipped: 1, SkippedFiles: skip}, true},
		{"some asked", targets.Summary{Files: 2, Asked: 1, Skipped: 1, SkippedFiles: skip}, false},
		{"none skipped", targets.Summary{Files: 1, Asked: 1}, false},
		{"empty slice", targets.Summary{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out, errw bytes.Buffer
			err := reportJevTargets(&out, &errw, tt.sum)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, want error %v", err, tt.wantErr)
			}
			named := strings.Contains(errw.String(), "skipped internal/a/one.go: file text unreadable")
			if named != (len(tt.sum.SkippedFiles) > 0) {
				t.Errorf("stderr = %q, skipped file named = %v", errw.String(), named)
			}
		})
	}
}

// A run with no price configured stops before it reaches Jev, and the error
// names the key to set.
func TestJevTargetsPassNeedsAPrice(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out, errw bytes.Buffer
	err := runJevTargetsPass(&out, &errw, town, targets.RunRequest{
		Run:         "gt-run1",
		RankedPath:  filepath.Join(town, "ranked.txt"),
		HistoryPath: filepath.Join(town, "history.txt"),
	})
	if err == nil || !strings.Contains(err.Error(), "price per billion tokens") {
		t.Fatalf("err = %v, want the missing price named", err)
	}
	if _, statErr := os.Stat(targets.LogPath(town)); !os.IsNotExist(statErr) {
		t.Errorf("a refused run wrote a log: %v", statErr)
	}
}
