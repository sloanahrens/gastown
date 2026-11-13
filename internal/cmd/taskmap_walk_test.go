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

	dir := filepath.Join("..", "taskmap", "testdata", "unplaced")
	unplaced, problems := readTaskmapUnplaced(t, dir)

	problems = append(problems, taskmap.Validate(paths, unplaced)...)
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	t.Fatalf("%d problem(s) in the task map:\n%s\nplace the command in internal/taskmap/place_pN.go for its batch; never add a line to a file under testdata/unplaced",
		len(problems), strings.Join(problems, "\n"))
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

// readTaskmapUnplaced returns the command paths listed across the batch files
// in dir, plus one problem per path that appears more than once. The file
// names are checked so that a stray file is reported by name instead of being
// read as unplaced command paths.
func readTaskmapUnplaced(t *testing.T, dir string) ([]string, []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	sort.Strings(files)
	want := make([]string, 13)
	for i := range want {
		want[i] = fmt.Sprintf("P%d.txt", i+1)
	}
	sort.Strings(want)
	if !slices.Equal(files, want) {
		t.Fatalf("%s holds %v; want exactly %v", dir, files, want)
	}

	var paths, problems []string
	seen := map[string]string{}
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" {
				continue
			}
			if first, ok := seen[line]; ok {
				if first == file {
					problems = append(problems, fmt.Sprintf("%q: listed twice in %s", line, file))
				} else {
					problems = append(problems, fmt.Sprintf("%q: listed in both %s and %s", line, first, file))
				}
				continue
			}
			seen[line] = file
			paths = append(paths, line)
		}
	}
	return paths, problems
}
