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
// e.g. 301s) and why it belongs in the slow tier. The gate fails any
// fast-tier package whose wall time exceeds FastTierMaxWall, so the boundary
// cannot drift silently.

// FastTierMaxWall is the most wall time one fast-tier package may take in
// `make gate` before the gate fails it.
const FastTierMaxWall = 15 * time.Second

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

// ParsePackageWalls reads plain `go test` output and returns the wall time of
// every package that ran, from its "ok" or "FAIL" summary line
// ("ok  \t<import path>\t1.234s", optionally followed by coverage). Packages
// served from the result cache ("(cached)") ran nothing and are left out, as
// are lines that are not package summaries. Package names are relative to
// module.
func ParsePackageWalls(r io.Reader, module string) ([]PackageWall, error) {
	var walls []PackageWall
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
		wall, err := time.ParseDuration(strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		pkg := strings.TrimSpace(fields[1])
		if pkg != module && !strings.HasPrefix(pkg, module+"/") {
			continue
		}
		pkg = strings.TrimPrefix(strings.TrimPrefix(pkg, module), "/")
		walls = append(walls, PackageWall{Package: pkg, Wall: wall})
	}
	return walls, sc.Err()
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
