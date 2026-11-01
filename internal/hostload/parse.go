package hostload

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// ParsePS reads `ps -axo pid=,command=` output: one "pid command" line per
// process, with the command line kept whole so a process is classified by what
// it runs rather than by the name top may cut short. A line that is neither
// blank nor "pid command" is an error, so a ps that printed something else is
// not read as a short process table.
func ParsePS(out string) (map[int]string, error) {
	cmds := make(map[int]string)
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.SplitN(line, " ", 2)
		pid, err := strconv.Atoi(f[0])
		if err != nil || len(f) < 2 {
			return nil, fmt.Errorf("ps line %d: %q is not \"pid command\"", i+1, line)
		}
		cmds[pid] = strings.TrimSpace(f[1])
	}
	return cmds, nil
}

// ParseTop reads the CPU of every process from the last process table in
// top's output: a two-sample `top -l 2` prints each process's average since it
// started in the first table and its CPU over the sample window in the second.
// The columns come from the header, so a top whose fields the user changed is
// read by where PID and %CPU sit, and a table holding neither is refused
// rather than guessed at.
func ParseTop(out string) (map[int]float64, error) {
	lines := strings.Split(out, "\n")
	start, pidCol, cpuCol := -1, -1, -1
	for i, line := range lines {
		f := strings.Fields(line)
		if p := slices.Index(f, "PID"); p >= 0 {
			if c := slices.Index(f, "%CPU"); c >= 0 {
				start, pidCol, cpuCol = i, p, c
			}
		}
	}
	if start < 0 {
		return nil, errors.New("top printed no process table with PID and %CPU columns")
	}
	cpu := make(map[int]float64)
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		f := strings.Fields(line)
		if len(f) <= pidCol || len(f) <= cpuCol {
			return nil, fmt.Errorf("top: %q is shorter than its header", line)
		}
		pid, err := strconv.Atoi(f[pidCol])
		if err != nil {
			return nil, fmt.Errorf("top: %q has no pid in column %d", line, pidCol+1)
		}
		// A locale with a decimal comma prints 38,2.
		pct, err := strconv.ParseFloat(strings.ReplaceAll(f[cpuCol], ",", "."), 64)
		if err != nil || math.IsNaN(pct) || math.IsInf(pct, 0) {
			return nil, fmt.Errorf("top: %q has no CPU number in column %d", line, cpuCol+1)
		}
		cpu[pid] = pct
	}
	if len(cpu) == 0 {
		return nil, errors.New("top's process table held no rows")
	}
	return cpu, nil
}

// ParseLsof reads `lsof -a -d cwd -Fpn` output — a "p<pid>" record followed by
// the "n<path>" working directories lsof could read for it — into pid to
// working directory. A line that is neither is an error: a lsof that printed
// something else did not answer the question.
func ParseLsof(out string) (map[int]string, error) {
	cwds := make(map[int]string)
	pid := -1
	for i, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(line[1:])
			if err != nil {
				return nil, fmt.Errorf("lsof line %d: %q does not name a pid", i+1, line)
			}
			pid = n
		case 'n':
			if pid < 0 {
				return nil, fmt.Errorf("lsof line %d: %q names a path before any pid", i+1, line)
			}
			cwds[pid] = line[1:]
		default:
			return nil, fmt.Errorf("lsof line %d: %q is not a pid or a path", i+1, line)
		}
	}
	return cwds, nil
}
