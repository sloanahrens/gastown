package cmd

import (
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// archiveIssueShards picks a rig's issues files out of a `git ls-tree --name-only`
// listing and returns them unchanged in shard order; a name jsonlShardName never writes is out.
func archiveIssueShards(listing string) []string {
	byShard := make(map[int]string)
	for _, line := range strings.Split(listing, "\n") {
		base := path.Base(line)
		if base == "issues.jsonl" {
			byShard[1] = line
			continue
		}
		rest, ok := strings.CutPrefix(base, "issues.")
		if !ok {
			continue
		}
		number, ok := strings.CutSuffix(rest, ".jsonl")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(number)
		// A canonical number keeps out issues.02.jsonl and issues.+2.jsonl.
		if err != nil || n < 2 || number != strconv.Itoa(n) {
			continue
		}
		byShard[n] = line
	}
	if len(byShard) == 0 {
		return nil
	}
	shards := make([]string, 0, len(byShard))
	for _, n := range slices.Sorted(maps.Keys(byShard)) {
		shards = append(shards, byShard[n])
	}
	return shards
}
