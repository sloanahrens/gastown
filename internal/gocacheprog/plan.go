package gocacheprog

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// youngWindow is how recently a body must have been used for the summary to
// count it as young.
const youngWindow = 75 * time.Minute

// Summary describes the index a plan started from and what the plan evicts.
// A body several entries name counts once in the totals and is evicted with
// the last of those entries.
type Summary struct {
	Entries        int   // entries in the index before eviction
	TotalBytes     int64 // bytes of the distinct bodies the index names
	YoungBytes     int64 // bytes of the bodies last used within youngWindow of now
	EvictedEntries int   // entries the plan evicts
	EvictedBytes   int64 // bytes of the bodies no remaining entry names
	OverInUseBytes int64 // bytes the remaining index is over maxBytes by, else 0
}

// Plan returns the actions to evict so the bodies idx names total at most
// maxBytes, least recently used first and ties by action id, the outputs no
// remaining entry names, and a summary of idx and of the eviction.
//
// An entry last used at or after a non-zero protectFrom, and every body it
// names, is never evicted; if those leave the index over maxBytes, OverInUse
// reports the excess. A zero protectFrom protects nothing.
func Plan(idx Index, maxBytes int64, protectFrom, now time.Time) (actions, outputs [][]byte, summary Summary, err error) {
	if maxBytes <= 0 {
		return nil, nil, Summary{}, fmt.Errorf("gocacheprog: plan: max bytes %d is not above 0", maxBytes)
	}
	bodyBytes := make(map[string]int64, len(idx))
	bodyUse := make(map[string]time.Time, len(idx))
	named := make(map[string]int, len(idx))
	for _, e := range idx {
		if use, ok := bodyUse[e.OutputID]; !ok || e.LastUse.After(use) {
			bodyUse[e.OutputID] = e.LastUse
		}
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
	youngFrom := now.Add(-youngWindow)
	for output, size := range bodyBytes {
		if !bodyUse[output].Before(youngFrom) {
			summary.YoungBytes += size
		}
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
		if !protectFrom.IsZero() && !e.LastUse.Before(protectFrom) {
			continue
		}
		actions = append(actions, []byte(action))
		summary.EvictedEntries++
		named[e.OutputID]--
		if named[e.OutputID] == 0 {
			outputs = append(outputs, []byte(e.OutputID))
			summary.EvictedBytes += e.Size
			remaining -= e.Size
		}
	}
	if remaining > maxBytes {
		summary.OverInUseBytes = remaining - maxBytes
	}
	return actions, outputs, summary, nil
}
