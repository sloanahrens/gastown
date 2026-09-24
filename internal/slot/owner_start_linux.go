//go:build linux

package slot

import (
	"os"
	"strconv"
	"strings"
)

// processStartToken returns pid's start time in clock ticks since boot, field
// 22 of /proc/<pid>/stat. It never changes for the life of a process, so a
// live pid whose token differs from the one recorded at container creation is
// a different process that reused the pid. ok is false when it cannot be read.
func processStartToken(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	// Field 2 (comm) is parenthesized and may contain spaces, so count fields
	// from after its closing paren: field 3 is the first one there.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(s[i+1:])
	const startTimeIdx = 22 - 3
	if len(fields) <= startTimeIdx || fields[startTimeIdx] == "" {
		return "", false
	}
	return fields[startTimeIdx], true
}
