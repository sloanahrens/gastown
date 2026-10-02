package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestSessionInfoJSONOutput(t *testing.T) {
	t.Parallel()
	info := &polecat.SessionInfo{
		Polecat:   "alpha",
		SessionID: "gt-alpha",
		Running:   true,
		RigName:   "gastown",
		Attached:  false,
		Created:   time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
		Windows:   1,
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed["polecat"] != "alpha" {
		t.Errorf("polecat = %v, want alpha", parsed["polecat"])
	}
	if parsed["session_id"] != "gt-alpha" {
		t.Errorf("session_id = %v, want gt-alpha", parsed["session_id"])
	}
	if parsed["running"] != true {
		t.Errorf("running = %v, want true", parsed["running"])
	}
	if parsed["rig_name"] != "gastown" {
		t.Errorf("rig_name = %v, want gastown", parsed["rig_name"])
	}
}

func TestSessionStatusCmdJSONFlagWiring(t *testing.T) {
	t.Parallel()
	// Verify --json flag is registered on the session status command.
	// This catches regressions where flag binding is accidentally removed,
	// which would silently break formulas that depend on --json output.
	f := sessionStatusCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("session status command missing --json flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--json default = %q, want \"false\"", f.DefValue)
	}
}

// TestSessionRestartRequestedByFlagWiring pins the flag an automated restarter
// passes so its restart is attributable in the town log. The witness's
// RestartPolecatSession calls `gt session restart --requested-by witness`; if
// the flag binding is removed, that call fails and restarts stop happening, so
// the wiring is asserted here rather than discovered in production (gt-tcrgb).
func TestSessionRestartRequestedByFlagWiring(t *testing.T) {
	t.Parallel()
	f := sessionRestartCmd.Flags().Lookup("requested-by")
	if f == nil {
		t.Fatal("session restart command missing --requested-by flag")
	}
	if f.DefValue != "" {
		t.Errorf("--requested-by default = %q, want \"\"", f.DefValue)
	}
}

// TestSessionRestartWakeContext pins the distinction that was missing when a
// witness restart of a hooked-but-idle polecat was mistaken for the checkpoint
// dog spawning a session (gt-tcrgb): a restart must not write the bare
// "resumed" line that `gt session start` writes, and when the caller names
// itself the line has to carry it.
func TestSessionRestartWakeContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		requestedBy string
		want        string
	}{
		{"anonymous restart still names itself", "", "restart"},
		{"whitespace-only caller is anonymous", "   ", "restart"},
		{"named caller is recorded", "witness", "restart requested by witness"},
		{"caller is trimmed", "  witness\n", "restart requested by witness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sessionRestartWakeContext(tc.requestedBy)
			if got != tc.want {
				t.Errorf("sessionRestartWakeContext(%q) = %q, want %q", tc.requestedBy, got, tc.want)
			}
			// The restart line must never be confusable with an explicit
			// `gt session start`, which logs the issue alone.
			if got == "" {
				t.Error("restart wake context is empty; the restart would be unattributable")
			}
		})
	}
}

func TestSessionInfoJSONOutputNotRunning(t *testing.T) {
	t.Parallel()
	info := &polecat.SessionInfo{
		Polecat:   "beta",
		SessionID: "gt-beta",
		Running:   false,
		RigName:   "testrig",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed["running"] != false {
		t.Errorf("running = %v, want false", parsed["running"])
	}
}
