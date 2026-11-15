package beadsfake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

var sampleAt = time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)

// TestFakeCostSamplesContract runs the cost-sample suite against the fake, so
// it is runnable alone: go test ./internal/beads/beadsfake -run CostSamples.
func TestFakeCostSamplesContract(t *testing.T) {
	t.Parallel()
	RunCostSamplesContract(t, func(t *testing.T) beads.Client { return New() })
}

// TestFakeCostSamplesCopyTheRow: changing the sample a caller wrote after the
// write, or a row a read returned, does not change what the store holds.
func TestFakeCostSamplesCopyTheRow(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	s := beads.Sample{Kind: beads.SampleKindClaudePlan, At: sampleAt, FiveHourPct: ptr(41.0)}
	mustDo(t, "WriteCostSample", f.WriteCostSample(ctx, s))
	*s.FiveHourPct = 99.0
	rows, err := f.ReadCostSamples(ctx, beads.SampleKindClaudePlan, time.Time{}, 0)
	if err != nil || len(rows) != 1 || rows[0].FiveHourPct == nil || *rows[0].FiveHourPct != 41.0 {
		t.Fatalf("ReadCostSamples = %v, %v; want the written row at 41, not the caller's later edit", rows, err)
	}
	*rows[0].FiveHourPct = 12.0
	again, err := f.ReadCostSamples(ctx, beads.SampleKindClaudePlan, time.Time{}, 0)
	if err != nil || len(again) != 1 || again[0].FiveHourPct == nil || *again[0].FiveHourPct != 41.0 {
		t.Fatalf("ReadCostSamples after editing a returned row = %v, %v; want the stored row still at 41", again, err)
	}
}

// TestFakeCostSamplesReportScriptedFailures: both sample commands honor
// FailWith, so a caller test can script a store that did not answer.
func TestFakeCostSamplesReportScriptedFailures(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	boom := errors.New("boom")
	f.FailWith("sample add", boom)
	if err := f.WriteCostSample(ctx, beads.Sample{Kind: beads.SampleKindClaudePlan, At: sampleAt}); !errors.Is(err, boom) {
		t.Errorf("WriteCostSample with FailWith(sample add) = %v, want boom", err)
	}
	f.FailWith("sample list", boom)
	if _, err := f.ReadCostSamples(ctx, beads.SampleKindClaudePlan, time.Time{}, 0); !errors.Is(err, boom) {
		t.Errorf("ReadCostSamples with FailWith(sample list) = %v, want boom", err)
	}
}

// TestFakeCostSamplesStoreTheTimeAsBdDoes: as bd sample add writes it, At is
// stored in UTC at second precision, and two rows at the same At list the
// later write first.
func TestFakeCostSamplesStoreTheTimeAsBdDoes(t *testing.T) {
	t.Parallel()
	f := New()
	ctx := context.Background()
	at := time.Date(2026, 11, 15, 22, 5, 30, 900_000_000, time.FixedZone("CST", -6*3600))
	mustDo(t, "WriteCostSample(first)", f.WriteCostSample(ctx, beads.Sample{Kind: beads.SampleKindClaudePlan, At: at, Note: "first"}))
	mustDo(t, "WriteCostSample(second)", f.WriteCostSample(ctx, beads.Sample{Kind: beads.SampleKindClaudePlan, At: at, Note: "second"}))
	rows, err := f.ReadCostSamples(ctx, beads.SampleKindClaudePlan, time.Time{}, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("ReadCostSamples = %v, %v; want both rows", rows, err)
	}
	want := at.UTC().Truncate(time.Second)
	for _, r := range rows {
		if !r.At.Equal(want) {
			t.Errorf("At = %v, want %v in UTC at second precision", r.At, want)
		}
	}
	if rows[0].Note != "second" || rows[1].Note != "first" {
		t.Errorf("rows = %q, %q; want the later write first at equal At", rows[0].Note, rows[1].Note)
	}
}
