package daemon

import (
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestNewRigLandingWorkerKeepsRecordsInTheTownLandings: the rig's Lander keeps
// the landing records a bead write could not make in the town's landings
// directory (gt-r2rmz6).
func TestNewRigLandingWorkerKeepsRecordsInTheTownLandings(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", filepath.Join(t.TempDir(), "promote-gastown.key"))
	writeRigsJSON(t, townRoot, []string{"gastown"})
	w, err := newForgejoPromoteDaemon(t, townRoot, "").newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	l, _ := w.Lander.(*land.Lander)
	want := filepath.Join(townRoot, ".runtime", "landings", "records.jsonl")
	if l == nil || l.Records == nil || l.Records.Path != want {
		t.Fatalf("Lander.Records = %+v; want the record queue at %s", l, want)
	}
}
