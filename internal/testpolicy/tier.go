package testpolicy

import (
	"bufio"
	"io"
	"sort"
	"strings"
	"time"
)

// The unit tier is every package: `make gate` runs them all, and since
// gt-ik4a1.9 there is no slow tier beside it (internal/cmd and internal/beads,
// its last members, measured 12s and 11s). `make tier-check` fails any package
// whose wall time exceeds FastTierMaxWall, so a package that grows slow cannot
// do so silently; `make gate` only warns, because wall time depends on host
// load and a landing must not be refused for contention.

// FastTierMaxWall is the one definition of the per-package wall limit: the
// most wall time one unit-tier package may take before `make tier-check`
// fails it (and `make gate` warns). The budget runner's -fast-tier flag reads
// it; nothing else restates the number.
const FastTierMaxWall = 30 * time.Second

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
