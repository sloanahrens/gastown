package daemon

import (
	"context"
	"strings"
	"testing"
)

// viaJob runs cacheTrimClaimViaJob on the fixture's claim with an empty
// namespace ledger, the way one pass calls it for its first claim.
func viaJob(f *cacheTrimFixture, apply bool, trimmed map[string]string) bool {
	return f.d.cacheTrimClaimViaJob(context.Background(), "/kubeconfig", cacheTrimClaim{ClaimName: cacheTrimClaimName},
		"gastown-polecat:dev", cacheTrimNamespace, 1<<30, apply, trimmed)
}

// TestCacheTrimViaJobUsesTheClaimJobOnly pins that the claim is trimmed by the
// claim Job and nothing else: the Job is applied, no exec runs in a seat pod,
// no lister Job is applied, and the run reports a trim.
func TestCacheTrimViaJobUsesTheClaimJobOnly(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: "removed 4 absent 1 kept 2\n"},
	})
	if !viaJob(f, true, map[string]string{}) {
		t.Fatalf("a finished claim Job = false, want true; log: %s", f.log)
	}
	if applied := f.applied(t); len(applied) != 1 || applied[0] != "Job "+cacheTrimJob {
		t.Errorf("the run applied %v; want only the claim Job %s", applied, cacheTrimJob)
	}
	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "exec ") || strings.Contains(verb, " exec ") {
			t.Errorf("the run made an exec call %q; the claim Job is the only route", verb)
		}
	}
}

// TestCacheTrimViaJobFailureReturnsFalse pins that a claim Job whose result
// line cannot be read is logged as a failure and returns false, never a
// recorded trim.
func TestCacheTrimViaJobFailureReturnsFalse(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		cacheTrimClaimJobLogVerb: {stdout: ""},
	})
	if viaJob(f, true, map[string]string{}) {
		t.Fatalf("a claim Job with no result line = true, want false")
	}
	if want := "cache trim " + cacheTrimClaimName + " in " + cacheTrimNamespace + ": failed: "; !strings.Contains(f.log.String(), want) {
		t.Errorf("the log lacks %q; log: %s", want, f.log)
	}
}

// TestCacheTrimViaJobRefusesASecondClaimInANamespace pins that a namespace
// whose claim was already trimmed this pass is a verdict: nothing is applied and
// the run returns true, as the old route did.
func TestCacheTrimViaJobRefusesASecondClaimInANamespace(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	if !viaJob(f, true, map[string]string{cacheTrimNamespace: "gt-cache-other"}) {
		t.Fatalf("a second claim in one namespace = false, want true (a verdict, not a retry)")
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("the refused claim applied %v; want nothing", applied)
	}
	if !strings.Contains(f.log.String(), "gt-cache-other") {
		t.Errorf("the refusal does not name the claim already trimmed; log: %s", f.log)
	}
}

// TestCacheTrimViaJobStopsAtAPodReadItCannotMake pins that a pod list the
// cluster cannot answer trims nothing and returns false.
func TestCacheTrimViaJobStopsAtAPodReadItCannotMake(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stderr: "error: connection refused", code: 1},
	})
	if viaJob(f, true, map[string]string{}) {
		t.Fatalf("an unreadable pod list = true, want false")
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("the run applied %v with no sandbox count; want nothing", applied)
	}
}

// TestCacheTrimViaJobDryRunAppliesNothing pins that a dry run logs the Job it
// would apply, applies nothing and still returns true.
func TestCacheTrimViaJobDryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	if !viaJob(f, false, map[string]string{}) {
		t.Fatalf("a dry run = false, want true; log: %s", f.log)
	}
	if applied := f.applied(t); len(applied) != 0 {
		t.Errorf("a dry run applied %v; want nothing", applied)
	}
	if !strings.Contains(f.log.String(), "dry_run") {
		t.Errorf("a dry run logged no dry_run line; log: %s", f.log)
	}
}
