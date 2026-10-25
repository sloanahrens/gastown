package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/metrics"
)

// A run that finds the lock held by another run records the hour as skipped and
// writes nothing: no snapshot is taken and no analysis runs. The lock's own
// contention across processes is pinned in internal/metrics; here the acquire is
// one that reports the lock held.
func TestMetricsRunSkipsAnHourWhenAnotherRunHoldsTheLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	held := func(string) (*metrics.TownLock, error) { return nil, metrics.ErrLockHeld }

	var out bytes.Buffer
	if err := runMetricsHour(context.Background(), &out, town, held); err != nil {
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
