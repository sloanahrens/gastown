package taskmap

import (
	"strings"
	"testing"
)

// TestJobsCheckDoesNotPromiseExitZeroOnAFreshTown pins the jobs Check
// (gt-rwj9td) to what jobsStatus in internal/cmd/jobs.go enforces: exit 0 only
// when no row is unknown and no run failed. On a fresh town the node disk row
// has no reading, so it reads unknown and the command exits 1; the Check must
// not send the reader to a bare exit 0.
func TestJobsCheckDoesNotPromiseExitZeroOnAFreshTown(t *testing.T) {
	t.Parallel()

	e, ok := Lookup("jobs")
	if !ok {
		t.Fatal("Lookup(jobs) found no entry")
	}
	if strings.Contains(e.Check, "gt jobs exits 0") {
		t.Errorf("jobs Check = %q; the command exits 1 on a fresh town, so a bare exit 0 is not the sign", e.Check)
	}
	for _, want := range []string{"exits 0 only when", "every row is readable", "no run failed"} {
		if !strings.Contains(e.Check, want) {
			t.Errorf("jobs Check = %q, want it to name %q from jobsStatus", e.Check, want)
		}
	}
}
