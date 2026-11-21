package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestCacheTrimClaimRoutesToTheClaimJob pins that a daemon with the claim Job
// route on trims a claim through the claim Job only: the Job is applied, no exec
// runs in a seat pod and no lister Job is applied, and the claim returns true.
func TestCacheTrimClaimRoutesToTheClaimJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	f.d.cacheTrimClaimJobRoute = true
	ok := f.d.cacheTrimClaim(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{}, time.Time{})
	if !ok {
		t.Fatalf("a claim trimmed through the claim Job = false, want true; log: %s", f.log)
	}
	if applied := f.applied(t); len(applied) != 1 || applied[0] != "Job "+cacheTrimJob {
		t.Errorf("the run applied %v; want only the claim Job %s", applied, cacheTrimJob)
	}
	for _, verb := range f.verbs() {
		if strings.Contains(verb, " exec ") || strings.HasPrefix(verb, "exec ") {
			t.Errorf("the run made an exec call %q; the claim Job is the only route", verb)
		}
	}
}

// TestCacheTrimClaimRouteClearsTheNoOpItem pins that the route clears the
// claim's cache-trim-noop item itself: the claim Job measures nothing, so an
// item an earlier measure raised for a claim no pod holds describes nothing
// the pass can judge, and the old route cleared it too.
func TestCacheTrimClaimRouteClearsTheNoOpItem(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[]}`},
		cacheTrimClaimJobLogVerb:                         {stdout: "removed 0 absent 0 kept 0\n"},
	})
	f.d.cacheTrimClaimJobRoute = true
	key := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName)
	f.d.cacheTrimNoOp.raise(attention.Item{Key: key})
	f.d.cacheTrimClaim(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{}, time.Time{})
	for _, it := range f.d.cacheTrimNoOp.observed() {
		if it.Key == key {
			t.Fatalf("the claim's no-op item %q is still raised after the route ran", key)
		}
	}
}
