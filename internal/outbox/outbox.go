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
//	  "title": "DISPROVEN: the retry resets; the counter is reset at the call site",
//	  "acceptance": "## Acceptance\n- [x] the refusal names the verb",
//	  "ticked_boxes": [0, 2],
//	  "findings": ["the retry never resets its counter"],
//	  "escalation": {"text": "HELP: the acceptance list names a missing file"}
//	}
//
// bead_id is the bead the seat was given. The seat omits a field it has nothing
// to report for. title and acceptance are the edits a file-mode `gt bead
// update` makes: the new title, and the whole replacement criteria block, as
// bd's --acceptance means it.
//
// The package does no I/O: the host reads the two files and hands their contents
// to Validate.
package outbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// The names a seat's outbox lives under, under the seat's root: the directory
// the host copies out of a sandbox and the notes file a file-mode write
// appends to.
const (
	// Dir is the outbox directory under a seat's root.
	Dir = ".gt/outbox"
	// NotesFile is the seat's notes inside Dir, which `gt bead note` appends
	// to in file mode.
	NotesFile = "notes.md"
	// ResultFile is the seat's result inside Dir, which `gt bead update`
	// rewrites in file mode.
	ResultFile = "result.json"
)

const (
	// maxFindings is the most findings one outbox may carry.
	maxFindings = 5
	// maxFindingBytes is the longest a single finding may be.
	maxFindingBytes = 2 << 10
	// maxResultBytes caps result.json; a longer file is refused.
	maxResultBytes = 64 << 10
	// maxNotesBytes caps notes.md; a longer file is refused.
	maxNotesBytes = 256 << 10
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
	// Title is the title the seat set, or nil when it changed none.
	Title *string
	// Acceptance is the whole criteria block the seat set, or nil when it
	// replaced none.
	Acceptance *string
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
	Title       *string     `json:"title"`
	Acceptance  *string     `json:"acceptance"`
	TickedBoxes []int       `json:"ticked_boxes"`
	Findings    []string    `json:"findings"`
	Escalation  *Escalation `json:"escalation"`
}

// Validate decodes the seat's result.json (raw) and notes.md (notes) into a
// Result for beadID, the bead the seat was given.
//
// It refuses, with an error and no Result: an empty beadID; a result.json over
// 64 KB or a notes.md over 256 KB; a result.json or notes.md that is not valid
// UTF-8; a raw that is not exactly one JSON object of the shape the package doc
// gives; an unknown field, which is how it refuses a label, assignee, close or
// other-bead field; a bead_id that is not beadID; a status other than StatusDone
// or StatusDeferred; a title or acceptance that is empty or only spaces; more
// than 5 findings; a finding over 2 KB; a negative or repeated ticked box; and
// an escalation whose text is empty.
func Validate(raw []byte, notes []byte, beadID string) (Result, error) {
	if beadID == "" {
		return Result{}, errors.New("outbox: no bead id was given to validate against")
	}
	if len(raw) > maxResultBytes {
		return Result{}, fmt.Errorf("outbox: result.json is %d bytes, more than the %d-byte cap", len(raw), maxResultBytes)
	}
	if !utf8.Valid(raw) {
		return Result{}, errors.New("outbox: result.json is not valid UTF-8")
	}
	if len(notes) > maxNotesBytes {
		return Result{}, fmt.Errorf("outbox: notes.md is %d bytes, more than the %d-byte cap", len(notes), maxNotesBytes)
	}
	if !utf8.Valid(notes) {
		return Result{}, errors.New("outbox: notes.md is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire *wireResult
	if err := dec.Decode(&wire); err != nil {
		return Result{}, fmt.Errorf("outbox: decode result.json: %w", err)
	}
	if wire == nil {
		return Result{}, errors.New("outbox: decode result.json: null is not a result")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("outbox: decode result.json: trailing data after the object")
	}
	if wire.BeadID != beadID {
		return Result{}, errors.New("outbox: result.json bead_id is not the bead the seat was given")
	}
	switch wire.Status {
	case StatusDone, StatusDeferred:
	default:
		return Result{}, fmt.Errorf("outbox: result.json status is neither %q nor %q", StatusDone, StatusDeferred)
	}
	// A title of nothing leaves the bead unnamed, and an acceptance of nothing
	// replaces the criteria with none, which gt done reads as every box
	// satisfied (gt-9smgr). The seat's own update refuses both, and a hand-built
	// result must not get past the decoder with them either.
	if wire.Title != nil && strings.TrimSpace(*wire.Title) == "" {
		return Result{}, errors.New("outbox: result.json title is empty")
	}
	if wire.Acceptance != nil && strings.TrimSpace(*wire.Acceptance) == "" {
		return Result{}, errors.New("outbox: result.json acceptance is empty, so it replaces the criteria with nothing")
	}
	if len(wire.Findings) > maxFindings {
		return Result{}, fmt.Errorf("outbox: result.json has %d findings, more than the %d allowed", len(wire.Findings), maxFindings)
	}
	for i, finding := range wire.Findings {
		if len(finding) > maxFindingBytes {
			return Result{}, fmt.Errorf("outbox: result.json finding %d is %d bytes, more than the %d allowed", i, len(finding), maxFindingBytes)
		}
	}
	seen := make(map[int]bool, len(wire.TickedBoxes))
	for i, box := range wire.TickedBoxes {
		if box < 0 {
			return Result{}, fmt.Errorf("outbox: result.json ticked box %d is negative (%d)", i, box)
		}
		if seen[box] {
			return Result{}, fmt.Errorf("outbox: result.json ticked box %d repeats an earlier index (%d)", i, box)
		}
		seen[box] = true
	}
	if wire.Escalation != nil && wire.Escalation.Text == "" {
		return Result{}, errors.New("outbox: result.json escalation text is empty")
	}
	return Result{
		Status:     wire.Status,
		Title:      wire.Title,
		Acceptance: wire.Acceptance,
		Ticked:     wire.TickedBoxes,
		Findings:   wire.Findings,
		Escalation: wire.Escalation,
		Notes:      string(notes),
	}, nil
}

// Encode returns the result.json bytes for beadID: every field of r, in the
// shape Validate decodes, which is the only shape the host applies. A seat
// writes all of r rather than the field it changed, so a later write keeps what
// an earlier one recorded: an escalation raised after an update still carries
// the update.
//
// Every field of the shape is a string, a string pointer, an int list or a list
// of strings, so the encoding has no failure to report; a caller validates the
// bytes it gets, and Validate refuses what the encoder could not have written
// as a result the host applies.
func Encode(beadID string, r Result) []byte {
	raw, _ := json.MarshalIndent(wireResult{ //nolint:errcheck // wireResult holds only strings and ints; MarshalIndent cannot fail for it
		BeadID:      beadID,
		Status:      r.Status,
		Title:       r.Title,
		Acceptance:  r.Acceptance,
		TickedBoxes: r.Ticked,
		Findings:    r.Findings,
		Escalation:  r.Escalation,
	}, "", "  ")
	return append(raw, '\n')
}
