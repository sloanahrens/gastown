package progress

import (
	"errors"
	"strings"
	"time"
)

// ErrOutsideTown is returned by TranscriptTail for a transcript the containment
// check refuses: outside the town and the seat's own Claude projects
// directories, or reached through a symlink that leaves them.
var ErrOutsideTown = errors.New("transcript is outside the town")

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
