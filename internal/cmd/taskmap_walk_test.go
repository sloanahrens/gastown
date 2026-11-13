package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/taskmap"
)

// TestEveryCommandHasATask keeps the task map complete. Every runnable,
// visible gt command must either have an entry in internal/taskmap or a line
// in one of the batch files under internal/taskmap/testdata/unplaced; a line
// that has an entry or names no command fails too, so placing a command means
// removing its line in the same change.
//
// Hidden commands are out of the map on purpose: they are plumbing that runs
// from a formula, hook or wrapper (daemon workers, the credential helper, the
// status line), and no agent should be told to reach for one.
func TestEveryCommandHasATask(t *testing.T) {
	t.Parallel()

	paths := taskmapCommandPaths(rootCmd)
	files := readTaskmapUnplaced(t, filepath.Join("..", "taskmap", "testdata", "unplaced"))

	if problems := checkPlacement(paths, files, taskmap.Default()); len(problems) > 0 {
		t.Fatalf("%d problem(s) in the task map:\n%s\n"+
			"Every runnable, visible gt command must be placed, deliberately, so an agent finds tools by the job to be done instead of rediscovering the command tree: give a new command an entry in internal/taskmap, never a line under testdata/unplaced.",
			len(problems), strings.Join(problems, "\n"))
	}
}

// taskmapCommandPaths returns every runnable, visible command path under root,
// without the binary name, sorted. Cobra's own help and completion commands
// are outside the map, and so are hidden commands (see the test above).
func taskmapCommandPaths(root *cobra.Command) []string {
	var paths []string
	var walk func(c *cobra.Command, prefix []string)
	walk = func(c *cobra.Command, prefix []string) {
		for _, child := range c.Commands() {
			name := child.Name()
			if child.Parent() == root && (name == "help" || name == "completion") {
				continue
			}
			p := append(append([]string(nil), prefix...), name)
			if child.Runnable() && !child.Hidden {
				paths = append(paths, strings.Join(p, " "))
			}
			walk(child, p)
		}
	}
	walk(root, nil)
	sort.Strings(paths)
	return paths
}

// readTaskmapUnplaced returns the lines of each batch file in dir, keyed by
// file name. A read failure stops the test: the map's completeness must never
// be reported from an unread directory.
func readTaskmapUnplaced(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := make(map[string][]string, len(entries))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var lines []string
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		files[e.Name()] = lines
	}
	return files
}

// batchFiles returns the batch file names the map expects, P1.txt to P13.txt.
func batchFiles() []string {
	names := make([]string, 13)
	for i := range names {
		names[i] = fmt.Sprintf("P%d.txt", i+1)
	}
	return names
}

// checkPlacement reports every way the batch files and reg disagree about the
// command tree: a batch file missing or stray, a path listed more than once,
// and every problem Registry.Validate finds between paths and reg's entries.
// A batch file may be empty, but a path listed twice hides a command whichever
// file it is on, so it is an alarm too.
func checkPlacement(paths []string, files map[string][]string, reg *taskmap.Registry) []string {
	want := batchFiles()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var problems []string
	for _, name := range names {
		if !slices.Contains(want, name) {
			problems = append(problems, fmt.Sprintf("%s: not a batch file (want P1.txt to P13.txt)", name))
		}
	}
	for _, name := range want {
		if _, ok := files[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: batch file missing (want P1.txt to P13.txt)", name))
		}
	}

	first := make(map[string]string)
	var unplaced []string
	for _, name := range names {
		for _, line := range files[name] {
			if seen, ok := first[line]; ok {
				if seen == name {
					problems = append(problems, fmt.Sprintf("%q: listed twice in %s", line, name))
				} else {
					problems = append(problems, fmt.Sprintf("%q: listed in both %s and %s", line, seen, name))
				}
				continue
			}
			first[line] = name
			unplaced = append(unplaced, line)
		}
	}

	problems = append(problems, reg.Validate(paths, unplaced)...)
	sort.Strings(problems)
	return problems
}

func TestCheckPlacement(t *testing.T) {
	t.Parallel()

	reg := taskmap.NewRegistry()
	reg.Register(taskmap.Entry{Path: "status", Group: "orient", Use: "Use this to see where the town stands."})

	// withFiles starts from all thirteen empty batch files, so a case that
	// fills one reports only what it means to test.
	withFiles := func(lists map[string][]string) map[string][]string {
		files := make(map[string][]string, 13)
		for _, name := range batchFiles() {
			files[name] = nil
		}
		for name, lines := range lists {
			files[name] = lines
		}
		return files
	}

	tests := []struct {
		name  string
		paths []string
		files map[string][]string
		want  []string
	}{
		{
			name:  "clean",
			paths: []string{"pool set", "status"},
			files: withFiles(map[string][]string{"P1.txt": {"pool set"}}),
		},
		{
			name:  "path listed in two batch files",
			paths: []string{"pool set", "status"},
			files: withFiles(map[string][]string{"P1.txt": {"pool set"}, "P2.txt": {"pool set"}}),
			want:  []string{`"pool set": listed in both P1.txt and P2.txt`},
		},
		{
			name:  "path listed twice in one batch file",
			paths: []string{"pool set", "status"},
			files: withFiles(map[string][]string{"P1.txt": {"pool set", "pool set"}}),
			want:  []string{`"pool set": listed twice in P1.txt`},
		},
		{
			name:  "stray batch file",
			paths: []string{"status"},
			files: func() map[string][]string {
				files := withFiles(nil)
				files["P14.txt"] = nil
				return files
			}(),
			want: []string{"P14.txt: not a batch file (want P1.txt to P13.txt)"},
		},
		{
			name:  "batch file missing",
			paths: []string{"status"},
			files: func() map[string][]string {
				files := withFiles(nil)
				delete(files, "P13.txt")
				return files
			}(),
			want: []string{"P13.txt: batch file missing (want P1.txt to P13.txt)"},
		},
		{
			name:  "unplaced command with no entry",
			paths: []string{"status", "orphan"},
			files: withFiles(nil),
			want:  []string{`"orphan": no task map entry and no line in testdata/unplaced`},
		},
		{
			name:  "stale line names no command",
			paths: []string{"status"},
			files: withFiles(map[string][]string{"P1.txt": {"gone"}}),
			want:  []string{`"gone": line in testdata/unplaced but no such command (remove the line)`},
		},
		{
			name:  "line for a command that already has an entry",
			paths: []string{"status"},
			files: withFiles(map[string][]string{"P1.txt": {"status"}}),
			want:  []string{`"status": line in testdata/unplaced but the command has an entry (remove the line)`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := checkPlacement(tc.paths, tc.files, reg)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("checkPlacement(%v, %v) = %q, want %q", tc.paths, tc.files, got, tc.want)
			}
		})
	}
}
