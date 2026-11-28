package daemon

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
)

// ciGocacheprogStatsCounts are the eight counts each stats line carries; the
// sum adds each one across the good lines (gt-o62k1l).
var ciGocacheprogStatsCounts = []string{"gets", "hits", "misses", "puts", "put_bytes", "hit_bytes", "errors", "elapsed_ms"}

// sumGocacheprogStats sums the JSON lines of a cache program's stats.jsonl
// (gt-o62k1l). The map always holds every key, so an empty input gives zero
// counts. processes counts the lines summed and bad_lines the lines refused. A
// refused line adds nothing: it is not a JSON object, lacks one of the eight
// counts, holds a count that is not a non-negative int64, would overflow a sum,
// or is a last line with no newline. Other fields are ignored.
func sumGocacheprogStats(data []byte) map[string]int64 {
	counts := map[string]int64{"processes": 0, "bad_lines": 0}
	for _, key := range ciGocacheprogStatsCounts {
		counts[key] = 0
	}
	rest := data
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			counts["bad_lines"]++
			break
		}
		line := rest[:i]
		rest = rest[i+1:]
		vals, ok := parseGocacheprogStatsLine(line)
		if !ok || gocacheprogStatsOverflows(counts, vals) {
			counts["bad_lines"]++
			continue
		}
		for key, v := range vals {
			counts[key] += v
		}
		counts["processes"]++
	}
	return counts
}

// parseGocacheprogStatsLine reads the eight counts from one stats line. ok is
// false for any line that is not an object holding each count as a
// non-negative int64 literal.
func parseGocacheprogStatsLine(line []byte) (map[string]int64, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return nil, false
	}
	vals := make(map[string]int64, len(ciGocacheprogStatsCounts))
	for _, key := range ciGocacheprogStatsCounts {
		raw, ok := fields[key]
		if !ok {
			return nil, false
		}
		v, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || v < 0 {
			return nil, false
		}
		vals[key] = v
	}
	return vals, true
}

// gocacheprogStatsOverflows reports whether adding vals to counts would push any
// sum past MaxInt64. A negative v is never an overflow here: the parser refuses
// it, and this check does not lean on that.
func gocacheprogStatsOverflows(counts, vals map[string]int64) bool {
	for key, v := range vals {
		if v > 0 && counts[key] > math.MaxInt64-v {
			return true
		}
	}
	return false
}
