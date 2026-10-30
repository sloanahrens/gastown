package testpolicy

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// SweepHostEntry is one line of sweep-host.txt: a package the sweep runner
// leaves to the host sweep, and the reason it cannot run in the container.
type SweepHostEntry struct {
	Package string
	Reason  string
	Line    int // 1-based line in the file, so a finding names the line to fix
}

// ReadSweepHost reads sweep-host.txt, whose absence is an error rather than an
// empty list: a missing manifest would classify every package as a runner one.
func ReadSweepHost(path string) ([]SweepHostEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSweepHost(strings.Split(string(data), "\n")), nil
}

// ParseSweepHost turns the file's lines into entries. A blank line is skipped,
// and so is a whole-line comment: it cuts to an empty package. The reason is
// the text after the first '#', and a line without one has an empty reason,
// which CheckSweepHostReasons reports.
func ParseSweepHost(lines []string) []SweepHostEntry {
	var entries []SweepHostEntry
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		pkg, reason, hasReason := strings.Cut(trimmed, "#")
		entry := SweepHostEntry{Package: strings.TrimSpace(pkg), Line: i + 1}
		if entry.Package == "" {
			continue
		}
		if hasReason {
			entry.Reason = strings.TrimSpace(reason)
		}
		entries = append(entries, entry)
	}
	return entries
}

// CheckSweepHostPackages reports each manifest line that names a package
// integration does not hold: the integration tag adds no file to it, so the
// sweep never runs it and its line is stale or misspelled. The finding names
// the line number, so the fix is the line.
func CheckSweepHostPackages(entries []SweepHostEntry, integration []string) []string {
	known := make(map[string]bool, len(integration))
	for _, p := range integration {
		known[p] = true
	}
	var findings []string
	for _, e := range entries {
		if !known[e.Package] {
			findings = append(findings, fmt.Sprintf("sweep-host.txt:%d names %s, which no file with the integration tag belongs to: delete its line", e.Line, e.Package))
		}
	}
	return findings
}

// CheckSweepHostReasons reports each manifest line with no reason after its
// '#', since the reason is what tells the next reader whether the line is
// still owed a container run.
func CheckSweepHostReasons(entries []SweepHostEntry) []string {
	var findings []string
	for _, e := range entries {
		if e.Reason == "" {
			findings = append(findings, fmt.Sprintf("sweep-host.txt:%d names %s without a '# reason': say why the runner cannot run it", e.Line, e.Package))
		}
	}
	return findings
}

// SweepRunnerPackages returns the packages the sweep runner runs: every
// package in integration that host does not name, sorted. The runner set and
// host are disjoint and together are the discovered set, so no package is
// both run in the container and left to the host.
func SweepRunnerPackages(integration []string, host []SweepHostEntry) []string {
	listed := make(map[string]bool, len(host))
	for _, e := range host {
		listed[e.Package] = true
	}
	runner := make([]string, 0, len(integration))
	for _, p := range integration {
		if !listed[p] {
			runner = append(runner, p)
		}
	}
	sort.Strings(runner)
	return runner
}

// CheckSweepHostCount reports a manifest whose entry count differs from pin;
// before changing either, read maxSweepHost in sweephost_test.go.
func CheckSweepHostCount(entries, pin int) []string {
	switch {
	case entries > pin:
		return []string{fmt.Sprintf("sweep-host.txt has %d entries, above its %d pin: this list only shrinks, so a line must not be added; a package the runner cannot run needs its own bead", entries, pin)}
	case entries < pin:
		return []string{fmt.Sprintf("sweep-host.txt has %d entries, below its %d pin: lower maxSweepHost in sweephost_test.go in the same change", entries, pin)}
	}
	return nil
}
