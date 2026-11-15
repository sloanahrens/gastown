package trackerclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestCostSamplesRefuseWithNoBd: the tracker client runs no bd, so both
// cost-sample methods refuse by name and send no request. A zero Client has no
// transport at all, so a method that reached the tracker would panic rather
// than answer.
func TestCostSamplesRefuseWithNoBd(t *testing.T) {
	t.Parallel()
	c := &Client{}
	err := c.WriteCostSample(context.Background(), beads.Sample{
		Kind: beads.SampleKindClaudePlan,
		At:   time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "runs no bd") || !strings.Contains(err.Error(), "WriteCostSample") {
		t.Fatalf("WriteCostSample = %v; want an error naming the method and that the tracker client runs no bd", err)
	}
	rows, err := c.ReadCostSamples(context.Background(), beads.SampleKindClaudePlan, time.Time{}, 1)
	if err == nil || !strings.Contains(err.Error(), "runs no bd") || !strings.Contains(err.Error(), "ReadCostSamples") {
		t.Fatalf("ReadCostSamples = %v, %v; want an error naming the method and that the tracker client runs no bd", rows, err)
	}
	if rows != nil {
		t.Errorf("ReadCostSamples answered %v beside its error, want no rows", rows)
	}
}
