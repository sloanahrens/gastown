package testpolicy

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// slow.txt splits the Go suite into two tiers (gt-z862q). `make gate`, the
// landing gate, runs every package NOT listed; `make test-slow` runs the
// listed ones after landing. Each line is "<package> <wall> # <why>": the
// wall the package took in the measurement that put it there (a Go duration,
// e.g. 301s) and why it belongs in the slow tier. `make tier-check` fails any
// fast-tier package whose wall time exceeds FastTierMaxWall, so the boundary
// cannot drift silently; `make gate` only warns, because wall time depends on
// host load and a landing must not be refused for contention.

// FastTierMaxWall is the one definition of the tier boundary: the most wall
// time one fast-tier package may take before `make tier-check` fails it (and
// `make gate` warns), and the cut-off for slow.txt (every listed package measured at least
// this much). The budget runner's -fast-tier flag reads it; nothing else
// restates the number.
const FastTierMaxWall = 30 * time.Second

// SlowEntry is one line of slow.txt.
type SlowEntry struct {
	Package string
	Wall    time.Duration
	Why     string
	Line    int
}

// ParseSlowList reads slow.txt's entries. Every entry needs a package, its
// measured wall time and a justification after "#".
func ParseSlowList(r io.Reader) ([]SlowEntry, error) {
	var entries []SlowEntry
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, why, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		why = strings.TrimSpace(why)
		if len(fields) != 2 || why == "" {
			return nil, fmt.Errorf("slow.txt:%d: want \"<package> <measured wall> # <why it is slow>\", got %q", n, sc.Text())
		}
		wall, err := time.ParseDuration(fields[1])
		if err != nil || wall <= 0 {
			return nil, fmt.Errorf("slow.txt:%d: %q is not a measured wall time (want a Go duration such as 45s)", n, fields[1])
		}
		if first, dup := seen[fields[0]]; dup {
			return nil, fmt.Errorf("slow.txt:%d: %s is listed twice (first on line %d)", n, fields[0], first)
		}
		seen[fields[0]] = n
		entries = append(entries, SlowEntry{Package: fields[0], Wall: wall, Why: why, Line: n})
	}
	return entries, sc.Err()
}

// ReadSlowList reads the slow.txt at path.
func ReadSlowList(path string) ([]SlowEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseSlowList(f)
}

// PackageWall is the wall time go test reported for one package.
type PackageWall struct {
	Package string
	Wall    time.Duration
}

// WallSummary is what ParsePackageWalls found in go test's output.
type WallSummary struct {
	// Walls are the packages that ran, with their wall times.
	Walls []PackageWall
	// Cached counts the packages served from the result cache: they ran
	// nothing, so they have no wall time.
	Cached int
	// Unparsed are summary lines of this module's packages whose time could
	// not be read. The caller must not treat them as fast.
	Unparsed []string
}

// ParsePackageWalls reads plain `go test` output and returns the wall time of
// every package that ran, from its "ok" or "FAIL" summary line
// ("ok  \t<import path>\t1.234s", optionally followed by
// " [no tests to run]" or a coverage field). Lines that are not package
// summaries of module are ignored. Package names are relative to module.
func ParsePackageWalls(r io.Reader, module string) (WallSummary, error) {
	var sum WallSummary
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 3 {
			continue
		}
		status := strings.TrimSpace(fields[0])
		if status != "ok" && status != "FAIL" {
			continue
		}
		pkg := strings.TrimSpace(fields[1])
		if pkg != module && !strings.HasPrefix(pkg, module+"/") {
			continue
		}
		pkg = strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		timeField := strings.Fields(fields[2])
		switch {
		case len(timeField) > 0 && timeField[0] == "(cached)":
			sum.Cached++
		case len(timeField) > 0:
			wall, err := time.ParseDuration(timeField[0])
			if err != nil {
				sum.Unparsed = append(sum.Unparsed, sc.Text())
				continue
			}
			sum.Walls = append(sum.Walls, PackageWall{Package: pkg, Wall: wall})
		default:
			sum.Unparsed = append(sum.Unparsed, sc.Text())
		}
	}
	return sum, sc.Err()
}

// WallOverruns returns the packages in walls that took longer than limit,
// slowest first.
func WallOverruns(walls []PackageWall, limit time.Duration) []PackageWall {
	var over []PackageWall
	for _, w := range walls {
		if w.Wall > limit {
			over = append(over, w)
		}
	}
	sort.SliceStable(over, func(i, j int) bool { return over[i].Wall > over[j].Wall })
	return over
}
