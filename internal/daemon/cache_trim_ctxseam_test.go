package daemon

import (
	"context"
	"testing"
)

// TestCacheTrimCtxSeamSeesTheCallsContext pins that the seam is handed the very
// context each cache trim cluster call was made with, once per call, and that a
// nil seam changes nothing.
func TestCacheTrimCtxSeamSeesTheCallsContext(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{"get namespace": {stdout: "ns-a\n"}})
	var seen []context.Context
	f.d.cacheTrimCtxSeen = func(ctx context.Context, _ string) { seen = append(seen, ctx) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 2 {
		if out, err := f.d.cacheTrimKubectl(ctx, "/kubeconfig", "get", "namespace"); err != nil || string(out) != "ns-a\n" {
			t.Fatalf("cacheTrimKubectl = %q, %v; want the cluster's answer", out, err)
		}
	}
	if len(seen) != 2 || seen[0] != ctx || seen[1] != ctx {
		t.Fatalf("the seam saw %d contexts; want the call's own context once per call", len(seen))
	}
	f.d.cacheTrimCtxSeen = nil
	if out, err := f.d.cacheTrimKubectl(ctx, "/kubeconfig", "get", "namespace"); err != nil || string(out) != "ns-a\n" {
		t.Fatalf("with a nil seam, cacheTrimKubectl = %q, %v; want the cluster's answer", out, err)
	}
	if got := len(f.verbs()); got != 3 {
		t.Errorf("the calls ran %d times; want 3", got)
	}
}
