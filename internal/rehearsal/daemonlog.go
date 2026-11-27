// Package rehearsal checks a rehearsal run against the daemon log for the
// window the run covered.
package rehearsal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// stamp is log.LstdFlags as the daemon's logger writes it: a wall clock with
// no zone.
const stamp = "2006/01/02 15:04:05"

// maxLineBytes is the most of a line FailedCalls returns.
const maxLineBytes = 300

// failureMarkers are the substrings that show a Dolt, bd, gt or gateway call
// failed.
var failureMarkers = []string{
	"i/o timeout",
	"invalid connection",
	"http2 error",
	"service is currently unavailable",
	"did not answer",
	"signal: terminated",
}

// FailedCalls returns the daemon log lines in [start, end) that show a failed
// call. Each returned line is cut to 300 bytes, and a message that repeats an
// earlier one is returned once, at its first line. A line counts only if it
// begins with a stamp read in start's location; a line without one continues
// the entry above it and is skipped.
//
// A read error returns no lines. So does a window with no stamped line inside
// it: the log did not cover the window, and an empty list would read as a
// quiet one.
func FailedCalls(log io.Reader, start, end time.Time) ([]string, error) {
	loc := start.Location()
	var got []string
	seen := make(map[string]bool)
	covered := false
	r := bufio.NewReader(log)
	for {
		raw, err := r.ReadString('\n')
		line := strings.TrimRight(raw, "\r\n")
		if at, text, ok := splitStamp(line, loc); ok && !at.Before(start) && at.Before(end) {
			covered = true
			if hasFailureMarker(line) {
				key := cut(text)
				if !seen[key] {
					seen[key] = true
					got = append(got, cut(line))
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read daemon log: %w", err)
		}
	}
	if !covered {
		return nil, fmt.Errorf("daemon log has no stamped line in [%s, %s): the log does not cover the window",
			start.Format(stamp), end.Format(stamp))
	}
	return got, nil
}

// splitStamp reads the stamp that begins line, in loc, and returns its instant
// and the text after it. ok is false when line does not begin with a stamp.
func splitStamp(line string, loc *time.Location) (at time.Time, text string, ok bool) {
	if len(line) < len(stamp) {
		return time.Time{}, "", false
	}
	at, err := time.ParseInLocation(stamp, line[:len(stamp)], loc)
	if err != nil {
		return time.Time{}, "", false
	}
	return at, strings.TrimPrefix(line[len(stamp):], " "), true
}

// hasFailureMarker reports whether line contains one of the failure markers.
func hasFailureMarker(line string) bool {
	for _, marker := range failureMarkers {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// cut returns s cut to maxLineBytes. It backs off to the start of a rune when
// the cut would split one, so the result is valid UTF-8 and never longer than
// the limit.
func cut(s string) string {
	if len(s) <= maxLineBytes {
		return s
	}
	n := maxLineBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
