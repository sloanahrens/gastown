package done

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// seatFileReader is the seat's two input files as this check reads them:
// TASK.md's text and the outbox's result.json bytes. The caller injects the
// reader, so the check opens no file itself and a test can make a read fail.
type seatFileReader interface {
	// TaskText returns TASK.md's text. An unreadable file is an error.
	TaskText() (string, error)
	// ResultJSON returns result.json's bytes, or none when the seat has
	// written no result yet. An unreadable file is an error.
	ResultJSON() ([]byte, error)
}

// seatTicks is the part of result.json this check reads: the acceptance block
// the seat replaced, and the boxes it ticked by index. It is deliberately not
// outbox.Validate: the seat may have written no result yet, and the host
// validates the finished file with outbox.Validate at seat end.
type seatTicks struct {
	Acceptance *string `json:"acceptance"`
	Ticked     []int   `json:"ticked_boxes"`
}

// refuseUntickedSeatBoxes refuses a file-mode submission whose seat files leave
// an acceptance box unticked, naming each one; nil when every parsed box is
// ticked or TASK.md has no acceptance boxes. TASK.md's boxes are the ones
// taskfile.ParseBoxes reads back, so the ticks in result.json index them. An
// unreadable file, a TASK.md ParseBoxes rejects and a result.json that does not
// decode all refuse: none of them may read as every box ticked.
func refuseUntickedSeatBoxes(seat seatFileReader) error {
	text, err := seat.TaskText()
	if err != nil {
		return fmt.Errorf("cannot submit: the seat's TASK.md cannot be read, so its acceptance boxes are unknown: %w", err)
	}
	boxes, err := taskfile.ParseBoxes(text)
	if err != nil {
		return fmt.Errorf("cannot submit: TASK.md's acceptance boxes cannot be read: %w", err)
	}
	raw, err := seat.ResultJSON()
	if err != nil {
		return fmt.Errorf("cannot submit: the seat's result.json cannot be read, so its ticked boxes are unknown: %w", err)
	}
	var r outbox.Result
	if len(raw) > 0 {
		var ticks seatTicks
		if err := json.Unmarshal(raw, &ticks); err != nil {
			return fmt.Errorf("cannot submit: the seat's result.json cannot be decoded, so its ticked boxes are unknown: %w", err)
		}
		r.Acceptance, r.Ticked = ticks.Acceptance, ticks.Ticked
	}
	unticked, err := untickedBoxes(renderBoxes(boxes), r)
	if err != nil {
		return fmt.Errorf("cannot submit: the seat's result.json ticks cannot be judged: %w", err)
	}
	if len(unticked) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "cannot submit: the seat's files leave %d acceptance criteria unticked, and the landing worker rejects a bead with any:\n", len(unticked))
	for _, line := range unticked {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	b.WriteString("Tick each box the work satisfies, then run gt done again. A box the work does not satisfy is unfinished work: finish it, or gt escalate.")
	return errors.New(b.String())
}

// renderBoxes renders parsed acceptance boxes back into the block
// untickedBoxes judges: one "- [x] " or "- [ ] " line per box, each further
// line of a box's text indented two spaces, the boxes joined by newlines.
func renderBoxes(boxes []taskfile.Box) string {
	lines := make([]string, len(boxes))
	for i, box := range boxes {
		mark := "- [ ] "
		if box.Ticked {
			mark = "- [x] "
		}
		lines[i] = mark + strings.ReplaceAll(box.Text, "\n", "\n  ")
	}
	return strings.Join(lines, "\n")
}
