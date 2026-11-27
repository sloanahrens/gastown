package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/bdgateway"
)

func u64p(v uint64) *uint64 { return &v }

// TestGatewayUnreachableCollect: the first read sets the base, a rise of 12
// raises the item, and a failed or counterless read keeps the base.
func TestGatewayUnreachableCollect(t *testing.T) {
	t.Parallel()
	type tick struct {
		count *uint64
		err   error
		want  int
	}
	for _, tc := range []struct {
		name  string
		ticks []tick
	}{
		{"0 then 12 raises one", []tick{{count: u64p(0)}, {count: u64p(12), want: 1}}},
		{"0 then 3 raises none", []tick{{count: u64p(0)}, {count: u64p(3)}}},
		{"a failed read keeps the base", []tick{{count: u64p(0)}, {err: errors.New("down")}, {count: u64p(12), want: 1}}},
		{"a read without the counter keeps the base", []tick{{count: u64p(0)}, {}, {count: u64p(12), want: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cur tick
			c := &gatewayUnreachableCollector{stats: func(context.Context) (bdgateway.GatewayStats, error) {
				return bdgateway.GatewayStats{Unreachable: cur.count}, cur.err
			}}
			for i, tk := range tc.ticks {
				cur = tk
				items, err := c.collect(context.Background())
				if !errors.Is(err, tk.err) || len(items) != tk.want || (tk.want > 0 && items[0].Kind != attention.KindBDGatewayUnreachable) {
					t.Fatalf("tick %d: items %+v, err %v; want %d items, error %v", i, items, err, tk.want, tk.err)
				}
			}
		})
	}
	f := newAttentionFixture(t, refusalsTick)
	f.d.getenv = func(string) string { return "" }
	s := f.d.attentionSources(refusalsTick)
	items, err := s.collectGatewayUnreachable(context.Background())
	if s.gatewayUnreachable != nil || err != nil || len(items) != 0 {
		t.Fatalf("no gateway: collector %v, items %d, err %v", s.gatewayUnreachable, len(items), err)
	}
	f.d.getenv = func(string) string { return "http://gateway.test" }
	if s := f.d.attentionSources(refusalsTick); s.gatewayUnreachable != &f.d.gatewayUnreachable {
		t.Fatalf("sources do not hold the Daemon's collector")
	}
}
