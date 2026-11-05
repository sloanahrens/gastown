package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSlingRefusesWhenTheOperatorSettingsCannotBeRead: the operator guard reads
// the identities from the town settings (operator.names), and a settings file
// that does not parse refuses the sling before it writes anything, since
// taking a bead the setting may reserve is what the guard prevents (gt-ywuip).
func TestSlingRefusesWhenTheOperatorSettingsCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{Description: "TestIntegration fails in internal/cmd."})
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run.townRoot = townRoot
	err := h.run.run(context.Background(), nil, []string{slingBead, "crew"})
	if err == nil {
		t.Fatal("an unreadable settings file must refuse the sling")
	}
	if !strings.Contains(err.Error(), "operator identities") {
		t.Errorf("the refusal does not name the setting read: %v", err)
	}
}
