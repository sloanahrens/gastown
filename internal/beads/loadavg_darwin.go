//go:build darwin

package beads

import (
	"os/exec"
	"regexp"
	"strings"
)

// braceRe matches curly braces in sysctl output.
var braceRe = regexp.MustCompile(`[{}]`)

// getLoadavg returns the 1-minute load average on macOS.
// macOS sysctl kern.loadavg prints: kern.loadavg: { 1.23 2.34 3.45 }
// The numbers are wrapped in curly braces; strip them before parsing.
func getLoadavg() (float64, error) {
	out, err := exec.Command("sysctl", "kern.loadavg").Output()
	if err != nil {
		return -1, err
	}
	// Output format: "kern.loadavg: { 1.23 2.34 3.45 }\n"
	// Strip braces, then take the first number after ':'.
	trimmed := braceRe.ReplaceAllString(strings.TrimSpace(string(out)), "")
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return -1, nil
	}
	fields := strings.Fields(parts[1])
	if len(fields) == 0 {
		return -1, nil
	}
	return parseLoadavgField(fields[0])
}
