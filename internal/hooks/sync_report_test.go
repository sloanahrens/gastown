package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncReportRoundTrip(t *testing.T) {
	townRoot := t.TempDir()

	written := &SyncReport{
		Timestamp: time.Now().UTC().Truncate(time.Second),
		Canary: SyncReportCanary{
			Target:       "mayor",
			SettingsPath: filepath.Join(townRoot, "mayor", ".claude", "settings.json"),
			Blocked:      SyncReportShape{Verdict: SyncReportPass, Detail: "blocked ok"},
			Allowed:      SyncReportShape{Verdict: SyncReportPass, Detail: "allowed ok"},
		},
		Roles: map[string]SyncReportRole{
			"mayor": {
				Role: "mayor",
				Path: filepath.Join(townRoot, "mayor", ".claude", "settings.json"),
				Hooks: HooksConfig{
					PreToolUse: []HookEntry{
						{Matcher: "Bash", Hooks: []Hook{{Type: "command", Command: "gt tap guard pr-workflow"}}},
					},
				},
			},
		},
	}

	if err := WriteSyncReport(townRoot, written); err != nil {
		t.Fatalf("WriteSyncReport: %v", err)
	}

	read, err := ReadSyncReport(townRoot)
	if err != nil {
		t.Fatalf("ReadSyncReport: %v", err)
	}

	if !read.Timestamp.Equal(written.Timestamp) {
		t.Errorf("timestamp mismatch: got %v, want %v", read.Timestamp, written.Timestamp)
	}
	if !read.Canary.Passed() {
		t.Error("expected canary to round-trip as passed")
	}
	if read.Canary.Target != "mayor" {
		t.Errorf("canary target = %q, want %q", read.Canary.Target, "mayor")
	}
	role, ok := read.Roles["mayor"]
	if !ok {
		t.Fatal("expected mayor role to round-trip")
	}
	if len(role.Hooks.PreToolUse) != 1 || role.Hooks.PreToolUse[0].Matcher != "Bash" {
		t.Errorf("unexpected rendered hooks: %+v", role.Hooks)
	}
	if role.Hooks.PreToolUse[0].Hooks[0].Command != "gt tap guard pr-workflow" {
		t.Errorf("unexpected rendered command: %+v", role.Hooks.PreToolUse[0].Hooks)
	}
}

func TestReadSyncReportAbsent(t *testing.T) {
	townRoot := t.TempDir()

	_, err := ReadSyncReport(townRoot)
	if err == nil {
		t.Fatal("expected an error when no sync report has ever been written")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected an os.IsNotExist error, got: %v", err)
	}
}

func TestSyncReportCanaryPassed(t *testing.T) {
	tests := []struct {
		name    string
		blocked SyncReportVerdict
		allowed SyncReportVerdict
		want    bool
	}{
		{"both pass", SyncReportPass, SyncReportPass, true},
		{"blocked failed", SyncReportFail, SyncReportPass, false},
		{"allowed failed", SyncReportPass, SyncReportFail, false},
		{"blocked inconclusive", SyncReportInconclusive, SyncReportPass, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := SyncReportCanary{
				Blocked: SyncReportShape{Verdict: tt.blocked},
				Allowed: SyncReportShape{Verdict: tt.allowed},
			}
			if got := c.Passed(); got != tt.want {
				t.Errorf("Passed() = %v, want %v", got, tt.want)
			}
		})
	}
}
