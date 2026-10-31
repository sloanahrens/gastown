package tiersweep

import (
	"regexp"
	"sort"
	"strings"
)

// Partition splits names into at most k shards whose walls are close to equal,
// so a package's tests can run as k processes that finish at about the same
// time. Every name lands in exactly one shard and the input's multiset is kept.
// A name with no wall in walls takes the median of the known walls among names,
// or 0 when none is known. Names are placed longest first, each on the shard
// with the least wall so far, then the fewest names, then the lowest index, so
// the same names and walls give the same shards in any input order. Shards with
// no names are dropped. A k below 1 is read as 1, so no name is lost.
func Partition(names []string, walls map[string]float64, k int) [][]string {
	if len(names) == 0 {
		return nil
	}
	if k < 1 {
		k = 1
	}
	if k > len(names) {
		k = len(names)
	}
	med := medianWall(names, walls)
	type job struct {
		name string
		wall float64
	}
	jobs := make([]job, 0, len(names))
	for _, n := range names {
		w, ok := walls[n]
		if !ok {
			w = med
		}
		jobs = append(jobs, job{name: n, wall: w})
	}
	sort.SliceStable(jobs, func(i, j int) bool {
		if jobs[i].wall != jobs[j].wall {
			return jobs[i].wall > jobs[j].wall
		}
		return jobs[i].name < jobs[j].name
	})
	shards := make([][]string, k)
	loads := make([]float64, k)
	for _, j := range jobs {
		best := 0
		for s := 1; s < k; s++ {
			if loads[s] < loads[best] || (loads[s] == loads[best] && len(shards[s]) < len(shards[best])) {
				best = s
			}
		}
		shards[best] = append(shards[best], j.name)
		loads[best] += j.wall
	}
	var out [][]string
	for _, s := range shards {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// medianWall is the median of the walls of the names that have one, or 0 when
// none does. An even count takes the mean of its two middle walls.
func medianWall(names []string, walls map[string]float64) float64 {
	var known []float64
	for _, n := range names {
		if w, ok := walls[n]; ok {
			known = append(known, w)
		}
	}
	if len(known) == 0 {
		return 0
	}
	sort.Float64s(known)
	mid := len(known) / 2
	if len(known)%2 == 1 {
		return known[mid]
	}
	return (known[mid-1] + known[mid]) / 2
}

// RunPattern is the -test.run pattern that selects exactly the tests named in
// shard. Each name is regexp-escaped and the alternatives are anchored, so the
// pattern for [TestA] matches TestA and no test that only contains it. An empty
// shard gives ^$, which selects nothing: an empty pattern would select every
// test. It covers top-level test names only, not subtests.
func RunPattern(shard []string) string {
	if len(shard) == 0 {
		return "^$"
	}
	quoted := make([]string, len(shard))
	for i, n := range shard {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}
