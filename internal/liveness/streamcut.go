package liveness

import (
	"bytes"
	"encoding/json"
)

// StreamCutTail reports whether the last complete user-or-assistant record of a
// transcript tail is an assistant record the harness cut short: isApiErrorMessage
// true and a non-empty streamCutCause. Partial and undecodable lines are skipped.
func StreamCutTail(tail []byte) bool {
	lines := bytes.Split(tail, []byte("\n"))
	lines = lines[:len(lines)-1] // the segment after the last newline is never a complete record
	for i := len(lines) - 1; i >= 0; i-- {
		var r struct {
			Type              string `json:"type"`
			IsAPIErrorMessage bool   `json:"isApiErrorMessage"`
			StreamCutCause    string `json:"streamCutCause"`
		}
		if json.Unmarshal(bytes.TrimSpace(lines[i]), &r) != nil || r.Type != "assistant" && r.Type != "user" {
			continue
		}
		return r.Type == "assistant" && r.IsAPIErrorMessage && r.StreamCutCause != ""
	}
	return false
}
