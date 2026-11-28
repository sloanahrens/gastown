package daemon

import (
	"maps"
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

func TestCIGocacheprogSumAddsGoodLines(t *testing.T) {
	t.Parallel()
	second := `{"gets": 5,"hits":5,"misses":0,"puts":0,"put_bytes":0,"hit_bytes":900,"errors":1,"start":"2026-11-28T00:05:00Z","elapsed_ms":250,"evicted_bytes":7}`
	data := []byte(ciGocacheprogStatsLine + "\n" + second + "\n")
	want := ciGocacheprogStatsWant(map[string]int64{"gets": 8, "hits": 7, "misses": 1, "puts": 4, "put_bytes": 400, "hit_bytes": 1100, "errors": 1, "elapsed_ms": 1750}, 2, 0)
	if got := sumGocacheprogStats(data); !maps.Equal(got, want) {
		t.Fatalf("two good lines summed to %v; want %v", got, want)
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
			if got := sumGocacheprogStats(row.data); !maps.Equal(got, row.want) {
				t.Fatalf("%q summed to %v; want %v", row.data, got, row.want)
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
	if got := sumGocacheprogStats(data); !maps.Equal(got, want) {
		t.Fatalf("overflowing pair summed to %v; want %v", got, want)
	}
}
