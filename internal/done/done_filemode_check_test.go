package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
)

const seatBeadID = "gt-seat1"

// fakeSeatFiles is the seat's files as a test sets them: a set error stands for
// a file the seat cannot read, and no result bytes for a seat that has written
// no result.json yet.
type fakeSeatFiles struct {
	text    string
	textErr error
	raw     []byte
	rawErr  error
}

func (f fakeSeatFiles) TaskText() (string, error) { return f.text, f.textErr }

func (f fakeSeatFiles) ResultJSON() ([]byte, error) { return f.raw, f.rawErr }

// seatTaskText is the TASK.md Assemble renders for a seat holding boxes.
func seatTaskText(boxes ...taskfile.Box) string {
	return taskfile.Assemble(taskfile.Input{BeadID: seatBeadID, Title: "The seat's task", Description: "The task text.", Boxes: boxes})
}

// seatFiles is a seat whose TASK.md holds boxes and whose result.json is
// result, as the outbox encoder writes it.
func seatFiles(boxes []taskfile.Box, result outbox.Result) fakeSeatFiles {
	return fakeSeatFiles{text: seatTaskText(boxes...), raw: outbox.Encode(seatBeadID, result)}
}

// TestNoBeadAccessCheckRefusesUntickedBoxes: a box the files leave unticked is
// refused and named, every parsed box ticked passes, and a TASK.md with no
// acceptance section is no refusal by itself.
func TestNoBeadAccessCheckRefusesUntickedBoxes(t *testing.T) {
	t.Parallel()
	first := taskfile.Box{Text: "the first box"}
	second := taskfile.Box{Text: "the second box"}
	for name, tc := range map[string]struct {
		seat    fakeSeatFiles
		wantNil bool
		want    []string
	}{
		"one unticked box refuses and names it": {
			seat: seatFiles([]taskfile.Box{first, second}, outbox.Result{Ticked: []int{0}}),
			want: []string{"1 acceptance criteria unticked", "- [ ] the second box"},
		},
		"every box ticked returns nil": {
			seat:    seatFiles([]taskfile.Box{first, second}, outbox.Result{Ticked: []int{0, 1}}),
			wantNil: true,
		},
		"a box ticked in TASK.md counts": {
			seat:    seatFiles([]taskfile.Box{{Text: "the ticked box", Ticked: true}, second}, outbox.Result{Ticked: []int{1}}),
			wantNil: true,
		},
		"no acceptance heading and no ticks returns nil": {
			seat:    fakeSeatFiles{text: taskfile.Assemble(taskfile.Input{BeadID: seatBeadID, Title: "No criteria"})},
			wantNil: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refuseUntickedSeatBoxes(tc.seat)
			if tc.wantNil {
				if err != nil {
					t.Fatalf("refuseUntickedSeatBoxes = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("refuseUntickedSeatBoxes = nil, want a refusal naming each unticked box")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not carry %q", err, want)
				}
			}
		})
	}
}

// TestNoBeadAccessCheckRefusals: each file the check cannot read or decode is a
// refusal carrying its reason, never a pass and never "no boxes".
func TestNoBeadAccessCheckRefusals(t *testing.T) {
	t.Parallel()
	box := taskfile.Box{Text: "the box"}
	for name, tc := range map[string]struct {
		seat fakeSeatFiles
		want []string
	}{
		"unreadable TASK.md": {
			seat: fakeSeatFiles{textErr: errors.New("permission denied")},
			want: []string{"TASK.md cannot be read", "permission denied"},
		},
		"TASK.md ParseBoxes rejects": {
			seat: fakeSeatFiles{text: "## Acceptance\n\n- a bullet that is not a box\n"},
			want: []string{"acceptance boxes cannot be read", "not a box"},
		},
		"unreadable result.json": {
			seat: fakeSeatFiles{text: seatTaskText(box), rawErr: errors.New("input/output error")},
			want: []string{"result.json cannot be read", "input/output error"},
		},
		"undecodable result.json": {
			seat: fakeSeatFiles{text: seatTaskText(box), raw: []byte("{")},
			want: []string{"result.json cannot be decoded"},
		},
		"a tick past TASK.md's boxes": {
			seat: seatFiles([]taskfile.Box{box}, outbox.Result{Ticked: []int{5}}),
			want: []string{"ticks cannot be judged", "tick index 5"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refuseUntickedSeatBoxes(tc.seat)
			if err == nil {
				t.Fatalf("refuseUntickedSeatBoxes = nil, want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not carry %q", err, want)
				}
			}
		})
	}
}

// TestNoBeadAccessCheckMultiLineFirstBox: a first box of two lines is still one
// box, so a tick naming the second box leaves the first unticked and names its
// first line, while ticking both passes.
func TestNoBeadAccessCheckMultiLineFirstBox(t *testing.T) {
	t.Parallel()
	boxes := []taskfile.Box{{Text: "the first box's first line\nthe first box's second line"}, {Text: "the second box"}}
	err := refuseUntickedSeatBoxes(seatFiles(boxes, outbox.Result{Ticked: []int{1}}))
	if err == nil || !strings.Contains(err.Error(), "the first box's first line") || strings.Contains(err.Error(), "the second box") {
		t.Errorf("refuseUntickedSeatBoxes with only box 1 ticked = %v, want the first box's first line refused alone", err)
	}
	if err := refuseUntickedSeatBoxes(seatFiles(boxes, outbox.Result{Ticked: []int{0, 1}})); err != nil {
		t.Errorf("refuseUntickedSeatBoxes with both boxes ticked = %v, want nil", err)
	}
}
