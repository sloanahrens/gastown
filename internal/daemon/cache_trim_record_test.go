package daemon

import (
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// TestCacheTrimRecordRunLeavesAnUnmeasuredBeforeOut: a run with no before
// figure writes no claim_bytes_before stat, and a run with one still writes it.
func TestCacheTrimRecordRunLeavesAnUnmeasuredBeforeOut(t *testing.T) {
	t.Parallel()
	for _, before := range []*int64{nil, ptrInt64(3000)} {
		f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
		f.d.cacheTrimRecordRun(cacheTrimRun{claim: cacheTrimClaimName, namespace: cacheTrimNamespace,
			started: diskMaintNow, outcome: cacheTrimRunOK, before: before})
		runs := f.jobRuns(t)
		if len(runs) != 1 {
			t.Fatalf("the ledger holds %d run(s), want one: %+v", len(runs), runs)
		}
		got, present := runs[0].Stats[cacheTrimStatBytesBefore]
		if before == nil && present {
			t.Errorf("run with no before figure holds %s = %d, want it absent", cacheTrimStatBytesBefore, got)
		}
		if before != nil && (!present || got != *before) {
			t.Errorf("run with before %d holds %s = %d (present %v)", *before, cacheTrimStatBytesBefore, got, present)
		}
	}
}
