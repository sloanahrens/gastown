// Package dockerstats parses the JSON line `docker stats --no-stream --format
// '{{json .}}'` prints for one container (gt-8kreef).
package dockerstats

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

// Stats is one container's CPU and memory reading from a docker stats line.
type Stats struct {
	// CPUPct is docker's percentage of one CPU, so 349.74 means about three and
	// a half cores.
	CPUPct float64
	// MemBytes is the memory in use. It is the printed figure times its unit,
	// rounded to the nearest byte: docker prints four significant digits, so a
	// value like 7.989GiB is not the exact cgroup byte count.
	MemBytes int64
	// MemLimitBytes is the memory limit in bytes.
	MemLimitBytes int64
}

var (
	// ErrNotJSON reports a line that is not a docker stats JSON object.
	ErrNotJSON = errors.New("line is not a docker stats JSON object")
	// ErrMissingField reports a line without CPUPerc or MemUsage.
	ErrMissingField = errors.New("docker stats line is missing a field")
	// ErrBadPercent reports a CPUPerc that is not a finite, non-negative
	// percentage.
	ErrBadPercent = errors.New("CPU percent does not parse")
	// ErrBadSize reports a memory size that is not a number with a unit.
	ErrBadSize = errors.New("memory size does not parse")
	// ErrUnknownUnit reports a memory unit outside the ones docker prints.
	ErrUnknownUnit = errors.New("memory size unit is unknown")
)

// units are the byte multipliers of the sizes docker prints; the SI units are
// powers of ten and the IEC ones powers of two.
var units = map[string]float64{
	"B": 1, "kB": 1e3, "KiB": 1 << 10,
	"MB": 1e6, "MiB": 1 << 20,
	"GB": 1e9, "GiB": 1 << 30,
	"TB": 1e12, "TiB": 1 << 40,
}

// Parse reads the JSON line `docker stats --no-stream --format '{{json .}}'`
// prints for one container, refusing any field it cannot read rather than
// reading it as zero.
func Parse(line string) (Stats, error) {
	var raw struct {
		CPUPerc  string
		MemUsage string
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return Stats{}, ErrNotJSON
	}
	if raw.CPUPerc == "" || raw.MemUsage == "" {
		return Stats{}, ErrMissingField
	}
	cpu, err := parsePercent(raw.CPUPerc)
	if err != nil {
		return Stats{}, err
	}
	used, limit, err := parseUsage(raw.MemUsage)
	if err != nil {
		return Stats{}, err
	}
	return Stats{CPUPct: cpu, MemBytes: used, MemLimitBytes: limit}, nil
}

func parsePercent(s string) (float64, error) {
	n, ok := strings.CutSuffix(s, "%")
	if !ok {
		return 0, ErrBadPercent
	}
	pct, err := strconv.ParseFloat(n, 64)
	if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return 0, ErrBadPercent
	}
	return pct, nil
}

func parseUsage(s string) (used, limit int64, err error) {
	left, right, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, ErrBadSize
	}
	if used, err = parseSize(left); err != nil {
		return 0, 0, err
	}
	if limit, err = parseSize(right); err != nil {
		return 0, 0, err
	}
	return used, limit, nil
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r == '.')
	})
	if i <= 0 {
		return 0, ErrBadSize
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, ErrBadSize
	}
	mult, ok := units[s[i:]]
	if !ok {
		return 0, ErrUnknownUnit
	}
	if n*mult >= math.MaxInt64 {
		return 0, ErrBadSize
	}
	return int64(math.Round(n * mult)), nil
}
