package cmd

import (
	"fmt"
	"os"
	"strings"
)

// processStart returns an opaque token for when pid started, or "" when it
// cannot be read. Two reads of the same process agree; a process that later
// reuses the PID reads differently. It is read in-process, so the capacity
// scan spawns nothing.
func processStart(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// The command name in field 2 is parenthesised and may hold spaces or
	// parens, so count fields from the last ')': the remainder starts at
	// field 3 (state), and starttime is field 22.
	stat := string(data)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
