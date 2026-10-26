package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/metrics"
)

// A run that finds the lock held by another run records the hour as skipped and
// writes nothing: no snapshot is taken and no analysis runs. The lock is held
// here by a second open file description in this process, which is what a
// second run is to flock.
func TestMetricsRunSkipsAnHourWhenAnotherRunHoldsTheLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	lockPath := filepath.Join(town, ".runtime", "town-metrics.lock")
	held, err := metrics.AcquireTownLock(lockPath)
	if err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	defer held.Release()

	var out bytes.Buffer
	if err := runMetricsHour(context.Background(), &out, town); err != nil {
		t.Fatalf("runMetricsHour with the lock held: %v", err)
	}
	got := out.String()
	if !strings.HasSuffix(got, "[plugin-result skipped]\n") {
		t.Errorf("output = %q, want it to end with the skipped marker", got)
	}
	if _, err := os.Stat(metrics.SnapshotPath(town)); !os.IsNotExist(err) {
		t.Errorf("a snapshot was appended while the lock was held (stat err = %v)", err)
	}
}
