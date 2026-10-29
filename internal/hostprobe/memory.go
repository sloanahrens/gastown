// Package hostprobe reads the host the daemon runs on. Its parsers are shared
// by the daemon and by the host agent, so both read a tool's output one way.
package hostprobe

import (
	"fmt"
	"regexp"
	"strconv"
)

// memFreeLine is the line of memory_pressure's output that carries the figure.
var memFreeLine = regexp.MustCompile(`System-wide memory free percentage:\s*([0-9.]+)%`)

// ParseMemoryPressure reads the free-memory percentage from memory_pressure's
// output, and an output without the line is an error, never zero: a zero would
// read as a host out of memory.
func ParseMemoryPressure(out string) (float64, error) {
	m := memFreeLine.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no %q line in output", "System-wide memory free percentage")
	}
	return strconv.ParseFloat(m[1], 64)
}
