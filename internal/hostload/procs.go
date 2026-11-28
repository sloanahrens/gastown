package hostload

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var procLineRE = regexp.MustCompile(`^\s*(\S+)\s+(\S+)\s+(\S.*)$`)

// Proc is one process in ps output: its pid, cumulative CPU seconds and command (gt-efoycy).
type Proc struct {
	PID     int
	CPUTime float64
	Command string
}

// ParseProcs reads each line of `ps -axo pid=,time=,comm=` output as one
// process, keeping a command's inner spaces; a malformed line is refused.
func ParseProcs(text string) ([]Proc, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("ps text is empty")
	}
	var procs []Proc
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		p, err := parseProc(line)
		if err != nil {
			return nil, fmt.Errorf("ps line %d: %w", i+1, err)
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// parseProc reads one ps line, whose fields ps pads with runs of spaces.
func parseProc(line string) (Proc, error) {
	m := procLineRE.FindStringSubmatch(line)
	if m == nil {
		return Proc{}, errors.New("line has no pid, time and command")
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return Proc{}, errors.New("pid is not a number")
	}
	sec, err := procSeconds(m[2])
	if err != nil {
		return Proc{}, err
	}
	return Proc{PID: pid, CPUTime: sec, Command: m[3]}, nil
}

// procSeconds reads a ps time of minutes:seconds, refusing a negative or non-finite value.
func procSeconds(text string) (float64, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return 0, errors.New("time is not minutes:seconds")
	}
	minutes, mErr := strconv.ParseInt(parts[0], 10, 64)
	seconds, sErr := strconv.ParseFloat(parts[1], 64)
	if mErr != nil || sErr != nil {
		return 0, errors.New("time is not minutes:seconds")
	}
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, errors.New("time's seconds are not finite")
	}
	if minutes < 0 || seconds < 0 {
		return 0, errors.New("time is negative")
	}
	return float64(minutes)*60 + seconds, nil
}
