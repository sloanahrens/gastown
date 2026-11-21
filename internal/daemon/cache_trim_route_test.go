package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// TestCacheTrimClaimRoutesToTheClaimJob pins that a daemon with the claim Job
// route on trims a claim through the claim Job only: the Job is applied, no exec
// runs in a seat pod and no lister Job is applied, and the claim returns true.
func TestCacheTrimClaimRoutesToTheClaimJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	ok := f.d.cacheTrimClaim(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{})
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

// TestCacheTrimClaimRouteClearsTheNoOpItemAtAFinishedTrimUnderThePercent pins
// that the route does not clear the item at the start of a pass: a finished trim
// that leaves the disk under the percent clears the item an earlier pass raised.
func TestCacheTrimClaimRouteClearsTheNoOpItemAtAFinishedTrimUnderThePercent(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[]}`},
		cacheTrimClaimJobLogVerb:                         {stdout: "removed 0 absent 0 kept 0\n"},
	})
	f.d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		return []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 50}}
	}
	key := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName)
	f.d.cacheTrimNoOp.raise(attention.Item{Key: key})
	f.d.cacheTrimClaim(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, true, map[string]string{})
	for _, it := range f.d.cacheTrimNoOp.observed() {
		if it.Key == key {
			t.Fatalf("the claim's no-op item %q is still raised after the route ran", key)
		}
	}
}
