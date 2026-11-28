package gocacheprog

import (
	"cmp"
	"fmt"
	"slices"
)

// Summary describes the index a plan started from and what the plan evicts.
// A body several entries name counts once in the totals and is evicted with
// the last of those entries.
type Summary struct {
	Entries        int   // entries in the index before eviction
	TotalBytes     int64 // bytes of the distinct bodies the index names
	EvictedEntries int   // entries the plan evicts
	EvictedBytes   int64 // bytes of the bodies no remaining entry names
}

// Plan returns the actions to evict so the bodies idx names total at most
// maxBytes, least recently used first and ties by action id, the outputs no
// remaining entry names, and a summary of idx and of the eviction.
func Plan(idx Index, maxBytes int64) (actions, outputs [][]byte, summary Summary, err error) {
	if maxBytes <= 0 {
		return nil, nil, Summary{}, fmt.Errorf("gocacheprog: plan: max bytes %d is not above 0", maxBytes)
	}
	bodyBytes := make(map[string]int64, len(idx))
	named := make(map[string]int, len(idx))
	for _, e := range idx {
		if size, ok := bodyBytes[e.OutputID]; ok {
			if size != e.Size {
				return nil, nil, Summary{}, fmt.Errorf("gocacheprog: plan: output %s is named at sizes %d and %d", e.OutputID, size, e.Size)
			}
			named[e.OutputID]++
			continue
		}
		bodyBytes[e.OutputID] = e.Size
		named[e.OutputID] = 1
		summary.TotalBytes += e.Size
	}
	summary.Entries = len(idx)
	order := make([]string, 0, len(idx))
	for action := range idx {
		order = append(order, action)
	}
	slices.SortFunc(order, func(a, b string) int {
		return cmp.Or(idx[a].LastUse.Compare(idx[b].LastUse), cmp.Compare(a, b))
	})
	remaining := summary.TotalBytes
	for _, action := range order {
		if remaining <= maxBytes {
			break
		}
		e := idx[action]
		actions = append(actions, []byte(action))
		summary.EvictedEntries++
		named[e.OutputID]--
		if named[e.OutputID] == 0 {
			outputs = append(outputs, []byte(e.OutputID))
			summary.EvictedBytes += e.Size
			remaining -= e.Size
		}
	}
	return actions, outputs, summary, nil
}
