package daemon

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ciGocacheprogStatsLine is one line the cache program appends (gt-l4bzgs). The
// sum ignores its start field.
const ciGocacheprogStatsLine = `{"gets":3,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"start":"2026-11-28T00:00:00Z","elapsed_ms":1500}`

// ciGocacheprogStatsOne is the counts ciGocacheprogStatsLine alone sums to.
var ciGocacheprogStatsOne = map[string]int64{"gets": 3, "hits": 2, "misses": 1, "puts": 4, "put_bytes": 400, "hit_bytes": 200, "errors": 0, "elapsed_ms": 1500}

// ciGocacheprogStatsWant is the full map a test expects: the eight sums (absent
// ones zero), then processes and bad_lines.
func ciGocacheprogStatsWant(sums map[string]int64, processes, badLines int64) map[string]int64 {
	want := map[string]int64{"processes": processes, "bad_lines": badLines}
	for _, key := range []string{"gets", "hits", "misses", "puts", "put_bytes", "hit_bytes", "errors", "elapsed_ms"} {
		want[key] = sums[key]
	}
	return want
}

// ciGocacheprogCountsPart is the eight counts of an eviction line; the eviction
// figures follow it in the same object (gt-p9mvuy).
const ciGocacheprogCountsPart = `"gets":2,"hits":1,"misses":1,"puts":1,"put_bytes":100,"hit_bytes":50,"errors":0,"elapsed_ms":40`

// ciGocacheprogEvictPart is the eight eviction figures of one line.
const ciGocacheprogEvictPart = `"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12`

// ciGocacheprogEvictLine is one line that holds the eight counts and the eight
// eviction figures.
const ciGocacheprogEvictLine = `{` + ciGocacheprogCountsPart + `,` + ciGocacheprogEvictPart + `}`

// ciGocacheprogEvictOne is the counts ciGocacheprogEvictLine alone sums to.
var ciGocacheprogEvictOne = map[string]int64{"gets": 2, "hits": 1, "misses": 1, "puts": 1, "put_bytes": 100, "hit_bytes": 50, "errors": 0, "elapsed_ms": 40}

// ciGocacheprogEvictStoreOne is the store ciGocacheprogEvictLine alone gives.
var ciGocacheprogEvictStoreOne = map[string]int64{"evictions": 1, "evicted_entries": 3, "evicted_bytes": 450, "evict_ms": 12, "entries": 9, "claim_bytes_before": 1000, "young_bytes": 300, "cap_bytes": 800, "over_in_use_bytes": 200}

func TestCIGocacheprogSumAddsGoodLines(t *testing.T) {
	t.Parallel()
	second := `{"gets": 5,"hits":5,"misses":0,"puts":0,"put_bytes":0,"hit_bytes":900,"errors":1,"start":"2026-11-28T00:05:00Z","elapsed_ms":250,"note":"extra"}`
	data := []byte(ciGocacheprogStatsLine + "\n" + second + "\n")
	want := ciGocacheprogStatsWant(map[string]int64{"gets": 8, "hits": 7, "misses": 1, "puts": 4, "put_bytes": 400, "hit_bytes": 1100, "errors": 1, "elapsed_ms": 1750}, 2, 0)
	if got, _ := sumGocacheprogStats(data); !maps.Equal(got, want) {
		t.Fatalf("two good lines summed to %v; want %v", got, want)
	}
}

func TestCIGocacheprogSumNoEvictionLineHasNilStore(t *testing.T) {
	t.Parallel()
	_, store := sumGocacheprogStats([]byte(ciGocacheprogStatsLine + "\n"))
	if store != nil {
		t.Fatalf("a file with no eviction line gave store %v; want nil", store)
	}
}

func TestCIGocacheprogSumReadsEvictionLines(t *testing.T) {
	t.Parallel()
	// The second eviction line is the last one, so its entries, total_bytes,
	// young_bytes, cap_bytes and over_in_use_bytes are the store's figures; the
	// sums cover both eviction lines and the plain line between them.
	last := `{"gets":1,"hits":0,"misses":1,"puts":0,"put_bytes":0,"hit_bytes":0,"errors":1,"elapsed_ms":10,"entries":4,"total_bytes":600,"young_bytes":100,"cap_bytes":800,"over_in_use_bytes":0,"evicted_entries":2,"evicted_bytes":150,"evict_ms":5}`
	data := []byte(ciGocacheprogEvictLine + "\n" + ciGocacheprogStatsLine + "\n" + last + "\n")
	wantCounts := ciGocacheprogStatsWant(map[string]int64{"gets": 6, "hits": 3, "misses": 3, "puts": 5, "put_bytes": 500, "hit_bytes": 250, "errors": 1, "elapsed_ms": 1550}, 3, 0)
	wantStore := map[string]int64{"evictions": 2, "evicted_entries": 5, "evicted_bytes": 600, "evict_ms": 17, "entries": 4, "claim_bytes_before": 600, "young_bytes": 100, "cap_bytes": 800, "over_in_use_bytes": 0}
	got, store := sumGocacheprogStats(data)
	if !maps.Equal(got, wantCounts) {
		t.Fatalf("eviction lines summed counts to %v; want %v", got, wantCounts)
	}
	if !maps.Equal(store, wantStore) {
		t.Fatalf("eviction lines gave store %v; want %v", store, wantStore)
	}
}

func TestCIGocacheprogSumRefusesBadLines(t *testing.T) {
	t.Parallel()
	good := ciGocacheprogStatsLine + "\n"
	one := ciGocacheprogStatsOne
	// withBad puts one bad line ahead of a good line, so each row also proves a
	// refused line leaves the sums of the good line alone.
	withBad := func(bad string) []byte { return []byte(bad + "\n" + good) }
	missingPuts := `{"gets":3,"hits":2,"misses":1,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`
	rows := []struct {
		name string
		data []byte
		want map[string]int64
	}{
		{"empty input", nil, ciGocacheprogStatsWant(nil, 0, 0)},
		{"a blank line", []byte("\n" + good), ciGocacheprogStatsWant(one, 1, 1)},
		{"a JSON array", withBad(`[1,2]`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a JSON string", withBad(`"gets"`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a bare number", withBad(`7`), ciGocacheprogStatsWant(one, 1, 1)},
		{"JSON null", withBad(`null`), ciGocacheprogStatsWant(one, 1, 1)},
		{"text after the object", withBad(ciGocacheprogStatsLine + ` x`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a missing count", withBad(missingPuts), ciGocacheprogStatsWant(one, 1, 1)},
		{"a negative count", withBad(`{"gets":-1,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a fractional count", withBad(`{"gets":1.5,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a whole-valued float count", withBad(`{"gets":3.0,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"an exponent count", withBad(`{"gets":3e0,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a quoted count", withBad(`{"gets":"3","hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a null count", withBad(`{"gets":null,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a count past int64", withBad(`{"gets":9223372036854775808,"hits":2,"misses":1,"puts":4,"put_bytes":400,"hit_bytes":200,"errors":0,"elapsed_ms":1500}`), ciGocacheprogStatsWant(one, 1, 1)},
		{"a last line with no newline", []byte(good + ciGocacheprogStatsLine), ciGocacheprogStatsWant(one, 1, 1)},
		{"one valid line with no newline", []byte(ciGocacheprogStatsLine), ciGocacheprogStatsWant(nil, 0, 1)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, store := sumGocacheprogStats(row.data)
			if !maps.Equal(got, row.want) {
				t.Fatalf("%q summed to %v; want %v", row.data, got, row.want)
			}
			if store != nil {
				t.Fatalf("%q with no eviction line gave store %v; want nil", row.data, store)
			}
		})
	}
}

func TestCIGocacheprogSumRefusesBadEvictionFigures(t *testing.T) {
	t.Parallel()
	// Each row puts one bad eviction line ahead of ciGocacheprogEvictLine. The bad
	// line must add nothing to the counts or the store, so each row's want is the
	// good line's alone, with bad_lines 1.
	withBad := func(bad string) []byte { return []byte(bad + "\n" + ciGocacheprogEvictLine + "\n") }
	counts := func(bad string) string { return `{` + ciGocacheprogCountsPart + `,` + bad + `}` }
	rows := []struct {
		name string
		data []byte
	}{
		{"a partial eviction set", withBad(counts(`"evicted_bytes":7`))},
		{"no evict_ms of the eight", withBad(counts(`"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450`))},
		{"no entries of the eight", withBad(counts(`"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12`))},
		{"a negative entries", withBad(counts(`"entries":-9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12`))},
		{"a negative evict_ms", withBad(counts(`"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":-12`))},
		{"a fractional evicted_bytes", withBad(counts(`"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450.5,"evict_ms":12`))},
		{"a quoted total_bytes", withBad(counts(`"entries":9,"total_bytes":"1000","young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12`))},
		{"an exponent cap_bytes", withBad(counts(`"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":8e2,"over_in_use_bytes":200,"evicted_entries":3,"evicted_bytes":450,"evict_ms":12`))},
	}
	want := ciGocacheprogStatsWant(ciGocacheprogEvictOne, 1, 1)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, store := sumGocacheprogStats(row.data)
			if !maps.Equal(got, want) {
				t.Fatalf("%q summed to %v; want %v", row.data, got, want)
			}
			if !maps.Equal(store, ciGocacheprogEvictStoreOne) {
				t.Fatalf("%q gave store %v; want %v", row.data, store, ciGocacheprogEvictStoreOne)
			}
		})
	}
}

func TestCIGocacheprogSumRefusesOverflow(t *testing.T) {
	t.Parallel()
	// The first line sums gets to MaxInt64; the second would wrap it, so it is
	// refused whole rather than added.
	data := []byte(`{"gets":9223372036854775807,"hits":0,"misses":0,"puts":0,"put_bytes":0,"hit_bytes":0,"errors":0,"elapsed_ms":0}` + "\n" +
		`{"gets":1,"hits":0,"misses":0,"puts":0,"put_bytes":0,"hit_bytes":0,"errors":0,"elapsed_ms":0}` + "\n")
	want := ciGocacheprogStatsWant(map[string]int64{"gets": 9223372036854775807}, 1, 1)
	if got, _ := sumGocacheprogStats(data); !maps.Equal(got, want) {
		t.Fatalf("overflowing pair summed to %v; want %v", got, want)
	}
}

func TestCIGocacheprogSumRefusesStoreOverflow(t *testing.T) {
	t.Parallel()
	// The first eviction line sums evicted_bytes to MaxInt64; the second would
	// wrap the store, so it is refused whole. The last eviction line is therefore
	// the first one, whose entries and cache figures the store keeps.
	first := `{` + ciGocacheprogCountsPart + `,"entries":9,"total_bytes":1000,"young_bytes":300,"cap_bytes":800,"over_in_use_bytes":200,"evicted_entries":0,"evicted_bytes":9223372036854775807,"evict_ms":0}`
	second := `{"gets":1,"hits":0,"misses":0,"puts":0,"put_bytes":0,"hit_bytes":0,"errors":0,"elapsed_ms":0,"entries":4,"total_bytes":600,"young_bytes":100,"cap_bytes":800,"over_in_use_bytes":0,"evicted_entries":0,"evicted_bytes":1,"evict_ms":0}`
	data := []byte(first + "\n" + second + "\n")
	wantCounts := ciGocacheprogStatsWant(ciGocacheprogEvictOne, 1, 1)
	wantStore := map[string]int64{"evictions": 1, "evicted_entries": 0, "evicted_bytes": 9223372036854775807, "evict_ms": 0, "entries": 9, "claim_bytes_before": 1000, "young_bytes": 300, "cap_bytes": 800, "over_in_use_bytes": 200}
	got, store := sumGocacheprogStats(data)
	if !maps.Equal(got, wantCounts) {
		t.Fatalf("store overflow pair summed counts to %v; want %v", got, wantCounts)
	}
	if !maps.Equal(store, wantStore) {
		t.Fatalf("store overflow pair gave store %v; want %v", store, wantStore)
	}
}

// ciGocacheprogReadWith runs one read against a fake exec that answers reply, returning the read's result and the exec calls.
func ciGocacheprogReadWith(reply cliReply) ([]byte, bool, []cliCall, error) {
	cli := newFakeCLIFor(func(cliCall) cliReply { return reply })
	data, present, err := (&Daemon{execCmd: cli.run}).ciGocacheprogRead(context.Background(), ciCacheExecTarget{runner: ciCacheGateRunner, kubeconfig: "kubeconfig", kubeContext: ciCacheTestContext, pod: ciCacheTestPod})
	return data, present, cli.recorded(), err
}

func TestCIGocacheprogReadPresentData(t *testing.T) {
	t.Parallel()
	root := ciCacheVolumeRoot(ciGocacheprogVolume)
	data, present, calls, err := ciGocacheprogReadWith(cliReply{stdout: ciGocacheprogStatsLine + "\n"})
	if err != nil || !present || string(data) != ciGocacheprogStatsLine+"\n" {
		t.Fatalf("read = %q, %v, %v; want the file back, true, nil", data, present, err)
	}
	if len(calls) != 1 || !slices.Contains(calls[0].args, root+"/stats.jsonl") {
		t.Fatalf("execs %v; want one exec naming %s/stats.jsonl", calls, root)
	}
	for _, arg := range calls[0].args {
		if strings.HasPrefix(arg, "/") && !strings.HasPrefix(arg, root+"/") {
			t.Fatalf("exec names %q, outside the volume root %s", arg, root)
		}
	}
	data, present, _, err = ciGocacheprogReadWith(cliReply{stdout: strings.Repeat("x", ciGocacheprogStatsMaxBytes)})
	if err != nil || !present || len(data) != ciGocacheprogStatsMaxBytes {
		t.Fatalf("a reply at the limit gave %d bytes, %v, %v; want all of it, true, nil", len(data), present, err)
	}
}

func TestCIGocacheprogReadAbsentIsClean(t *testing.T) {
	t.Parallel()
	data, present, _, err := ciGocacheprogReadWith(cliReply{code: 3, stderr: "stats file not found\n"})
	if err != nil || present || data != nil {
		t.Fatalf("absent volume or file = %q, %v, %v; want nil, false, nil", data, present, err)
	}
}

func TestCIGocacheprogReadRefusals(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		reply cliReply
		want  string
	}{
		{"an oversize reply", cliReply{stdout: strings.Repeat("x", ciGocacheprogStatsMaxBytes+1)}, strconv.Itoa(ciGocacheprogStatsMaxBytes)},
		{"a failed exec", cliReply{code: 1, stderr: "first line\nmv: cannot stat\ncommand terminated with exit code 1\n"}, "mv: cannot stat"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			data, present, _, err := ciGocacheprogReadWith(row.reply)
			switch {
			case err == nil:
				t.Fatalf("accepted %s: %d bytes", row.name, len(data))
			case !present:
				t.Fatalf("%s = present false with error %v; only an absent volume or file is a clean absence", row.name, err)
			case !strings.Contains(err.Error(), row.want):
				t.Fatalf("%s error %q; want it to name %q", row.name, err, row.want)
			}
		})
	}
}
