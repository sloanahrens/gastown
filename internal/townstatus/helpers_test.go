package townstatus

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/slot"
)

// testRegistry is the fixed rig prefix table the moved status tests name
// sessions and bead ids through.
func testRegistry() *session.PrefixRegistry {
	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")
	registry.Register("do", "coder_dotfiles")
	registry.Register("mr", "myrig")
	return registry
}

// writeTestRoutes writes routes.jsonl so GetPrefixForRig finds the fixtures'
// rig prefixes.
func writeTestRoutes(t *testing.T, townRoot string, routes []beads.Route) {
	t.Helper()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("create beads dir: %v", err)
	}
	if err := beads.WriteRoutes(beadsDir, routes); err != nil {
		t.Fatalf("write routes: %v", err)
	}
}

var stubNoContainersOnce sync.Once

// stubNoContainers keeps the gate-slot reads off docker: the pool read under
// test is the flock one, and a real `docker ps` would be a unit-tier
// subprocess.
func stubNoContainers(t *testing.T) {
	t.Helper()
	stubNoContainersOnce.Do(func() {
		slot.SetContainerListerForTest(func() ([]string, error) { return nil, nil })
	})
}
