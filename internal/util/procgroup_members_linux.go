//go:build linux

package util

import (
	"os"
	"strconv"
	"strings"
)

// procGroupMembers lists the live (not zombie) processes whose process group is
// pgid, read from /proc. A zombie is already dead; kill(-pgid, 0) counts it as a
// member, /proc says so.
func procGroupMembers(pgid int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited between the listing and the read
		}
		group, state, ok := parseProcStat(string(data))
		if ok && group == pgid && state != 'Z' && state != 'X' {
			members = append(members, pid)
		}
	}
	return members, nil
}

// parseProcStat reads the process group and state out of a /proc/<pid>/stat
// line: "pid (comm) state ppid pgrp ...". comm may hold spaces and parentheses,
// so the fields are counted from the last ")".
func parseProcStat(line string) (pgrp int, state byte, ok bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(line[i+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return pgrp, fields[0][0], true
}
