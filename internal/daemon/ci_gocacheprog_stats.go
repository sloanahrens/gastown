package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ciGocacheprogStatsCounts are the eight counts each stats line carries; the
// sum adds each one across the good lines (gt-o62k1l).
var ciGocacheprogStatsCounts = []string{"gets", "hits", "misses", "puts", "put_bytes", "hit_bytes", "errors", "elapsed_ms"}

// ciGocacheprogEvictKeys are the eight eviction figures a line holds when the
// cache program's eviction ran (gt-p9mvuy). A line holds all eight or none.
var ciGocacheprogEvictKeys = []string{"entries", "total_bytes", "young_bytes", "cap_bytes", "over_in_use_bytes", "evicted_entries", "evicted_bytes", "evict_ms"}

// sumGocacheprogStats sums the JSON lines of a cache program's stats.jsonl
// (gt-o62k1l). The counts map always holds every count key, so an empty input
// gives zero counts. processes counts the lines summed and bad_lines the lines
// refused. A refused line adds nothing: it is not a JSON object, lacks one of
// the eight counts, holds a count that is not a non-negative int64, holds only
// some of the eight eviction figures or one that is not a non-negative int64,
// would overflow a sum, or is a last line with no newline. Other fields are
// ignored.
//
// store is nil unless a good line holds the eight eviction figures. Then it
// sums evictions (such lines), evicted_entries, evicted_bytes and evict_ms, and
// takes entries, claim_bytes_before (the last line's total_bytes), young_bytes,
// cap_bytes and over_in_use_bytes from the last such line.
func sumGocacheprogStats(data []byte) (map[string]int64, map[string]int64) {
	counts := map[string]int64{"processes": 0, "bad_lines": 0}
	for _, key := range ciGocacheprogStatsCounts {
		counts[key] = 0
	}
	var store, last map[string]int64
	rest := data
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			counts["bad_lines"]++
			break
		}
		line := rest[:i]
		rest = rest[i+1:]
		vals, evict, ok := parseGocacheprogStatsLine(line)
		if !ok || gocacheprogStatsOverflows(counts, vals) {
			counts["bad_lines"]++
			continue
		}
		var delta map[string]int64
		if evict != nil {
			delta = map[string]int64{"evictions": 1, "evicted_entries": evict["evicted_entries"], "evicted_bytes": evict["evicted_bytes"], "evict_ms": evict["evict_ms"]}
			if gocacheprogStatsOverflows(store, delta) {
				counts["bad_lines"]++
				continue
			}
		}
		for key, v := range vals {
			counts[key] += v
		}
		counts["processes"]++
		if evict != nil {
			if store == nil {
				store = map[string]int64{"evictions": 0, "evicted_entries": 0, "evicted_bytes": 0, "evict_ms": 0}
			}
			for key, v := range delta {
				store[key] += v
			}
			last = evict
		}
	}
	if last != nil {
		store["entries"] = last["entries"]
		store["claim_bytes_before"] = last["total_bytes"]
		store["young_bytes"] = last["young_bytes"]
		store["cap_bytes"] = last["cap_bytes"]
		store["over_in_use_bytes"] = last["over_in_use_bytes"]
	}
	return counts, store
}

// parseGocacheprogStatsLine reads the eight counts from one stats line and, when
// the line holds eviction figures, the eight of those too (evict is nil
// otherwise). ok is false for any line that is not an object holding each count
// as a non-negative int64 literal, or that holds only some of the eviction
// figures or one that is not a non-negative int64 literal.
func parseGocacheprogStatsLine(line []byte) (vals, evict map[string]int64, ok bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return nil, nil, false
	}
	vals, ok = parseGocacheprogStatsFields(fields, ciGocacheprogStatsCounts)
	if !ok {
		return nil, nil, false
	}
	present := 0
	for _, key := range ciGocacheprogEvictKeys {
		if _, has := fields[key]; has {
			present++
		}
	}
	switch present {
	case 0:
		return vals, nil, true
	case len(ciGocacheprogEvictKeys):
		evict, ok = parseGocacheprogStatsFields(fields, ciGocacheprogEvictKeys)
		if !ok {
			return nil, nil, false
		}
		return vals, evict, true
	default:
		return nil, nil, false
	}
}

// parseGocacheprogStatsFields reads each named key of fields as a non-negative
// int64 literal. ok is false if one is missing or is not such a literal.
func parseGocacheprogStatsFields(fields map[string]json.RawMessage, keys []string) (map[string]int64, bool) {
	vals := make(map[string]int64, len(keys))
	for _, key := range keys {
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
// it, and this check does not lean on that. A nil counts is all zero.
func gocacheprogStatsOverflows(counts, vals map[string]int64) bool {
	for key, v := range vals {
		if v > 0 && counts[key] > math.MaxInt64-v {
			return true
		}
	}
	return false
}

// ciGocacheprogVolume is the cache program's stats volume (gt-0lo0iw), not in ciCacheVolumes.
const ciGocacheprogVolume = "ci-gocacheprog"

// ciGocacheprogStatsMaxBytes bounds the stats file a read takes: 8 MiB is far past a run's.
const ciGocacheprogStatsMaxBytes = 8 << 20

// ciGocacheprogReadScript moves $1 aside to $2, prints the first $3 bytes of it and removes $2; exit 3 is absent.
const ciGocacheprogReadScript = `set -eu
src=$1; aside=$2; limit=$3
trap 'rm -f "$aside"' EXIT
[ -f "$src" ] || { echo "stats file not found" >&2; exit 3; }
mv "$src" "$aside"
head -c "$limit" "$aside"
`

// ciGocacheprogRead takes the stats file out of the ci-gocacheprog volume (gt-0lo0iw): absent
// is present false with no error; an oversize file or a failed exec is present true with an error.
func (d *Daemon) ciGocacheprogRead(ctx context.Context, at ciCacheExecTarget) (data []byte, present bool, err error) {
	root := ciCacheVolumeRoot(ciGocacheprogVolume)
	out, stderr, err := d.ciEngineExec(ctx, at, "sh", "-c", ciGocacheprogReadScript, "ci-gocacheprog-read",
		root+"/stats.jsonl", root+"/stats.jsonl.read", strconv.Itoa(ciGocacheprogStatsMaxBytes+1))
	var status interface{ ExitCode() int }
	switch {
	case errors.As(err, &status) && status.ExitCode() == 3:
		return nil, false, nil
	case err != nil:
		return nil, true, fmt.Errorf("read gocacheprog stats: %s, last stderr line %q", ciEngineExitStatus(err), ciGocacheprogStderrLine(err, stderr))
	case len(out) > ciGocacheprogStatsMaxBytes:
		return nil, true, fmt.Errorf("read gocacheprog stats: over the %d byte limit", ciGocacheprogStatsMaxBytes)
	}
	return out, true, nil
}

// ciGocacheprogStderrLine is the last line of stderr that is not kubectl's exit report, bounded.
func ciGocacheprogStderrLine(err error, stderr []byte) string {
	lines := strings.Split(string(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := ciEngineBounded(lines[i]); line != "" && !ciEngineKubectlLine(err, lines[i]) {
			return line
		}
	}
	return ""
}
