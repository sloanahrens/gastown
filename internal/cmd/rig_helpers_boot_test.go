package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// writeBootHoldFile writes the daemon's boot-health file under town, as the
// daemon's poller does while infrastructure is not answering.
func writeBootHoldFile(t *testing.T, town, reason string) {
	t.Helper()
	path := dispatch.BootHealthPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(reason+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRingHoldNowBootGate: with the boot-health file present, a sling into a
// rig refuses and the refusal names the reason and the file. With the file
// absent the Ring's own verdict is what ringHoldNow returns. The sling check is
// slingBlocked, the production call that passes ringHoldNow on.
func TestRingHoldNowBootGate(t *testing.T) {
	t.Parallel()
	noEstop := func(string, string) (bool, error) { return false, nil }
	notParked := func(string, string) (bool, string) { return false, "" }

	t.Run("file present refuses with the reason and the file", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeBootHoldFile(t, town, "waiting for infrastructure")
		if got := ringHoldNow(town); got != dispatch.BootHold(town) {
			t.Fatalf("ringHoldNow with the boot file = %q, want the boot hold %q", got, dispatch.BootHold(town))
		}
		err := slingBlocked(town, "gastown", ringHoldNow, noEstop, notParked)
		if err == nil {
			t.Fatal("slingBlocked with the boot file present = nil, want the sling refused")
		}
		for _, want := range []string{
			"waiting for infrastructure",
			dispatch.BootHealthPath(town),
			"remove the file by hand",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not contain %q", err, want)
			}
		}
	})

	t.Run("file absent keeps the Ring's verdict", func(t *testing.T) {
		t.Parallel()
		// A town with no ring section holds nothing, whatever the host
		// environment holds, so the verdict does not depend on the key.
		town := t.TempDir()
		ringTestTownFile(t, town)
		if got := ringHoldNow(town); got != "" {
			t.Fatalf("ringHoldNow with no boot file and no ring = %q, want \"\"", got)
		}
		if err := slingBlocked(town, "gastown", ringHoldNow, noEstop, notParked); err != nil {
			t.Fatalf("slingBlocked with no boot file and no ring = %v, want the sling to proceed", err)
		}
	})
}
