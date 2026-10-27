package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// overseerTestName is the human identity the landing worker fixtures record:
// the name a submission must not carry (gt-2ge3z).
const overseerTestName = "Test Operator"

// writeOverseerIdentity writes the town's overseer identity document, which
// the landing worker reads the human's identity from.
func writeOverseerIdentity(t *testing.T, townRoot, name string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"type":"overseer","version":1,"name":"` + name + `","source":"test"}`
	if err := os.WriteFile(filepath.Join(mayorDir, "overseer.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNewRigLandingWorkerCarriesTheHumanIdentity: the worker refuses a
// submission under the town's overseer identity, so construction hands it that
// name (gt-2ge3z).
func TestNewRigLandingWorkerCarriesTheHumanIdentity(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-gastown.key")
	promoteRigConfig(t, townRoot, "gastown", keyFile)
	writeRigsJSON(t, townRoot, []string{"gastown"})

	d := newForgejoPromoteDaemon(t, townRoot, "gastown")
	w, err := d.newRigLandingWorker("gastown")
	if err != nil {
		t.Fatalf("newRigLandingWorker: %v", err)
	}
	if w.HumanActor != overseerTestName {
		t.Errorf("HumanActor = %q, want the town's overseer %q", w.HumanActor, overseerTestName)
	}
}

// TestNewRigLandingWorkerRefusesATownWithNoOverseerIdentity: without the
// human's name no submission can be checked, so the rig gets no worker and
// nothing lands until the identity is readable (gt-2ge3z).
func TestNewRigLandingWorkerRefusesATownWithNoOverseerIdentity(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "promote-gastown.key")
	promoteRigConfig(t, townRoot, "gastown", keyFile)
	writeRigsJSON(t, townRoot, []string{"gastown"})

	d := newForgejoPromoteDaemon(t, townRoot, "gastown")
	if err := os.Remove(filepath.Join(townRoot, "mayor", "overseer.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.newRigLandingWorker("gastown"); err == nil {
		t.Fatal("a town with no overseer identity built a landing worker; it could not check a submission's actor")
	}
}
