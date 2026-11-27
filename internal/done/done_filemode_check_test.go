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
// a file the seat cannot read, and no bytes for a seat with no result.json yet.
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
	return taskfile.Assemble(taskfile.Input{BeadID: seatBeadID, Boxes: boxes})
}

// seatFiles is a seat whose TASK.md holds boxes and whose result.json is result.
func seatFiles(boxes []taskfile.Box, result outbox.Result) fakeSeatFiles {
	return fakeSeatFiles{text: seatTaskText(boxes...), raw: outbox.Encode(seatBeadID, result)}
}

// acc is a replacement acceptance block carrying the given box lines.
func acc(lines ...string) *string {
	block := "## Acceptance\n\n" + strings.Join(lines, "\n") + "\n"
	return &block
}

// seatCase is one row of a check table: the seat and the outcome expected.
type seatCase struct {
	seat    fakeSeatFiles
	wantNil bool
	want    []string
}

// checkRefusal asserts refuseUntickedSeatBoxes(tc.seat) is nil when wantNil,
// else an error carrying every want.
func checkRefusal(t *testing.T, tc seatCase) {
	t.Helper()
	err := refuseUntickedSeatBoxes(tc.seat)
	if tc.wantNil {
		if err != nil {
			t.Fatalf("refuseUntickedSeatBoxes = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("refuseUntickedSeatBoxes = nil, want a refusal")
	}
	for _, w := range tc.want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q does not carry %q", err, w)
		}
	}
}

// TestNoBeadAccessCheckSeatFiles: a box the files leave unticked is refused and
// named and every box ticked passes, no acceptance section is no refusal, and
// each file the check cannot read or decode refuses with its reason.
func TestNoBeadAccessCheckSeatFiles(t *testing.T) {
	t.Parallel()
	box, second := taskfile.Box{Text: "the first box"}, taskfile.Box{Text: "the second box"}
	for name, tc := range map[string]seatCase{
		"one unticked box refuses and names it":          {seat: seatFiles([]taskfile.Box{box, second}, outbox.Result{Ticked: []int{0}}), want: []string{"1 acceptance criteria unticked", "- [ ] the second box"}},
		"every box ticked returns nil":                   {seat: seatFiles([]taskfile.Box{box, second}, outbox.Result{Ticked: []int{0, 1}}), wantNil: true},
		"a box ticked in TASK.md counts":                 {seat: seatFiles([]taskfile.Box{{Text: "the ticked box", Ticked: true}, second}, outbox.Result{Ticked: []int{1}}), wantNil: true},
		"no acceptance heading and no ticks returns nil": {seat: fakeSeatFiles{text: taskfile.Assemble(taskfile.Input{BeadID: seatBeadID, Title: "No criteria"})}, wantNil: true},
		"unreadable TASK.md":                             {seat: fakeSeatFiles{textErr: errors.New("permission denied")}, want: []string{"TASK.md cannot be read", "permission denied"}},
		"TASK.md ParseBoxes rejects":                     {seat: fakeSeatFiles{text: "## Acceptance\n\n- a bullet that is not a box\n"}, want: []string{"acceptance boxes cannot be read", "not a box"}},
		"unreadable result.json":                         {seat: fakeSeatFiles{text: seatTaskText(box), rawErr: errors.New("input/output error")}, want: []string{"result.json cannot be read", "input/output error"}},
		"undecodable result.json":                        {seat: fakeSeatFiles{text: seatTaskText(box), raw: []byte("{")}, want: []string{"result.json cannot be decoded"}},
		"a tick past TASK.md's boxes":                    {seat: seatFiles([]taskfile.Box{box}, outbox.Result{Ticked: []int{5}}), want: []string{"acceptance boxes cannot be judged", "tick index 5"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRefusal(t, tc)
		})
	}
}

// TestNoBeadAccessCheckReplacement: a result that resends the acceptance block
// may change tick marks only; the same boxes are then judged by its marks and
// its own ticked_boxes.
func TestNoBeadAccessCheckReplacement(t *testing.T) {
	t.Parallel()
	boxes := []taskfile.Box{{Text: "the first box"}, {Text: "the second box"}}
	for name, tc := range map[string]struct {
		result  outbox.Result
		wantNil bool
		want    []string
	}{
		"a replacement that drops a box refuses even with every box ticked": {result: outbox.Result{Acceptance: acc("- [x] the first box")}, want: []string{"drops", "the second box"}},
		"a replacement that adds a box refuses even with every box ticked":  {result: outbox.Result{Acceptance: acc("- [x] the first box", "- [x] the second box", "- [x] an extra box")}, want: []string{"adds", "an extra box"}},
		"a replacement that rewords a box refuses naming it":                {result: outbox.Result{Acceptance: acc("- [x] the first box, reworded", "- [x] the second box")}, want: []string{"the first box", "missing or different"}},
		"a replacement that reorders the boxes refuses naming the first":    {result: outbox.Result{Acceptance: acc("- [x] the second box", "- [x] the first box")}, want: []string{"the first box", "missing or different"}},
		"a matching replacement with every mark ticked returns nil":         {result: outbox.Result{Acceptance: acc("- [x] the first box", "- [x] the second box")}, wantNil: true},
		"a matching replacement with one box unticked names it":             {result: outbox.Result{Acceptance: acc("- [x] the first box", "- [ ] the second box")}, want: []string{"- [ ] the second box"}},
		"a matching replacement ticks by its own ticked_boxes":              {result: outbox.Result{Acceptance: acc("- [ ] the first box", "- [ ] the second box"), Ticked: []int{0, 1}}, wantNil: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRefusal(t, seatCase{seat: seatFiles(boxes, tc.result), wantNil: tc.wantNil, want: tc.want})
		})
	}
}

// TestNoBeadAccessCheckMultiLineFirstBox: a first box whose later line reads
// like a checkbox is still one box, so ticking index 1 leaves it unticked and
// names it, and ticking both indexes passes.
func TestNoBeadAccessCheckMultiLineFirstBox(t *testing.T) {
	t.Parallel()
	boxes := []taskfile.Box{{Text: "alpha\n- [ ] a checkbox written in a later line"}, {Text: "gamma"}}
	err := refuseUntickedSeatBoxes(seatFiles(boxes, outbox.Result{Ticked: []int{1}}))
	if err == nil || !strings.Contains(err.Error(), "alpha") || strings.Contains(err.Error(), "gamma") {
		t.Errorf("refuseUntickedSeatBoxes with only box 1 ticked = %v, want alpha refused alone", err)
	}
	if err := refuseUntickedSeatBoxes(seatFiles(boxes, outbox.Result{Ticked: []int{0, 1}})); err != nil {
		t.Errorf("refuseUntickedSeatBoxes with both boxes ticked = %v, want nil", err)
	}
}

// TestNoBeadAccessCheckRenderedBoxCount: a block beads reads as other than one
// box per parsed box refuses, so a future render cannot judge fewer.
func TestNoBeadAccessCheckRenderedBoxCount(t *testing.T) {
	t.Parallel()
	if _, err := checkedBlock("- [ ] one\n- [ ] two", 1); err == nil || !strings.Contains(err.Error(), "2 boxes for 1") {
		t.Errorf("checkedBlock with a box more = %v, want a refusal naming the counts", err)
	}
	if block, err := checkedBlock("- [ ] one", 1); err != nil || block != "- [ ] one" {
		t.Errorf("checkedBlock with one box = %q, %v, want the block and nil", block, err)
	}
}
