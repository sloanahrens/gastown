package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// refusalsTick is the time the refusals cases tick at.
var refusalsTick = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

// refusalsTickWith ticks the refusals collector at at, with a gateway whose stats
// read is read.
func refusalsTickWith(t *testing.T, f *attentionFixture, at time.Time, read func(context.Context) (bdgateway.GatewayStats, error)) []attention.Item {
	t.Helper()
	f.src.now = at
	f.src.gatewayStats = read
	return f.collect(t, f.src.collectGatewayRefusals)
}

// refusalsTickAt ticks the refusals collector at at, with a gateway whose stats
// read answers counts as its refusals.
func refusalsTickAt(t *testing.T, f *attentionFixture, at time.Time, counts map[string]uint64) []attention.Item {
	t.Helper()
	return refusalsTickWith(t, f, at, func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{Refusals: counts, RefusalsPresent: true}, nil
	})
}

// TestGatewayRefusalsAttentionRaises: growth of more than five refused calls in
// the hour raises one item naming each reason and its count, growth of five
// raises none, and once the rise has aged out of the hour a quiet tick clears it.
func TestGatewayRefusalsAttentionRaises(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.src.refusalWindow = &RefusalWindow{}
	refusalsTickAt(t, f, refusalsTick, map[string]uint64{"cap": 0, "stale": 0})

	items := refusalsTickAt(t, f, refusalsTick.Add(10*time.Minute), map[string]uint64{"cap": 3, "stale": 2})
	if len(items) != 0 {
		t.Fatalf("growth of 5 raised %d items, want none", len(items))
	}

	items = refusalsTickAt(t, f, refusalsTick.Add(20*time.Minute), map[string]uint64{"cap": 4, "stale": 2})
	if len(items) != 1 {
		t.Fatalf("growth of 6 raised %d items, want one", len(items))
	}
	if items[0].Kind != attention.KindBdGatewayRefusals {
		t.Errorf("kind = %s, want %s", items[0].Kind, attention.KindBdGatewayRefusals)
	}
	for _, want := range []string{"cap 4", "stale 2"} {
		if !strings.Contains(items[0].Summary, want) {
			t.Errorf("summary %q does not name %q", items[0].Summary, want)
		}
	}

	// The rise was recorded at twenty minutes, so at eighty minutes it has aged
	// out of the hour and only the latest sample is left: no growth.
	items = refusalsTickAt(t, f, refusalsTick.Add(time.Hour+20*time.Minute), map[string]uint64{"cap": 4, "stale": 2})
	if len(items) != 0 {
		t.Errorf("a quiet tick after the rise aged out raised %d items, want none", len(items))
	}
}

// refusalsStatsErr is the stats read's failure in the tests that fail it.
var refusalsStatsErr = errors.New("the gateway did not answer")

// refusalsSamples counts the samples the window holds across every reason.
func refusalsSamples(w *RefusalWindow) int {
	n := 0
	for _, history := range w.samples {
		n += len(history)
	}
	return n
}

// TestGatewayRefusalsAttentionEdges: an answer without the refusals field raises
// nothing and adds nothing, and a failed stats read takes the probe-failed path:
// one info item naming the probe, no refusals item, and no error.
func TestGatewayRefusalsAttentionEdges(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.src.refusalWindow = &RefusalWindow{}

	absent := func(context.Context) (bdgateway.GatewayStats, error) { return bdgateway.GatewayStats{}, nil }
	if items := refusalsTickWith(t, f, refusalsTick, absent); len(items) != 0 {
		t.Fatalf("an absent refusals field raised %d items, want none", len(items))
	}
	if n := refusalsSamples(f.src.refusalWindow); n != 0 {
		t.Errorf("an absent refusals field left %d samples in the window, want none", n)
	}

	failing := func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, refusalsStatsErr
	}
	items := refusalsTickWith(t, f, refusalsTick.Add(10*time.Minute), failing)
	if len(items) != 1 {
		t.Fatalf("a failed stats read raised %d items, want the probe-failed item alone", len(items))
	}
	probe := items[0]
	if probe.Key != hostProbeFailedPrefix+gatewayRefusalsProbe || probe.Kind != attention.KindHostProbe {
		t.Errorf("probe item = key %q kind %s, want key %q kind %s",
			probe.Key, probe.Kind, hostProbeFailedPrefix+gatewayRefusalsProbe, attention.KindHostProbe)
	}
	if !strings.Contains(probe.Summary, refusalsStatsErr.Error()) {
		t.Errorf("probe summary %q does not carry the read's error", probe.Summary)
	}
	if n := refusalsSamples(f.src.refusalWindow); n != 0 {
		t.Errorf("a failed stats read left %d samples in the window, want none", n)
	}
}

// TestGatewayRefusalsAttentionEdgesNoAdd: a raised item survives stats reads that
// fail for longer than the hour, unchanged, and the window takes no sample from
// them: the failed ticks age its samples out only if Add is called.
func TestGatewayRefusalsAttentionEdgesNoAdd(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.src.refusalWindow = &RefusalWindow{}
	refusalsTickAt(t, f, refusalsTick, map[string]uint64{"cap": 0, "stale": 0})
	raised := refusalsTickAt(t, f, refusalsTick.Add(20*time.Minute), map[string]uint64{"cap": 4, "stale": 2})
	if len(raised) != 1 || raised[0].Kind != attention.KindBdGatewayRefusals {
		t.Fatalf("growth of 6 raised %v, want one refusals item", raised)
	}
	f.src.prev = attention.State{Items: raised}
	samples := refusalsSamples(f.src.refusalWindow)

	failing := func(context.Context) (bdgateway.GatewayStats, error) {
		return bdgateway.GatewayStats{}, refusalsStatsErr
	}
	for _, after := range []int{40, 70, 100, 130} {
		items := refusalsTickWith(t, f, refusalsTick.Add(time.Duration(after)*time.Minute), failing)
		var kept []attention.Item
		for _, it := range items {
			if it.Kind == attention.KindBdGatewayRefusals {
				kept = append(kept, it)
			}
		}
		if len(kept) != 1 || kept[0].Summary != raised[0].Summary {
			t.Fatalf("failed read at +%dm kept refusals items %v, want the raised one unchanged", after, kept)
		}
		f.src.prev = attention.State{Items: items}
	}
	if n := refusalsSamples(f.src.refusalWindow); n != samples {
		t.Errorf("failed reads changed the window from %d to %d samples, want no Add", samples, n)
	}
}
