package done

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// refuseUntickedSeatDir is the seat-file check's production entry: taskDir holds
// MODE and TASK.md, the ticks in the outbox beside it. Dolt mode reads neither
// file and returns nil; an unreadable marker refuses (gt-fvjfsr).
func refuseUntickedSeatDir(taskDir string) error {
	files, err := taskfile.ReadFileMode(taskDir)
	if err != nil {
		return fmt.Errorf("cannot submit: this seat's bead access is unknown, so its acceptance boxes are unknown: %w", err)
	}
	if !files {
		return nil
	}
	return refuseUntickedSeatBoxes(seatDirFiles{
		task:   filepath.Join(taskDir, "TASK.md"),
		result: filepath.Join(filepath.Dir(taskDir), filepath.Base(outbox.Dir), outbox.ResultFile),
	})
}

// seatDirFiles is file mode's TASK.md and outbox result.json (internal/cmd's fileOutbox).
type seatDirFiles struct{ task, result string }

func (d seatDirFiles) TaskText() (string, error) {
	data, err := os.ReadFile(d.task)
	return string(data), err
}

func (d seatDirFiles) ResultJSON() ([]byte, error) {
	data, err := os.ReadFile(d.result)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // a seat that wrote no result yet has no ticks: every box refuses
	}
	return data, err
}

// seatFileReader is the seat's two input files as this check reads them. The
// caller injects the reader, so the check opens no file and a test can fail a read.
type seatFileReader interface {
	TaskText() (string, error)
	ResultJSON() ([]byte, error)
}

// refuseUntickedSeatBoxes refuses a file-mode submission whose seat files leave
// a TASK.md acceptance box unticked, naming each one; nil when every box is
// ticked or TASK.md has no acceptance boxes. TASK.md's boxes are the ones
// taskfile.ParseBoxes reads back, so the ticks in result.json index them. An
// unreadable file, a ParseBoxes error, a replacement that is not the same boxes
// and a result.json that does not decode all refuse, never as every box ticked.
func refuseUntickedSeatBoxes(seat seatFileReader) error {
	text, err := seat.TaskText()
	if err != nil {
		return fmt.Errorf("cannot submit: the seat's TASK.md cannot be read, so its acceptance boxes are unknown: %w", err)
	}
	boxes, err := taskfile.ParseBoxes(text)
	if err != nil {
		return fmt.Errorf("cannot submit: TASK.md's acceptance boxes cannot be read: %w", err)
	}
	block, blockErr := renderBoxes(boxes)
	raw, err := seat.ResultJSON()
	if err != nil {
		return fmt.Errorf("cannot submit: the seat's result.json cannot be read, so its ticked boxes are unknown: %w", err)
	}
	// Only these two fields are read: the result may be unfinished, and the host
	// validates the finished file with outbox.Validate at seat end.
	var r outbox.Result
	if len(raw) > 0 {
		var ticks struct {
			Acceptance *string `json:"acceptance"`
			Ticked     []int   `json:"ticked_boxes"`
		}
		if err := json.Unmarshal(raw, &ticks); err != nil {
			return fmt.Errorf("cannot submit: the seat's result.json cannot be decoded, so its ticked boxes are unknown: %w", err)
		}
		r.Acceptance, r.Ticked = ticks.Acceptance, ticks.Ticked
	}
	// A file-mode seat ticks by resending the block, so the result may carry a
	// replacement; with TASK.md boxes it may change tick marks only.
	if r.Acceptance != nil && len(boxes) > 0 {
		if err := replacementMismatch(block, *r.Acceptance); err != nil {
			return fmt.Errorf("cannot submit: %w", err)
		}
	}
	// The block and the ticks are judged together: either being unjudgeable is
	// one refusal, and a render guard no ParseBoxes output fails today still
	// refuses here rather than passing.
	unticked, err := untickedBoxes(block, r)
	if err := errors.Join(blockErr, err); err != nil {
		return fmt.Errorf("cannot submit: the seat's acceptance boxes cannot be judged: %w", err)
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

// replacementMismatch returns why the replacement acceptance may not stand in
// for TASK.md's boxes: its boxed items must be the rendered block's in number,
// order and text, so a box dropped, added, reworded or reordered refuses.
func replacementMismatch(block, replacement string) error {
	want, got := boxedItems(block), boxedItems(replacement)
	for i := 0; i < len(want) && i < len(got); i++ {
		if want[i] != got[i] {
			return fmt.Errorf("result.json's acceptance replacement changes TASK.md's boxes: %q is missing or different", want[i])
		}
	}
	switch {
	case len(got) > len(want):
		return fmt.Errorf("result.json's acceptance replacement adds a box (%q) that TASK.md does not have", got[len(want)])
	case len(got) < len(want):
		return fmt.Errorf("result.json's acceptance replacement drops TASK.md's box %q", want[len(got)])
	}
	return nil
}

// boxedItems is every boxed item's text in text, in order, as beads reads them.
func boxedItems(text string) []string {
	var items []string
	for _, item := range beads.AcceptanceItems(text) {
		if item.Boxed {
			items = append(items, strings.TrimSpace(item.Box))
		}
	}
	return items
}

// renderBoxes renders parsed acceptance boxes into the block untickedBoxes
// judges: one "- [x] " or "- [ ] " line per box, carrying the first line of its
// text, so a later line that reads like a checkbox can neither add a box nor
// move an index.
func renderBoxes(boxes []taskfile.Box) (string, error) {
	lines := make([]string, len(boxes))
	for i, box := range boxes {
		mark := "- [ ] "
		if box.Ticked {
			mark = "- [x] "
		}
		first, _, _ := strings.Cut(box.Text, "\n")
		lines[i] = mark + first
	}
	return checkedBlock(strings.Join(lines, "\n"), len(boxes))
}

// checkedBlock returns block when beads reads it as want boxes, so untickedBoxes
// judges exactly the boxes ParseBoxes found, and refuses any other block.
func checkedBlock(block string, want int) (string, error) {
	if found := len(beads.AcceptanceBoxes(block)); found != want {
		return "", fmt.Errorf("the rendered acceptance text reads as %d boxes for %d parsed boxes", found, want)
	}
	return block, nil
}
