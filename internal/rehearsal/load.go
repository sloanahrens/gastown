package rehearsal

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LoadSummary reads the host load sampler's lines, one per call as
// internal/hostload.Line writes them, and returns the load in [start, end), the
// window FailedCalls reads: the sample count, the largest one-minute load and
// that load's nearest-rank p90. A line that does not parse, a read error and a
// window with no sample are errors; an empty Load would read as an idle host.
func LoadSummary(r io.Reader, start, end time.Time) (Load, error) {
	var one []float64
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		at, load1, err := parseLoadLine(sc.Text())
		if err != nil {
			return Load{}, fmt.Errorf("host load line %d: %w", n, err)
		}
		if !at.Before(start) && at.Before(end) {
			one = append(one, load1)
		}
	}
	if err := sc.Err(); err != nil {
		return Load{}, fmt.Errorf("read host load: %w", err)
	}
	if len(one) == 0 {
		return Load{}, fmt.Errorf("host load has no sample in [%s, %s): the text does not cover the window",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	sort.Float64s(one)
	// The nearest-rank p90 is the ceil(0.9n)-th smallest sample, never above max.
	rank := (9*len(one) + 9) / 10
	return Load{Samples: len(one), Max1: one[len(one)-1], P90One: one[rank-1]}, nil
}

// parseLoadLine reads one sampler line: an RFC3339 time, then load1, load5,
// load15 and ncpu as key=value fields.
func parseLoadLine(line string) (time.Time, float64, error) {
	f := strings.Fields(line)
	if len(f) != 5 {
		return time.Time{}, 0, fmt.Errorf("host load line %q is not a time and four fields", cut(line))
	}
	at, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("host load line %q has no RFC3339 time", cut(line))
	}
	var loads [3]float64
	for i, key := range [3]string{"load1=", "load5=", "load15="} {
		if loads[i], err = loadField(f[i+1], key, line); err != nil {
			return time.Time{}, 0, err
		}
	}
	ncpu, ok := strings.CutPrefix(f[4], "ncpu=")
	if _, err := strconv.ParseUint(ncpu, 10, 16); !ok || err != nil {
		return time.Time{}, 0, fmt.Errorf("host load line %q has no ncpu=<count> field", cut(line))
	}
	return at, loads[0], nil
}

// loadField returns the number after key in field, which must hold key and a
// value a sampler can write: finite, not negative and not NaN.
func loadField(field, key, line string) (float64, error) {
	text, ok := strings.CutPrefix(field, key)
	v, err := strconv.ParseFloat(text, 64)
	if !ok || err != nil || !finite(v) {
		return 0, fmt.Errorf("host load line %q has no %s<float> field", cut(line), key)
	}
	return v, nil
}

// finite reports whether v is a load average: not an infinity, and not
// negative. NaN fails v >= 0 too.
func finite(v float64) bool { return !math.IsInf(v, 0) && v >= 0 }
