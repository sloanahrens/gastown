package cmd

import (
	"bytes"
	"context"
	"io"
	"strings"
)

// realRenderDrift is the update Job's render-drift hook: it runs the drift
// command's compare over d's seams, keeps the lines the compare prints to
// itself, and answers how many distinct objects those lines name. A difference
// is the compare's silent exit, which is the count, not an error; any other
// error is the compare's own and is returned unchanged, so the run can tell a
// missing record from a compare it could not read.
func realRenderDrift(d clusterDriftDeps) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		var printed bytes.Buffer
		d.out = &printed
		err := compareClusterDrift(ctx, d)
		if _, difference := IsSilentExit(err); difference {
			return driftObjectCount(printed.String()), nil
		}
		// No silent exit: a compare error is returned as it is, and a nil one
		// (a cluster in step) is no difference at all.
		return 0, err
	}
}

// driftObjectCount is the number of distinct objects the compare's printed
// lines name: the resource before the first ": " of each line. An explained
// difference and a not-compared object are printed but are not drift, so their
// lines are skipped. Each marker is matched where the compare puts it, after
// the resource, so a difference whose own text holds ": not compared: " still
// counts.
func driftObjectCount(printed string) int {
	resources := make(map[string]struct{})
	for _, line := range strings.Split(printed, "\n") {
		resource, rest, ok := strings.Cut(line, ": ")
		if !ok || resource == "" {
			continue
		}
		if strings.HasPrefix(rest, "explained: ") || strings.HasPrefix(rest, "not compared: ") {
			continue
		}
		resources[resource] = struct{}{}
	}
	return len(resources)
}

// renderDriftDepsAt is the hook's deps for a town the update run already found.
// The compare's town lookup is pinned to that root, so the hook reads the
// record the run found rather than whatever the process's working directory
// names. Its output goes to io.Discard until realRenderDrift captures it.
func renderDriftDepsAt(townRoot string) clusterDriftDeps {
	d := realClusterDriftDeps(io.Discard)
	d.render.findTown = func() (string, error) { return townRoot, nil }
	return d
}
