//go:build !integration

package makegate

import (
	"os"
	"strings"
	"testing"
)

// TestPolecatImageRecipe checks the polecat-image recipe is glue over the context
// command (gt-gg9hv.33): the command runs before the engine build, the engine
// build reads the context the command printed, and no recipe line before the
// engine build reaches a container engine or cross-compiles on its own. The
// integration tier runs the recipe for real under stub tools; this tier reads
// the recipe's own lines, so `go test` proves the order with no make call.
// Mutation: put the engine build before the command line, and the first check
// fails; build from deploy/polecat instead of the staged context, and the last
// one does.
func TestPolecatImageRecipe(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	lines := recipeLines(string(data), "polecat-image")
	stage, build := -1, -1
	for i, l := range lines {
		if stage < 0 && strings.Contains(l, "./internal/polecat/imagectx/cmd/build") {
			stage = i
		}
		if build < 0 && strings.Contains(l, "docker build") {
			build = i
		}
	}
	if stage < 0 {
		t.Fatalf("the polecat-image recipe never runs the context command\n%s", strings.Join(lines, "\n"))
	}
	if build < 0 {
		t.Fatalf("the polecat-image recipe has no engine build\n%s", strings.Join(lines, "\n"))
	}
	if build < stage {
		t.Fatalf("the engine build (recipe line %d) runs before the context command (line %d)\n%s", build, stage, strings.Join(lines, "\n"))
	}
	for _, l := range lines[:build] {
		for _, bad := range []string{"docker", "podman", "nerdctl", "buildctl", "go build"} {
			if strings.Contains(l, bad) && !strings.Contains(l, "imagectx/cmd/build") {
				t.Errorf("a recipe line before the engine build names %q, so the context stage is not engine-free: %s", bad, l)
			}
		}
	}
	if !strings.Contains(lines[build], `"$$ctx"`) {
		t.Errorf("the engine build does not read the context the command printed: %s", lines[build])
	}
}

// recipeLines returns the logical lines of target's recipe: each tab-indented
// line after "target:", with a trailing backslash joined to the line after it,
// and the leading tab and any make prefix (@, -) removed. The recipe ends at the
// first line that is not tab-indented.
func recipeLines(makefile, target string) []string {
	var out []string
	in := false
	joining := ""
	for _, raw := range strings.Split(makefile, "\n") {
		if !in {
			in = raw == target+":"
			continue
		}
		if !strings.HasPrefix(raw, "\t") {
			break
		}
		line := strings.TrimPrefix(raw, "\t")
		if strings.HasSuffix(line, "\\") {
			joining += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		line = joining + line
		joining = ""
		out = append(out, strings.TrimLeft(line, "@-"))
	}
	return out
}
