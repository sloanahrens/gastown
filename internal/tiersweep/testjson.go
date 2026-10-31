package tiersweep

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// testEvent is one line of a go test -json stream, as cmd/test2json writes it.
// Only the fields the parser reads are declared.
type testEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// TestTime is a top-level test's terminal event: its package and name, the
// seconds it took, and whether it passed, failed or was skipped. A subtest is
// not listed; its time is inside the Elapsed of the test that holds it.
type TestTime struct {
	Test    string
	Package string
	Elapsed float64
	Action  string
}

// ParseTestJSON reads a go test -json stream and returns the plain log text the
// same run would print, with the terminal event of each top-level test in
// stream order. The text is the output events concatenated in stream order, so
// the FAIL and ok lines and any panic text survive. A line that is not a stream
// event is kept in the text unchanged and does not stop the parse. A read error
// is returned as is, with no text and no tests.
// A JSON object line that is not a well-formed event is the one line that stops
// the parse: it is an error with no text and no tests, so its text is never
// echoed and a bad event never reads as a clean run.
func ParseTestJSON(r io.Reader) (text string, tests []TestTime, err error) {
	var out strings.Builder
	br := bufio.NewReader(r)
	lineNo := 0
	for {
		line, readErr := br.ReadString('\n')
		if line != "" {
			lineNo++
			if isJSONObject(line) && !isEvent(line) {
				return "", nil, fmt.Errorf("line %d: malformed go test -json event", lineNo)
			}
			var ev testEvent
			if json.Unmarshal([]byte(line), &ev) != nil || ev.Action == "" {
				out.WriteString(line)
			} else {
				out.WriteString(ev.Output)
				if isTopLevelTerminal(ev) {
					tests = append(tests, TestTime{
						Test:    ev.Test,
						Package: ev.Package,
						Elapsed: ev.Elapsed,
						Action:  ev.Action,
					})
				}
			}
		}
		if readErr == io.EOF {
			return out.String(), tests, nil
		}
		if readErr != nil {
			return "", nil, readErr
		}
	}
}

// isJSONObject reports whether a line is one complete JSON object, the envelope
// every stream event is written in.
func isJSONObject(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "{") && json.Valid([]byte(t))
}

// isEvent reports whether a line decodes as a stream event: its fields have the
// types testEvent declares, and it names an action.
func isEvent(line string) bool {
	var ev testEvent
	return json.Unmarshal([]byte(line), &ev) == nil && ev.Action != ""
}

// isTopLevelTerminal reports whether an event ends a top-level test: a pass,
// fail or skip whose test name has no slash, since go test names a subtest
// Parent/child.
func isTopLevelTerminal(ev testEvent) bool {
	if ev.Test == "" || strings.Contains(ev.Test, "/") {
		return false
	}
	switch ev.Action {
	case "pass", "fail", "skip":
		return true
	}
	return false
}
