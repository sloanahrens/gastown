package daemon

import (
	"strconv"
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
	for _, bad := range []string{
		"20261120T00", "", "20261120-002806Z",
		// A character the name is built from survives the 12-character slice, but
		// writeNodeImagePruneLast would refuse the id the prune then cannot record.
		"20261104T200/../x", "20261104T200 evil", "20261104T200\x00", "20261104T200é",
	} {
		// The error quotes the id, so it contains it as %q renders it.
		if _, err := nodePruneJobName(bad); err == nil || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("nodePruneJobName(%q) error = %v, want an error naming the id", bad, err)
		}
	}
}
