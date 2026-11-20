package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// jobOverdueNow is the tick every collection in these tests judges against, so
// no test reads the wall clock.
var jobOverdueNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// jobOverdueItems runs the job-overdue collector once over readings.
func jobOverdueItems(t *testing.T, f *attentionFixture, readings []JobReading) []attention.Item {
	t.Helper()
	f.src.jobList = func(time.Time) ([]JobReading, error) { return readings, nil }
	c := collectorFor(t, f.src, attention.KindJobOverdue)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	return items
}

// An overdue job raises one item keyed by its name and naming its last run age
// and its cadence; the same job on the next tick, run again, raises none.
func TestJobOverdueAttention(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)

	overdue := JobReading{Name: "dolt_gc", State: "overdue", LastRun: jobOverdueNow.Add(-3 * time.Hour), Cadence: time.Hour}
	items := jobOverdueItems(t, f, []JobReading{
		overdue,
		{Name: "dolt_backup", State: "ok", LastRun: jobOverdueNow.Add(-time.Hour), Cadence: 24 * time.Hour},
	})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the overdue job only", items)
	}
	it := items[0]
	if it.Key != "job-overdue:dolt_gc" || it.Kind != attention.KindJobOverdue || it.Severity != attention.SeverityLow {
		t.Errorf("item = %+v, want a low job-overdue item keyed job-overdue:dolt_gc", it)
	}
	for _, want := range []string{"dolt_gc", "3h0m", "1h0m0s"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("summary %q does not name %q", it.Summary, want)
		}
	}

	ranAgain := overdue
	ranAgain.State = "ok"
	ranAgain.LastRun = jobOverdueNow.Add(-time.Minute)
	if items := jobOverdueItems(t, f, []JobReading{ranAgain}); len(items) != 0 {
		t.Errorf("a job that ran again still raises %+v, want the item cleared", items)
	}
}

// With no list source wired the collector raises nothing and no error; a source
// that fails gives the error back, so the previous items stay (collectAttention).
func TestJobOverdueAttentionSource(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, jobOverdueNow)
	c := collectorFor(t, f.src, attention.KindJobOverdue)
	if items, err := c.collect(context.Background()); err != nil || len(items) != 0 {
		t.Errorf("no source: items %+v, err %v, want neither", items, err)
	}
	boom := errors.New("ledger unreadable")
	f.src.jobList = func(time.Time) ([]JobReading, error) { return nil, boom }
	if items, err := c.collect(context.Background()); !errors.Is(err, boom) || len(items) != 0 {
		t.Errorf("failing source: items %+v, err %v, want the source's error and no item", items, err)
	}
}
