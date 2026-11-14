// Package outbox defines the file a sandbox seat writes when it ends and the
// decoder the host uses to read it.
//
// A seat writes two files into its outbox directory: result.json, a single JSON
// object, and notes.md, the prose it would have written with `gt bead note`. The
// shape of result.json is:
//
//	{
//	  "bead_id": "gt-swfo9e",
//	  "status": "done",
//	  "ticked_boxes": [0, 2],
//	  "findings": ["the retry never resets its counter"],
//	  "escalation": {"text": "HELP: the acceptance list names a missing file"}
//	}
//
// bead_id is the bead the seat was given. The seat omits a field it has nothing
// to report for.
//
// The package does no I/O: the host reads the two files and hands their contents
// to Validate.
package outbox

import (
	"encoding/json"
	"fmt"
)

// Status is how a seat finished the task it was given.
type Status string

const (
	// StatusDone is a task the seat finished and submitted.
	StatusDone Status = "done"
	// StatusDeferred is a task the seat could not finish, with why in the notes
	// or the escalation.
	StatusDeferred Status = "deferred"
)

// Result is a decoded outbox: the outcome a seat reports for its task, with the
// notes it wrote alongside.
type Result struct {
	Status Status
	// Ticked is the zero-based indexes into the bead's acceptance list that the
	// seat ticked.
	Ticked []int
	// Findings are the texts of the defects the seat met outside its task.
	Findings []string
	// Escalation is the seat's request for help, or nil when it asked for none.
	Escalation *Escalation
	// Notes is the text of the seat's notes.md.
	Notes string
}

// Escalation is a seat's request that the host raise an escalation for it.
type Escalation struct {
	// Text is the request the host raises the escalation with.
	Text string `json:"text"`
}

// wireResult is result.json's shape as the seat writes it.
type wireResult struct {
	BeadID      string      `json:"bead_id"`
	Status      Status      `json:"status"`
	TickedBoxes []int       `json:"ticked_boxes"`
	Findings    []string    `json:"findings"`
	Escalation  *Escalation `json:"escalation"`
}

// Validate decodes the seat's result.json (raw) and notes.md (notes) into a
// Result. beadID is the bead the seat was given. A raw that is not a JSON object
// of result.json's shape is an error, never a zero Result.
func Validate(raw []byte, notes []byte, beadID string) (Result, error) {
	var wire *wireResult
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Result{}, fmt.Errorf("outbox: decode result.json: %w", err)
	}
	if wire == nil {
		return Result{}, fmt.Errorf("outbox: decode result.json: null is not a result")
	}
	return Result{
		Status:     wire.Status,
		Ticked:     wire.TickedBoxes,
		Findings:   wire.Findings,
		Escalation: wire.Escalation,
		Notes:      string(notes),
	}, nil
}
