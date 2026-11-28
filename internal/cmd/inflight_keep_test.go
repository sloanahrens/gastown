package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/townflight"
)

// TestInFlightKeep: keepInFlight leaves its entry locked after it returns, and
// refuses in a draining town.
func TestInFlightKeep(t *testing.T) {
	t.Parallel()
	town := daemonTown(t)
	if err := keepInFlight(town, "gt status"); err != nil {
		t.Fatalf("keepInFlight: %v", err)
	}
	entries, err := filepath.Glob(filepath.Join(town, "daemon", "inflight", "*.cmd"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("inflight entries = %v, %v; want one", entries, err)
	}
	if ok, err := flock.New(entries[0]).TryLock(); err != nil || ok {
		t.Fatalf("TryLock after return = %v, %v; want the entry still locked", ok, err)
	}
	if err := townflight.StartDrain(town); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	if err := keepInFlight(town, "gt status"); !errors.Is(err, townflight.ErrDraining) {
		t.Fatalf("keepInFlight while draining = %v, want ErrDraining", err)
	}
}

// daemonTown returns a town root holding a daemon.lock, which Register reads
// before it lists an entry.
func daemonTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatalf("creating daemon dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(town, "daemon", "daemon.lock"), nil, 0o600); err != nil {
		t.Fatalf("writing daemon.lock: %v", err)
	}
	return town
}
