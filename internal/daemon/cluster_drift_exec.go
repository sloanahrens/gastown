package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// clusterDriftCommand is the command whose one run this parse reads, named in
// every error it raises so the reader knows which answer was refused.
const clusterDriftCommand = "gt town cluster drift"

// The exit statuses a drift run answers with: 0 in step, 1 differing, 2 cannot
// say. Every other status is refused (gt-hhsi66, gt-b7p6jn).
const (
	clusterDriftExitInStep    = 0
	clusterDriftExitDiffers   = 1
	clusterDriftExitCannotSay = 2
)

// The words the command prints when no live render has recorded its flags, and
// the cap on the stderr line an exit 2 error carries (gt-b7p6jn).
const (
	clusterDriftRecordedFlagsWords = "no live render has recorded its flags"
	clusterDriftReasonMax          = 200
)

// parseClusterDriftRun reads one run of gt town cluster drift: its exit status,
// its stdout, its stderr and whether the child ran at all. Exit 0 is an empty
// result whatever stdout holds. Exit 1 is the non-empty stdout lines, kept as
// printed. Every other answer, a runErr or an exit 1 with no line is an error
// with no result (gt-hhsi66). Exit 2 is the cannot-say answer, and only it reads
// stderr (gt-b7p6jn).
func parseClusterDriftRun(exit int, stdout, stderr []byte, runErr error) (clusterDriftResult, error) {
	if runErr != nil {
		return clusterDriftResult{}, fmt.Errorf("running %s: %w", clusterDriftCommand, runErr)
	}
	switch exit {
	case clusterDriftExitInStep:
		return clusterDriftResult{}, nil
	case clusterDriftExitDiffers:
		lines := clusterDriftLines(stdout)
		if len(lines) == 0 {
			return clusterDriftResult{}, fmt.Errorf("%s exited 1 with no difference line, so no drift can be named", clusterDriftCommand)
		}
		return clusterDriftResult{Differing: lines}, nil
	case clusterDriftExitCannotSay:
		return clusterDriftResult{}, clusterDriftCannotSayError(stdout, stderr)
	default:
		return clusterDriftResult{}, fmt.Errorf("%s exited %d, which is neither in step (0), differing (1) nor cannot say (2)", clusterDriftCommand, exit)
	}
}

// clusterDriftCannotSayError is exit 2's error: the first stderr line, never
// stdout, which holds object values. It wraps errNoRecordedRender when either
// stream says no live render has recorded its flags (gt-b7p6jn).
func clusterDriftCannotSayError(stdout, stderr []byte) error {
	msg := fmt.Sprintf("%s exited %d, which cannot say whether the cluster is in step", clusterDriftCommand, clusterDriftExitCannotSay)
	if reason := clusterDriftFirstLine(stderr); reason != "" {
		msg += ": " + reason
	}
	words := []byte(clusterDriftRecordedFlagsWords)
	if bytes.Contains(stdout, words) || bytes.Contains(stderr, words) {
		return fmt.Errorf("%s: %w", msg, errNoRecordedRender)
	}
	return errors.New(msg)
}

// clusterDriftFirstLine is the first non-blank stderr line, cut to
// clusterDriftReasonMax runes so a multi-byte character is never split (gt-b7p6jn).
func clusterDriftFirstLine(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if utf8.RuneCountInString(line) > clusterDriftReasonMax {
				line = string([]rune(line)[:clusterDriftReasonMax])
			}
			return line
		}
	}
	return ""
}

// clusterDriftLines is the non-empty lines of stdout, kept exactly as printed:
// the command writes one line per difference, and a blank line names nothing.
func clusterDriftLines(stdout []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
