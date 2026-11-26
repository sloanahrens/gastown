package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

// capHitItems returns the bd_gateway.cap_hit items in st.
func capHitItems(st attention.State) []attention.Item {
	var out []attention.Item
	for _, it := range st.Items {
		if it.Kind == attention.KindBDGatewayCapHit {
			out = append(out, it)
		}
	}
	return out
}

// TestAttentionTickGatewayCap: the attention tick runs the gateway cap collector.
// A gateway whose TooManyCalls grows from 0 to 3 between two ticks holds one
// bd_gateway.cap_hit item after the second tick; with no gateway wired the
// tick raises none.
func TestAttentionTickGatewayCap(t *testing.T) {
	t.Parallel()
	t.Run("growth over two ticks holds one cap_hit item", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, refusalsTick)
		reads := []uint64{0, 3}
		var tick int
		f.src.gatewayCap = &gatewayCapCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
			st := bdgateway.GatewayStats{TooManyCalls: reads[tick], PeakInFlight: 5}
			tick++
			return st, nil
		}}

		first := f.tick(t, refusalsTick)
		if got := capHitItems(first); len(got) != 0 {
			t.Fatalf("first tick holds %d cap_hit items, want none (it only sets the base): %+v", len(got), first.Items)
		}
		second := f.tick(t, refusalsTick.Add(time.Minute))
		if got := capHitItems(second); len(got) != 1 {
			t.Fatalf("second tick holds %d cap_hit items, want 1: %+v", len(got), second.Items)
		}
	})
	t.Run("no gateway wired raises none", func(t *testing.T) {
		t.Parallel()
		f := newAttentionFixture(t, refusalsTick)
		f.src.gatewayCap = nil
		f.tick(t, refusalsTick)
		second := f.tick(t, refusalsTick.Add(time.Minute))
		if got := capHitItems(second); len(got) != 0 {
			t.Fatalf("tick with no gateway holds %d cap_hit items, want none: %+v", len(got), second.Items)
		}
	})
}

// TestAttentionTickGatewayCapWiredPerDaemon: the production sources hold the
// Daemon's cap collector when a gateway is configured, and the same one on every
// tick, since the tracker's base must outlive the sources rebuilt each tick. With
// no gateway configured they hold none.
func TestAttentionTickGatewayCapWiredPerDaemon(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, refusalsTick)
	f.d.getenv = func(key string) string {
		if key == bdgateway.WorkspaceURLEnv {
			return "http://gateway.test"
		}
		return ""
	}
	first := f.d.attentionSources(refusalsTick)
	second := f.d.attentionSources(refusalsTick.Add(time.Minute))
	if first.gatewayCap == nil || first.gatewayCap != &f.d.gatewayCap {
		t.Fatalf("first tick's cap collector = %p, want the Daemon's %p", first.gatewayCap, &f.d.gatewayCap)
	}
	if second.gatewayCap != first.gatewayCap {
		t.Fatalf("second tick's cap collector = %p, want the same %p as the first", second.gatewayCap, first.gatewayCap)
	}

	f.d.getenv = func(string) string { return "" }
	if none := f.d.attentionSources(refusalsTick); none.gatewayCap != nil {
		t.Fatalf("tick with no gateway configured holds a cap collector, want nil")
	}
}
