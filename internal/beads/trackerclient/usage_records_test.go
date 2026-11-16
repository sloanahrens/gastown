package trackerclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/usagerecord"
)

// TestUsageRecordsRefuseWithNoBd: the tracker client runs no bd, so both
// usage-record methods refuse by name and send no request. A zero Client has no
// transport at all, so a method that reached the tracker would panic rather
// than answer.
func TestUsageRecordsRefuseWithNoBd(t *testing.T) {
	t.Parallel()
	c := &Client{}
	err := c.WriteUsageRecord(context.Background(), beads.UsageRecordRow{
		BuiltAt: time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC),
		Record:  usagerecord.Record{Bead: "gt-1", Kind: dashboard.UsageKindWork},
	})
	if err == nil || !strings.Contains(err.Error(), "runs no bd") || !strings.Contains(err.Error(), "WriteUsageRecord") {
		t.Fatalf("WriteUsageRecord = %v; want an error naming the method and that the tracker client runs no bd", err)
	}
	rows, err := c.ReadUsageRecords(context.Background(), time.Time{}, 1)
	if err == nil || !strings.Contains(err.Error(), "runs no bd") || !strings.Contains(err.Error(), "ReadUsageRecords") {
		t.Fatalf("ReadUsageRecords = %v, %v; want an error naming the method and that the tracker client runs no bd", rows, err)
	}
	if rows != nil {
		t.Errorf("ReadUsageRecords answered %v beside its error, want no rows", rows)
	}
}
