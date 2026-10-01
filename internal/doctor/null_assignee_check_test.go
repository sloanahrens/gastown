package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The check reads registered rigs only. The Dolt server also hosts databases
// that are no rig: the operator's own tracker (claude) keeps human-owned epics
// in_progress with no assignee, and Fix would reopen them.
func TestNullAssigneeCheck_ScansRegisteredRigsOnly(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, d := range []string{"mayor", "gastown", "claude"} {
		if err := os.MkdirAll(filepath.Join(townRoot, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), `{"rigs":{"gastown":{}}}`)

	bd := newFakeBD()
	stuck := csvAnswer([]string{"id", "title", "updated_at"}, []string{"x-1", "step", "2026-09-30"})
	bd.db(filepath.Join(townRoot, "gastown")).OnSQL(stuck)
	bd.db(filepath.Join(townRoot, "claude")).OnSQL(stuck)

	result := NewNullAssigneeCheck().Run(bd.ctx(townRoot))

	if result.Status != StatusWarning || len(result.Details) != 1 ||
		!strings.HasPrefix(result.Details[0], "[gastown] x-1") {
		t.Errorf("want one gastown finding, got %v %q", result.Status, result.Details)
	}
	for _, o := range bd.opened() {
		if o.dir == filepath.Join(townRoot, "claude") {
			t.Errorf("check queried the unregistered claude database")
		}
	}
}
