package daemon

import "fmt"

// nodeImagePruneSummary turns one node-prune report into the lines the daemon
// logs and the figures its ledger records. An applied run plans removed+failed.
func nodeImagePruneSummary(r nodePruneReport, apply bool) (lines []string, stats map[string]int64) {
	if !apply {
		lines = append([]string{fmt.Sprintf("node_image_prune: dry run planned %d images", r.Planned)}, r.Lines...)
		return lines, map[string]int64{"planned": int64(r.Planned), "removed": 0, "failed": 0}
	}
	lines = []string{fmt.Sprintf("node_image_prune: applied removed %d failed %d", r.Removed, r.Failed)}
	return lines, map[string]int64{"planned": int64(r.Removed + r.Failed), "removed": int64(r.Removed), "failed": int64(r.Failed)}
}
