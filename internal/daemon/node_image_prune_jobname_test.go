package daemon

import (
	"strings"
	"testing"
)

// TestNodePruneJobName: a real run id gives the lower-case DNS-1123 Job name; an id
// that cannot is refused with an error naming it.
func TestNodePruneJobName(t *testing.T) {
	t.Parallel()
	if got, err := nodePruneJobName("20261120T002806Z-64ff48b3"); err != nil || got != "gt-node-prune-20261120t002" || !nodePruneJobNameRE.MatchString(got) {
		t.Fatalf("nodePruneJobName = %q, %v; want gt-node-prune-20261120t002 matching %s", got, err, nodePruneJobNameRE)
	}
	for _, bad := range []string{"20261120T00", "", "20261120-002806Z"} {
		if _, err := nodePruneJobName(bad); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("nodePruneJobName(%q) error = %v, want an error naming the id", bad, err)
		}
	}
}
