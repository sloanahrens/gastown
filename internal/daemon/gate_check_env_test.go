package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The gate check of a rig runs against that rig's own database, and any other
// database is routed from the town root (gt-p85pw.8). The env builder is read
// directly: the check's bd call itself needs a bd binary this tier does not
// start.
func TestGateCheckEnvPinsARigToItsDatabase(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigDir := filepath.Join(town, "rigA")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"ra-","path":"rigA"}` + "\n"
	if err := os.MkdirAll(filepath.Join(town, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, ".beads", beads.RoutesFileName), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}

	want := "BEADS_DIR=" + beads.ResolveBeadsDir(rigDir)
	if !envHas(gateCheckEnv(town, "rigA"), want) {
		t.Errorf("rig gate check env lacks %s", want)
	}
}

func TestGateCheckEnvRoutesAnUnknownRigFromTheTownRoot(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rigDir := filepath.Join(town, "rigA")
	pinned := "BEADS_DIR=" + beads.ResolveBeadsDir(rigDir)
	for _, rig := range []string{"", "nosuchrig"} {
		if envHas(gateCheckEnv(town, rig), pinned) {
			t.Errorf("gate check env for rig %q pins %s, want the town route", rig, pinned)
		}
	}
}

func envHas(env []string, entry string) bool {
	for _, kv := range env {
		if strings.TrimSpace(kv) == entry {
			return true
		}
	}
	return false
}
