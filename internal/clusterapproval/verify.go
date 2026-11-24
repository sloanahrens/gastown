package clusterapproval

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// DefaultValidity is the longest window a record may name between its
// approved_at and expires_at; ParseRecord and Verify both refuse a longer one,
// so an approval is spent soon after it is given.
const DefaultValidity = time.Hour

// Verify reports why rec may not authorize an apply of p to the cluster whose
// live resourceVersions are live at now, and returns nil when it may.
func Verify(rec Record, p Plan, live map[string]string, now time.Time) error {
	if rec.Act != ActApplyPlan {
		return fmt.Errorf("clusterapproval: the record's act is %q, not %q", rec.Act, ActApplyPlan)
	}
	if !ValidCluster(rec.Cluster) {
		return fmt.Errorf("clusterapproval: the record's cluster is %q, not %q or %q", rec.Cluster, ClusterControl, ClusterGTSbx)
	}
	// One approval names one cluster; a record for control never authorizes an
	// apply to gt-sbx, even though both are valid names.
	if rec.Cluster != p.Cluster {
		return fmt.Errorf("clusterapproval: the record approves an apply of the %q cluster, not %q", rec.Cluster, p.Cluster)
	}
	if rec.PlanSHA256 != p.SHA256 {
		return fmt.Errorf("clusterapproval: the record approves the plan %q, not %q", rec.PlanSHA256, p.SHA256)
	}
	if now.Before(rec.ApprovedAt) {
		return fmt.Errorf("clusterapproval: the record is not valid until %s", rec.ApprovedAt.Format(time.RFC3339))
	}
	if !now.Before(rec.ExpiresAt) {
		return fmt.Errorf("clusterapproval: the record expired at %s", rec.ExpiresAt.Format(time.RFC3339))
	}
	// Verify reads a Record value, so a caller that built one without ParseRecord
	// can still name a window over the limit; refuse it here too (gt-vr6mst).
	if rec.ExpiresAt.After(rec.ApprovedAt.Add(DefaultValidity)) {
		return fmt.Errorf("clusterapproval: the record is valid for more than %s", DefaultValidity)
	}
	// Only the plan's objects are compared: an object the plan does not name
	// is not part of the apply, so its drift cannot invalidate the approval.
	for _, key := range slices.Sorted(maps.Keys(p.Versions)) {
		want := p.Versions[key]
		got, present := live[key]
		if !present {
			return fmt.Errorf("clusterapproval: the live cluster has no object %q", key)
		}
		// The error names the key and never a version value, so the operator
		// re-reads the object rather than diffing values in a log line.
		if got != want {
			return fmt.Errorf("clusterapproval: the live version of %q is not the version the plan names", key)
		}
	}
	return nil
}
