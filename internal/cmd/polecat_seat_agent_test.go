package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPolecatSeatAgent pins where a seat's agent comes from: its intent record,
// the one source for a seat's facts (gt-kztn5c). A record that names the agent
// answers it whatever the session environment says; a seat with no record, or
// with a record that cannot be read, answers empty, which both the list and the
// dashboard's dash already display as an unknown agent.
func TestPolecatSeatAgent(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	recordDir := filepath.Join(townRoot, ".runtime", "agents", "gastown")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatalf("lay out the intent records: %v", err)
	}
	writes := map[string]string{
		"polecat.named.json":     `{"agent":"sbx-haiku"}`,
		"polecat.malformed.json": `{"agent":`,
	}
	for name, content := range writes {
		if err := os.WriteFile(filepath.Join(recordDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	tests := []struct {
		name    string
		polecat string
		want    string
	}{
		{name: "the record names the agent", polecat: "named", want: "sbx-haiku"},
		{name: "no record names no agent", polecat: "absent", want: ""},
		{name: "an unreadable record names no agent", polecat: "malformed", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := polecatSeatAgent(townRoot, "gastown", tt.polecat); got != tt.want {
				t.Errorf("agent = %q, want %q", got, tt.want)
			}
		})
	}
}
