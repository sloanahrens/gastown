package daemon

import (
	"fmt"
	"regexp"
	"strings"
)

// nodePruneJobNameRE is the only shape the Job name may take: Kubernetes requires
// a lower-case DNS-1123 name, and a run id holds capitals.
var nodePruneJobNameRE = regexp.MustCompile(`^gt-node-prune-[a-z0-9]{12}$`)

// nodePruneJobName returns gt-node-prune- plus the first 12 characters of runID,
// lower-cased. An id that does not give that name is refused, with an error naming it.
func nodePruneJobName(runID string) (string, error) {
	if len(runID) < 12 || !nodePruneJobNameRE.MatchString("gt-node-prune-"+strings.ToLower(runID[:12])) {
		return "", fmt.Errorf("node-prune run id %q does not give a lower-case DNS-1123 Job name", runID)
	}
	return "gt-node-prune-" + strings.ToLower(runID[:12]), nil
}
