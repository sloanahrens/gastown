package progress

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/report"
)

const (
	// transcriptWindow is how far back from the nudge the state reaches.
	transcriptWindow = 15 * time.Minute

	// tailBytes bounds how much of the transcript the pass reads: the end of
	// the file, where the window is. A window longer than this is cut at the
	// start, and the state marks it with WindowClipped.
	tailBytes = 8 << 20

	// fieldRunes caps each tool call's input and each tool result. Assistant
	// text is not capped.
	fieldRunes = 400

	// maxStateTokens caps the state Jev reads.
	maxStateTokens = 20000

	// bytesPerToken is the estimate the cap is taken in: a JSON state of n
	// bytes is about n/4 tokens.
	bytesPerToken = 4
)

// Event is one thing the polecat did or said in the window: its text, a tool
// call, or a tool result.
type Event struct {
	TS   time.Time `json:"ts"`
	Kind string    `json:"kind"`
	// Name is the tool a tool call names.
	Name string `json:"name,omitempty"`
	// Text is the assistant's text, a tool call's input or a tool result.
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}

// State is what the battery reads: the bead's title and acceptance, then the
// window's events, oldest first. The newest events survive the cap.
type State struct {
	BeadTitle      string `json:"bead_title"`
	BeadAcceptance string `json:"bead_acceptance"`
	WindowMinutes  int    `json:"window_minutes"`
	// WindowClipped is true when the window reaches back past the tail the
	// pass read, so events from its start are missing.
	WindowClipped bool    `json:"window_clipped"`
	EventsDropped int     `json:"events_dropped"`
	Events        []Event `json:"events"`
}

// transcriptBlock is one content block of a transcript line. Only the fields
// the battery reads are decoded.
type transcriptBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// readWindow returns the events in the Claude Code transcript at path from
// since on, oldest first, and whether the window reaches back past the tail
// it read. It reads the tail of the file only. A line that does not parse, or
// that has no timestamp, is skipped.
func readWindow(path string, since time.Time) ([]Event, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	var start int64
	if fi.Size() > tailBytes {
		start = fi.Size() - tailBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	r := bufio.NewReader(f)
	// probe is set when the tail was cut. The first timestamped line in it
	// decides whether the window starts before the cut: transcript times only
	// grow, so if that line is still inside the window, the lines before the
	// seek are too.
	probe := start > 0
	clipped := false
	if probe {
		// The seek landed mid-line: that partial line is not the window's.
		if _, err := r.ReadBytes('\n'); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
	}
	var out []Event
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if ts, ok := lineTime(line); ok && probe {
				probe = false
				clipped = !ts.Before(since)
			}
			out = append(out, parseLine(line, since)...)
		}
		if errors.Is(err, io.EOF) {
			// A tail with no timestamped line, one long line for instance, cannot
			// show where the window starts, so it is marked as clipped.
			return out, clipped || probe, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
}

// lineTime returns the timestamp of one transcript line, and false when the
// line does not parse or has none.
func lineTime(line []byte) (time.Time, bool) {
	var tl struct {
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(line, &tl) != nil {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
	return ts, err == nil
}

// parseLine returns the events one transcript line holds, if it is inside the
// window. A user line's text and a system line are not events.
func parseLine(line []byte, since time.Time) []Event {
	var tl struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &tl) != nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
	if err != nil || ts.Before(since) {
		return nil
	}
	// A user's plain prompt is a string, not blocks, and fails this decode.
	var blocks []transcriptBlock
	if json.Unmarshal(tl.Message.Content, &blocks) != nil {
		return nil
	}
	var out []Event
	for _, b := range blocks {
		switch {
		case tl.Type == "assistant" && b.Type == "text":
			out = append(out, Event{TS: ts, Kind: "assistant_text", Text: report.Scrub(b.Text)})
		case tl.Type == "assistant" && b.Type == "tool_use":
			out = append(out, Event{TS: ts, Kind: "tool_call", Name: b.Name, Text: clip(string(b.Input))})
		case tl.Type == "user" && b.Type == "tool_result":
			out = append(out, Event{TS: ts, Kind: "tool_result", Text: clip(resultText(b.Content)), IsError: b.IsError})
		}
	}
	return out
}

// resultText is a tool result's text. The content is a string, or a list of
// text blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []transcriptBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out []byte
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, b.Text...)
	}
	return string(out)
}

// clip scrubs s of credential-shaped text and caps it at fieldRunes. The scrub
// runs first, so a secret that straddles the cut is still removed.
func clip(s string) string {
	return truncate(report.Scrub(s), fieldRunes)
}

// buildState returns the state for one nudge: the bead's title and acceptance,
// and the window's events. When the state would pass the token cap, the oldest
// events are dropped until it fits.
func buildState(issue *beads.Issue, events []Event, clipped bool) State {
	st := State{
		BeadTitle:      truncate(issue.Title, truncateRunes),
		BeadAcceptance: truncate(land.CriteriaText(issue), truncateRunes),
		WindowMinutes:  int(transcriptWindow / time.Minute),
		WindowClipped:  clipped,
		Events:         events,
	}
	if st.Events == nil {
		st.Events = []Event{}
	}
	return fitState(st)
}

// fitState keeps the newest events that fit the token cap and counts the rest
// as dropped. Each event is measured once, so the cut is linear in the events.
// The size is an estimate: the JSON bytes, with one byte for each separator.
func fitState(st State) State {
	budget := maxStateTokens * bytesPerToken
	events := st.Events
	st.Events = []Event{}
	st.EventsDropped = len(events) // the widest count the field can hold
	base, _ := json.Marshal(st)
	used := len(base)

	sizes := make([]int, len(events))
	for i, ev := range events {
		b, _ := json.Marshal(ev)
		sizes[i] = len(b) + 1
	}
	first := len(events)
	for first > 0 && used+sizes[first-1] <= budget {
		used += sizes[first-1]
		first--
	}
	st.Events = events[first:]
	st.EventsDropped = first
	return st
}
