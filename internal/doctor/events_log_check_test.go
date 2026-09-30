package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEventsLogCheck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	check := NewEventsLogCheck()

	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Errorf("missing log: %v %s", r.Status, r.Message)
	}

	path := filepath.Join(town, ".events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Errorf("small log: %v %s", r.Status, r.Message)
	}

	// A sparse file past the threshold: size without writing 64 MB.
	if err := os.Truncate(path, eventsLogWarnBytes+1); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusWarning {
		t.Errorf("oversized log: %v %s", r.Status, r.Message)
	}
}
