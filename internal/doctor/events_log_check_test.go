package doctor

import (
	"os"
	"path/filepath"
	"strings"
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

// TestEventsLogCheckHintNamesTheDaemonSection: a migrated town has no
// mayor/daemon.json, so the hint for an oversized log names the daemon section.
func TestEventsLogCheckHintNamesTheDaemonSection(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeDoctorTownFile(t, town, "mayor/town.json", `{"registry":{}}`)
	writeDoctorTownFile(t, town, "settings/config.json", `{"daemon":{"type":"daemon-patrol-config","version":1}}`)
	path := filepath.Join(town, ".events.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, eventsLogWarnBytes+1); err != nil {
		t.Fatal(err)
	}

	r := NewEventsLogCheck().Run(&CheckContext{TownRoot: town})

	if r.Status != StatusWarning {
		t.Fatalf("oversized log: %v %s", r.Status, r.Message)
	}
	if want := `settings/config.json (section "daemon")`; !strings.Contains(r.FixHint, want) {
		t.Errorf("FixHint %q must name %s", r.FixHint, want)
	}
	if strings.Contains(r.FixHint, "daemon.json") {
		t.Errorf("FixHint %q names the retired daemon.json", r.FixHint)
	}
}
