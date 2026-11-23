// Package seatship carries the record a finished seat posts as one bead comment.
package seatship

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// CommentPrefix opens the comment a finished seat's record travels in.
const CommentPrefix = "SEAT-END v1 "

// MaxCommentBytes is the longest comment a seat posts.
const MaxCommentBytes = 96 << 10

// ErrTooLarge reports a payload too large for one comment even without its tail.
var ErrTooLarge = errors.New("seatship: payload too large for one comment")

// Payload is one finished seat's record.
type Payload struct {
	Rig            string                 `json:"rig"`
	Polecat        string                 `json:"polecat"`
	Bead           string                 `json:"bead"`
	Submitted      bool                   `json:"submitted"`
	Usage          *dashboard.UsageRecord `json:"usage,omitempty"`
	SessionIDs     []string               `json:"session_ids,omitempty"`
	Outbox         map[string]string      `json:"outbox,omitempty"`
	TranscriptTail string                 `json:"transcript_tail,omitempty"`
}

// Encode returns the comment that carries p, dropping the transcript tail when
// the comment would exceed MaxCommentBytes; one still over it is ErrTooLarge.
func Encode(p Payload) (string, error) {
	for _, dropTail := range []bool{false, true} {
		if dropTail {
			p.TranscriptTail = ""
		}
		b, err := json.Marshal(p)
		if err != nil {
			return "", err
		}
		if comment := CommentPrefix + string(b); len(comment) <= MaxCommentBytes {
			return comment, nil
		}
	}
	return "", ErrTooLarge
}

// Decode reads the payload a SEAT-END comment carries; found is false for
// another writer's comment, and an unreadable or bead-less one is an error.
func Decode(comment string) (p Payload, found bool, err error) {
	if !strings.HasPrefix(comment, CommentPrefix) {
		return Payload{}, false, nil
	}
	if err := json.Unmarshal([]byte(comment[len(CommentPrefix):]), &p); err != nil {
		return Payload{}, false, err
	}
	if p.Bead == "" {
		return Payload{}, false, errors.New("seatship: comment names no bead")
	}
	return p, true, nil
}
