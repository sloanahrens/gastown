package liveness

import (
	"bytes"
	"encoding/json"
	"time"
)

// StreamCutTail reports whether the last complete user-or-assistant record of a
// transcript tail is an assistant record the harness cut short: isApiErrorMessage
// true and a non-empty streamCutCause. Partial and undecodable lines are skipped.
func StreamCutTail(tail []byte) bool {
	cut, _ := StreamCutAt(tail)
	return cut
}

// StreamCutAt is StreamCutTail that also returns the timestamp of the record it
// judged. A cut record whose timestamp is missing, not a string or not RFC 3339
// is still a cut, with a zero time: the caller reads its age as unknown. A
// record that is not a cut returns a zero time whatever its timestamp.
func StreamCutAt(tail []byte) (cut bool, at time.Time) {
	lines := bytes.Split(tail, []byte("\n"))
	lines = lines[:len(lines)-1] // the segment after the last newline is never a complete record
	for i := len(lines) - 1; i >= 0; i-- {
		var r struct {
			Type              string          `json:"type"`
			IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
			StreamCutCause    string          `json:"streamCutCause"`
			Timestamp         json.RawMessage `json:"timestamp"`
		}
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) != nil || r.Type != "assistant" && r.Type != "user" {
			continue
		}
		if r.Type != "assistant" || !r.IsAPIErrorMessage || r.StreamCutCause == "" {
			return false, time.Time{}
		}
		var stamp string
		if json.Unmarshal(r.Timestamp, &stamp) == nil {
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				return true, t
			}
		}
		return true, time.Time{}
	}
	return false, time.Time{}
}
