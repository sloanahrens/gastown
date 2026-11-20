package daemon

import (
	"reflect"
	"testing"
)

// TestNodeImagePruneSummary turns one prune report into the log lines and ledger
// figures of a run: a dry run logs its planned count and each would-remove line,
// an applied run logs one line with its removed and failed counts.
func TestNodeImagePruneSummary(t *testing.T) {
	t.Parallel()

	dry := nodePruneReport{Planned: 3, Lines: []string{
		"node-prune: would remove sha256:aaa gastown-polecat created 2026-11-20T03:00:00Z",
		"node-prune: would remove sha256:bbb gastown-daemon created 2026-11-20T03:01:00Z",
		"node-prune: would remove sha256:ccc gastown-bd created 2026-11-20T03:02:00Z",
	}}
	lines, stats := nodeImagePruneSummary(dry, false)
	wantLines := append([]string{"node_image_prune: dry run planned 3 images"}, dry.Lines...)
	if !reflect.DeepEqual(lines, wantLines) || !reflect.DeepEqual(stats, map[string]int64{"planned": 3, "removed": 0, "failed": 0}) {
		t.Errorf("dry run = %q, %v, want %q with planned 3 removed 0 failed 0", lines, stats, wantLines)
	}

	lines, stats = nodeImagePruneSummary(nodePruneReport{Removed: 5, Failed: 1}, true)
	wantLines = []string{"node_image_prune: applied removed 5 failed 1"}
	if !reflect.DeepEqual(lines, wantLines) || !reflect.DeepEqual(stats, map[string]int64{"planned": 6, "removed": 5, "failed": 1}) {
		t.Errorf("applied = %q, %v, want %q with planned 6 removed 5 failed 1", lines, stats, wantLines)
	}
}
