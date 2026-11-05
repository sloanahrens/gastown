package sling

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRefusesWhenTheOperatorSettingsCannotBeRead: Run reads the operator
// identities before any guard, and a town settings file that does not parse
// refuses the dispatch rather than take a bead the setting may reserve
// (gt-ywuip).
func TestRunRefusesWhenTheOperatorSettingsCannotBeRead(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeSlingDeps{}
	opts := holdTestOptions()
	opts.TownRoot = townRoot
	result, err := Run(context.Background(), f.deps(), opts)
	if err == nil {
		t.Fatalf("the dispatch ran with an unreadable settings file (result %+v)", result)
	}
	if !strings.Contains(err.Error(), "operator identities") {
		t.Errorf("the refusal does not name the setting read: %v", err)
	}
	if f.spawns != 0 || f.hooks != 0 {
		t.Errorf("the refused dispatch wrote work: spawns=%d hooks=%d", f.spawns, f.hooks)
	}
}
