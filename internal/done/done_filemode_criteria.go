package done

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/outbox"
)

// untickedBoxes returns the acceptance items the seat's result leaves unticked,
// each named as bd prints it, or an error for a tick index outside the block's
// boxes or a replacement block of nothing. Ticks are read as outbox.Apply
// writes them: r.Acceptance replaces acceptance, and r.Ticked indexes the boxes
// of the block that results.
func untickedBoxes(acceptance string, r outbox.Result) ([]string, error) {
	block := acceptance
	if r.Acceptance != nil {
		if strings.TrimSpace(*r.Acceptance) == "" {
			return nil, errors.New("the result's acceptance is empty, so it replaces the criteria with nothing")
		}
		block = *r.Acceptance
	}
	boxes, lines := beads.AcceptanceBoxes(block), strings.Split(block, "\n")
	ticked := make([]bool, len(boxes))
	for _, i := range r.Ticked {
		if i < 0 || i >= len(boxes) {
			return nil, fmt.Errorf("tick index %d is not one of the %d checkbox lines", i, len(boxes))
		}
		ticked[i] = true
	}
	var unticked []string
	for i, box := range boxes {
		if !box.Checked && !ticked[i] {
			unticked = append(unticked, strings.TrimSpace(lines[box.Line]))
		}
	}
	return unticked, nil
}
