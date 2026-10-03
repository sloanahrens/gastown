package schedulerrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// writeSchedulerrunRigConfig writes body to <rigPath>/config.json, so a caller
// reads a real rig config file through the same strict loader production uses.
func writeSchedulerrunRigConfig(t *testing.T, rigPath, body string) {
	t.Helper()
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rigPath, err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// TestRigBeadsPrefix_ValidConfigUnchanged is the positive half of gt-8xk9k:
// a config.json that decodes still supplies the beads prefix, and nothing is
// reported.
func TestRigBeadsPrefix_ValidConfigUnchanged(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	writeSchedulerrunRigConfig(t, rigPath, `{"type":"rig","version":1,"name":"testrig","beads":{"prefix":"tr"}}`)

	if got := rigBeadsPrefix(townRoot, rigPath, "testrig"); got != "tr" {
		t.Errorf("rigBeadsPrefix() = %q; want %q", got, "tr")
	}
	if rig.RigConfigWarned(rigPath) {
		t.Errorf("RigConfigWarned(%s) = true for a valid config", rigPath)
	}
}

// TestRigBeadsPrefix_UnparseableConfigReports pins gt-8xk9k for the dispatch
// prefix guard: a config.json that no longer decodes is reported once instead
// of reading as a rig that declares no prefix.
func TestRigBeadsPrefix_UnparseableConfigReports(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	writeSchedulerrunRigConfig(t, rigPath, `{"type":"rig","version":1,"name":"testrig","beads":{"prefix":"tr"},"beadss":{}}`)

	if got := rigBeadsPrefix(townRoot, rigPath, "testrig"); got != "" {
		t.Errorf("rigBeadsPrefix() = %q; want the empty fallback", got)
	}
	if !rig.RigConfigWarned(rigPath) {
		t.Errorf("rigBeadsPrefix() answered no prefix without reporting the parse error")
	}
}
