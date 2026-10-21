package progress

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// ErrOutsideTown is returned by TranscriptTail for a transcript the containment
// check refuses: outside the town and the seat's own Claude projects
// directories, or reached through a symlink that leaves them.
var ErrOutsideTown = errors.New("transcript is outside the town")

// startScanLines bounds how many lines TranscriptStart reads looking for the
// first timestamp.
const startScanLines = 100

// TranscriptStart returns the time of the first timestamped line of the
// transcript at path, read through the same containment check as
// TranscriptTail. It returns the zero time when none of the first lines carries
// a timestamp.
func TranscriptStart(townRoot, worktree, path string) (time.Time, error) {
	p := &Pass{TownRoot: townRoot}
	resolved, reason := p.transcriptPath(Nudge{Transcript: path, Worktree: worktree})
	switch reason {
	case "":
	case ReasonOutsideTown:
		return time.Time{}, ErrOutsideTown
	default:
		return time.Time{}, errors.New("transcript cannot be read: " + reason)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReader(f)
	for i := 0; i < startScanLines; i++ {
		line, err := r.ReadBytes('\n')
		var rec struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &rec) == nil {
			if ts, perr := time.Parse(time.RFC3339Nano, rec.Timestamp); perr == nil {
				return ts, nil
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return time.Time{}, err
		}
	}
	return time.Time{}, nil
}

// TranscriptTail returns the last maxRunes runes of the activity in the
// transcript at path, the seat at worktree's transcript. It reads through the
// same containment check the progress pass uses, so a battery that reads a
// transcript through this function cannot open one the progress pass would
// refuse. The text holds each assistant line, tool call and tool result in
// order, scrubbed of credential-shaped text by the reader.
func TranscriptTail(townRoot, worktree, path string, maxRunes int) (string, error) {
	p := &Pass{TownRoot: townRoot}
	resolved, reason := p.transcriptPath(Nudge{Transcript: path, Worktree: worktree})
	switch reason {
	case "":
	case ReasonOutsideTown:
		return "", ErrOutsideTown
	default:
		return "", errors.New("transcript cannot be read: " + reason)
	}
	events, _, err := readWindow(resolved, time.Time{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, ev := range events {
		switch ev.Kind {
		case "tool_call":
			b.WriteString("[tool " + ev.Name + "] ")
		case "tool_result":
			b.WriteString("[result] ")
		}
		b.WriteString(ev.Text)
		b.WriteByte('\n')
	}
	return lastRunes(b.String(), maxRunes), nil
}

// lastRunes returns the last n runes of s, or all of s when it is shorter.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
