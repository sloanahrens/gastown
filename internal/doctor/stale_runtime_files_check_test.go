package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// writeStaleRuntimeRigs writes mayor/rigs.json with one entry per rig,
// mapping rig name to its beads prefix.
func writeStaleRuntimeRigs(t *testing.T, townRoot string, rigs map[string]string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("MkdirAll mayor: %v", err)
	}
	rigsConfig := config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}}
	for rigName, prefix := range rigs {
		rigsConfig.Rigs[rigName] = config.RigEntry{
			GitURL:      "https://github.com/test/" + rigName,
			AddedAt:     time.Now(),
			BeadsConfig: &config.BeadsConfig{Repo: "local", Prefix: prefix},
		}
	}
	data, err := json.Marshal(rigsConfig)
	if err != nil {
		t.Fatalf("Marshal rigs.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), data, 0644); err != nil {
		t.Fatalf("WriteFile rigs.json: %v", err)
	}
}

// writeStaleRuntimePID creates .runtime/pids/<sessionID>.pid.
func writeStaleRuntimePID(t *testing.T, townRoot, sessionID string) {
	t.Helper()
	pidsDir := filepath.Join(townRoot, ".runtime", "pids")
	if err := os.MkdirAll(pidsDir, 0755); err != nil {
		t.Fatalf("MkdirAll pids: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pidsDir, sessionID+".pid"), []byte("12345"), 0644); err != nil {
		t.Fatalf("WriteFile %s.pid: %v", sessionID, err)
	}
}

// TestStaleRuntimeFiles_HyphenatedRigPrefixIsKept guards gt-a4po7: a rig prefix
// may contain a hyphen, so reading my-app-nux.pid as prefix "my" reported a live
// PID file stale and Fix deleted it.
func TestStaleRuntimeFiles_HyphenatedRigPrefixIsKept(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeStaleRuntimeRigs(t, townRoot, map[string]string{"my-app": "my-app-"})
	writeStaleRuntimePID(t, townRoot, "my-app-nux")
	writeStaleRuntimePID(t, townRoot, "my-other-nux")

	check := NewStaleRuntimeFilesCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	details := strings.Join(result.Details, "\n")
	if strings.Contains(details, "my-app-nux.pid") {
		t.Errorf("live PID file for hyphenated rig prefix reported stale: %v", result.Details)
	}
	if !strings.Contains(details, "my-other-nux.pid") {
		t.Errorf("PID file for unregistered rig not reported stale: %v", result.Details)
	}
	if result.Status != StatusWarning {
		t.Errorf("Status = %v, want StatusWarning", result.Status)
	}
}

// TestStaleRuntimeFiles_RegisteredRigPrefixIsKept covers the unhyphenated case
// and the town-level (hq, gt) exemption alongside it.
func TestStaleRuntimeFiles_RegisteredRigPrefixIsKept(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeStaleRuntimeRigs(t, townRoot, map[string]string{"sw": "sw-", "my-app": "my-app-"})
	writeStaleRuntimePID(t, townRoot, "sw-crew-max")
	writeStaleRuntimePID(t, townRoot, "my-app-crew-max")
	writeStaleRuntimePID(t, townRoot, "hq-overseer")
	writeStaleRuntimePID(t, townRoot, "gt-daemon")

	check := NewStaleRuntimeFilesCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want StatusOK: %v", result.Status, result.Details)
	}
	if len(check.stalePIDFiles) != 0 {
		t.Errorf("stalePIDFiles = %v, want none", check.stalePIDFiles)
	}
}

func TestMatchKnownPrefix(t *testing.T) {
	t.Parallel()
	known := map[string]bool{"sw": true, "my-app": true, "hq": true}
	tests := []struct {
		filename   string
		wantPrefix string
		wantKnown  bool
	}{
		{"sw-nux.pid", "sw", true},
		{"sw-crew-max.pid", "sw", true},
		{"my-app-nux.pid", "my-app", true},
		{"my-app-crew-max.pid", "my-app", true},
		{"hq-overseer.pid", "hq", true},
		// No known prefix owns this file; the first-segment fallback is
		// what the caller reports as stale.
		{"my-other-nux.pid", "my", false},
		{"unknown-nux.pid", "unknown", false},
	}
	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			t.Parallel()
			prefix, ok := matchKnownPrefix(tt.filename, known)
			if prefix != tt.wantPrefix || ok != tt.wantKnown {
				t.Errorf("matchKnownPrefix(%q) = (%q, %v), want (%q, %v)",
					tt.filename, prefix, ok, tt.wantPrefix, tt.wantKnown)
			}
		})
	}
}

// TestMatchKnownPrefix_LongestWins covers nested prefixes: my-app owns
// my-app-nux.pid even when my is also a registered prefix.
func TestMatchKnownPrefix_LongestWins(t *testing.T) {
	t.Parallel()
	known := map[string]bool{"my": true, "my-app": true}
	prefix, ok := matchKnownPrefix("my-app-nux.pid", known)
	if !ok || prefix != "my-app" {
		t.Errorf("matchKnownPrefix = (%q, %v), want (\"my-app\", true)", prefix, ok)
	}
}
