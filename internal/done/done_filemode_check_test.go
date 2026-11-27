package done

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/taskfile"
	"github.com/steveyegge/gastown/internal/testutil"
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

func must(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}

// seatDir writes files (relative paths; a trailing slash makes a directory) under a fresh private temp dir.
func seatDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := testutil.PrivateTempDir(t)
	for rel, text := range files {
		path := filepath.Join(root, rel)
		if strings.HasSuffix(rel, "/") {
			must(t, os.MkdirAll(path, 0o700))
			continue
		}
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(text), 0o600))
	}
	return filepath.Join(root, ".gt", "task")
}

// envHome is an environment seam whose HOME is home.
func envHome(home string) func(string) string {
	return func(key string) string {
		if key == "HOME" {
			return home
		}
		return ""
	}
}

// seatHome stages TASK.md's boxes and the outbox result under a private home;
// marker is MODE's contents, empty for no marker, "/" for a directory there.
func seatHome(t *testing.T, marker string, boxes []taskfile.Box, result outbox.Result) string {
	t.Helper()
	files := map[string]string{
		".gt/task/TASK.md":                seatTaskText(boxes...),
		".gt/outbox/" + outbox.ResultFile: string(outbox.Encode(seatBeadID, result)),
	}
	switch marker {
	case "":
	case "/":
		files[".gt/task/"+taskfile.ModeFile+"/"] = ""
	default:
		files[".gt/task/"+taskfile.ModeFile] = marker
	}
	return filepath.Dir(filepath.Dir(seatDir(t, files)))
}

// TestNoBeadAccessDoneRefusesUntickedBoxBeforeRebase drives submitForLanding
// with a private home: a file-mode seat with one unticked box is refused, and
// named, before the rebase and the gate; every box ticked submits; a seat with
// no marker submits reading no TASK.md; an unreadable marker, an unreachable
// task directory and an unnameable home refuse (gt-kqa840).
func TestNoBeadAccessDoneRefusesUntickedBoxBeforeRebase(t *testing.T) {
	t.Parallel()
	boxes := []taskfile.Box{{Text: "one"}, {Text: "two"}}
	seat := func(marker string, ticked ...int) string {
		return seatHome(t, marker, boxes, outbox.Result{Ticked: ticked})
	}
	unreachable := func() string {
		home := testutil.PrivateTempDir(t)
		must(t, os.MkdirAll(filepath.Join(home, ".gt"), 0o700))
		must(t, os.Symlink(filepath.Join(home, ".gt", "task"), filepath.Join(home, ".gt", "task")))
		return home
	}
	for name, tc := range map[string]struct{ home, want string }{
		"one unticked box refuses before the rebase": {home: seat(taskfile.ModeFiles, 0), want: "- [ ] two"},
		"every box ticked submits":                   {home: seat(taskfile.ModeFiles, 0, 1)},
		"no marker reads dolt mode and no TASK.md":   {home: seat("")},
		"an unreadable marker refuses":               {home: seat("/"), want: "bead access is unknown"},
		"an unreachable task directory refuses":      {home: unreachable(), want: "task directory"},
		"a home the run cannot name refuses":         {home: "", want: "cannot name the seat's home"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newSubmitHarness(t)
			// Behind the target, so a rebase would move the head if it ran.
			h.repo.behind = 1
			h.r.env = envHome(tc.home)
			if err := h.submit(); tc.want == "" {
				if err != nil {
					t.Fatalf("submit: %v", err)
				}
				if h.repo.head != "feature1-rebased" || len(h.gate.heads) != 1 {
					t.Errorf("head %s gate %v, want the rebased branch gated", h.repo.head, h.gate.heads)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || h.repo.head != "feature1" || len(h.gate.heads) != 0 {
				t.Errorf("submit = %v, head %s, gate %v; want a refusal naming %q before any rebase or gate", err, h.repo.head, h.gate.heads, tc.want)
			}
		})
	}
}

// TestNoBeadAccessSeatDirRefusal: over the seat's files on disk, an unticked box
// refuses and is named, every box ticked passes, each unreadable input refuses,
// and with no marker and no TASK.md there it passes, so dolt mode read no file.
func TestNoBeadAccessSeatDirRefusal(t *testing.T) {
	t.Parallel()
	const mode, task, result = ".gt/task/" + taskfile.ModeFile, ".gt/task/TASK.md", outbox.Dir + "/" + outbox.ResultFile
	text := seatTaskText(taskfile.Box{Text: "one"}, taskfile.Box{Text: "two"})
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"an unticked box refuses and is named":     {files: map[string]string{mode: taskfile.ModeFiles, task: text, result: string(outbox.Encode(seatBeadID, outbox.Result{Ticked: []int{0}}))}, want: "- [ ] two"},
		"every box ticked returns nil":             {files: map[string]string{mode: taskfile.ModeFiles, task: text, result: string(outbox.Encode(seatBeadID, outbox.Result{Ticked: []int{0, 1}}))}},
		"no result written yet refuses every box":  {files: map[string]string{mode: taskfile.ModeFiles, task: text}, want: "2 acceptance criteria unticked"},
		"no marker and no TASK.md reads dolt mode": {files: map[string]string{}},
		"an unreadable marker refuses":             {files: map[string]string{mode + "/": "", task: text}, want: "bead access is unknown"},
		"a file-mode seat without TASK.md refuses": {files: map[string]string{mode: taskfile.ModeFiles}, want: "TASK.md cannot be read"},
		"an unreadable result.json refuses":        {files: map[string]string{mode: taskfile.ModeFiles, task: text, result + "/": ""}, want: "result.json cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refuseUntickedSeatDir(seatDir(t, tc.files))
			if (err != nil) != (tc.want != "") || err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refuseUntickedSeatDir = %v, want %q", err, tc.want)
			}
		})
	}
}
