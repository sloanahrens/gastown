//go:build integration

package tiersweep

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIntegrationSerialListNamesOnlyDiscoveredPackages: every package serial.txt
// names is one the integration tier's own plan discovers, listed from this
// checkout through the production listing (`git ls-files`), so a listed package
// that is renamed or deleted fails here instead of silently leaving the list
// that keeps it alone (gt-0rtgc). The unit tier may not run git (testpolicy's
// gitfree rule), so the guard runs where the real listing does.
func TestIntegrationSerialListNamesOnlyDiscoveredPackages(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The plan lists and reads relative to the repository the sweep runs in.
	t.Chdir(root)
	found := discoveredPackages(t, gitSource{exec: ProcessExec, read: os.ReadFile})
	for _, pkg := range undiscoveredSerial(SerialPackages(), found) {
		t.Errorf("serial.txt names %s, which the integration plan does not run: no integration-tagged test under it", pkg)
	}
}
