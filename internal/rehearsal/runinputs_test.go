package rehearsal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runInputsDay is the instant each row runs at unless it names another.
var runInputsDay = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

const runInputsLoad = ".runtime/host-load/2026-11-27.log"

// runInputsRoot builds a town root with every input a good run reads: the rig,
// three worktrees, today's host load file and sixteen beads under sets/.
func runInputsRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"rehearsal", "w1", "w2", "w3", ".runtime/host-load", "sets"} {
		must(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	writeFile(t, root, runInputsLoad, "2026-11-27T10:00:00Z load1=1.00 load5=1.00 load15=1.00 ncpu=24\n")
	for i := 1; i <= 16; i++ {
		writeFile(t, filepath.Join(root, "sets"), fmt.Sprintf("%02d.md", i), beadText(i))
	}
	return root
}

// runInputsArgs is a run over runInputsRoot: worktree-2 and the set are absolute, the rest relative.
func runInputsArgs(root string) RunArgs {
	return RunArgs{Rig: "rehearsal", Set: filepath.Join(root, "sets"), Worktrees: [3]string{"w1", filepath.Join(root, "w2"), "w3"}}
}

// must fails the test on an error from a fixture step.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRehearsalRunInputsGoodInputsReturnTheSet(t *testing.T) {
	t.Parallel()
	root := runInputsRoot(t)
	tasks, err := checkRunInputs(runInputsArgs(root), root, func() time.Time { return runInputsDay })
	if err != nil {
		t.Fatalf("checkRunInputs: %v", err)
	}
	if len(tasks) != 16 || tasks[0].Name != "01.md" || tasks[15].Name != "16.md" {
		t.Fatalf("checkRunInputs returned %d tasks, want the sixteen beads in name order", len(tasks))
	}
}

func TestRehearsalRunInputsRefusesEachInputByPath(t *testing.T) {
	t.Parallel()
	touch := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) { writeFile(t, root, name, "x") }
	}
	rm := func(name string) func(*testing.T, string) {
		return func(t *testing.T, root string) { must(t, os.Remove(filepath.Join(root, name))) }
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string)
		edit  func(a *RunArgs, root string)
		at    time.Time
		want  string // the path the error names, relative to root
	}{
		{name: "rig missing", edit: func(a *RunArgs, _ string) { a.Rig = "gone" }, want: "gone"},
		{name: "rig is a file", setup: touch("rig-file"), edit: func(a *RunArgs, _ string) { a.Rig = "rig-file" }, want: "rig-file"},
		{name: "worktree-1 missing", edit: func(a *RunArgs, _ string) { a.Worktrees[0] = "absent-w1" }, want: "absent-w1"},
		{name: "worktree-2 absolute and missing", edit: func(a *RunArgs, root string) { a.Worktrees[1] = filepath.Join(root, "absent-w2") }, want: "absent-w2"},
		{name: "worktree-3 is a file", setup: touch("w3-file"), edit: func(a *RunArgs, _ string) { a.Worktrees[2] = "w3-file" }, want: "w3-file"},
		{name: "host load file missing", setup: rm(runInputsLoad), want: runInputsLoad},
		{name: "host load path is a directory", setup: func(t *testing.T, root string) {
			rm(runInputsLoad)(t, root)
			must(t, os.Mkdir(filepath.Join(root, runInputsLoad), 0o755))
		}, want: runInputsLoad},
		{name: "host load file only for another day", at: time.Date(2026, 11, 28, 0, 5, 0, 0, time.UTC), want: ".runtime/host-load/2026-11-28.log"},
		{name: "set missing", edit: func(a *RunArgs, _ string) { a.Set = "absent-set" }, want: "absent-set"},
		{name: "set holds fifteen beads", setup: rm("sets/16.md"), want: "sets"},
		{name: "set bead has no title", setup: func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "sets"), "01.md", "no title\n\nbody\n\n## Acceptance\n")
		}, want: "sets"},
		{name: "rig and set missing name the rig", edit: func(a *RunArgs, _ string) { a.Rig, a.Set = "gone", "absent-set" }, want: "gone"},
		{name: "host load and set missing name the host load file", setup: rm(runInputsLoad), edit: func(a *RunArgs, _ string) { a.Set = "absent-set" }, want: runInputsLoad},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := runInputsRoot(t)
			if tc.setup != nil {
				tc.setup(t, root)
			}
			a := runInputsArgs(root)
			if tc.edit != nil {
				tc.edit(&a, root)
			}
			at := tc.at
			if at.IsZero() {
				at = runInputsDay
			}
			tasks, err := checkRunInputs(a, root, func() time.Time { return at })
			if want := filepath.Join(root, tc.want); err == nil || !strings.Contains(err.Error(), want) || tasks != nil {
				t.Fatalf("checkRunInputs returned %d tasks, err %v; want no tasks and an error naming %s", len(tasks), err, want)
			}
		})
	}
}
