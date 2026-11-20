package daemon

import (
	"fmt"
	"regexp"
	"strings"
)

// nodePruneJobNameRE is the only shape the Job name may take: Kubernetes requires
// a lower-case DNS-1123 name, and a run id holds capitals.
var nodePruneJobNameRE = regexp.MustCompile(`^gt-node-prune-[a-z0-9]{12}$`)

// nodePruneRunIDRE is the only shape a run id may take: letters, digits, dash and
// underscore, as writeNodeImagePruneLast requires. The whole id is checked, not the
// 12 characters the name is built from, so an id the prune could not record is
// refused before the prune runs.
var nodePruneRunIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// nodePruneJobName returns gt-node-prune- plus the first 12 characters of runID,
// lower-cased. An id that is not letters, digits, dash and underscore, or that does
// not give that name, is refused, with an error naming it.
func nodePruneJobName(runID string) (string, error) {
	if !nodePruneRunIDRE.MatchString(runID) {
		return "", fmt.Errorf("node-prune run id %q is empty or holds a character outside letters, digits, dash and underscore", runID)
	}
	if len(runID) < 12 || !nodePruneJobNameRE.MatchString("gt-node-prune-"+strings.ToLower(runID[:12])) {
		return "", fmt.Errorf("node-prune run id %q does not give a lower-case DNS-1123 Job name", runID)
	}
	return "gt-node-prune-" + strings.ToLower(runID[:12]), nil
}
