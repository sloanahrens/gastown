//go:build !integration

package makegate

import (
	"os"
	"regexp"
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

// TestPolecatImagePluginDocMatchesTheRecipe checks plugin.md describes the build
// the recipe runs (gt-gg9hv.33): the doc names the context command the recipe
// calls first and the pins files that command reads, and names no input the
// recipe has dropped. An agent or operator reads the doc to run the build, so a
// dropped input left there sends them to set a variable the recipe ignores.
// Mutation: put "BD_SRC" back in plugin.md, and the dropped-input check fails;
// take the context command's path out, and the naming check fails.
func TestPolecatImagePluginDocMatchesTheRecipe(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("../../plugins/polecat-image/plugin.md")
	if err != nil {
		t.Fatalf("read plugin.md: %v", err)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	recipe := strings.Join(recipeLines(string(makefile), "polecat-image"), "\n")
	// The command the recipe runs first is the URL of the doc's step 4, and the
	// pins files it reads are named there too.
	const contextCommand = "internal/polecat/imagectx/cmd/build"
	if !strings.Contains(recipe, contextCommand) {
		t.Fatalf("the polecat-image recipe does not run %s, so this test reads a stale recipe", contextCommand)
	}
	for _, want := range []string{
		contextCommand,
		"deploy/daemon/image-pins",
		"deploy/polecat/seed-pins",
		"deploy/polecat/seed-urls",
	} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("plugin.md does not name %q, which the polecat-image build reads", want)
		}
	}
	// The inputs this recipe has dropped: the doc must not name them, and the
	// Makefile must not define them, so the list cannot outlive the drop.
	defines := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Z_][A-Z0-9_]*)\s*[:?+]?=`).FindAllStringSubmatch(string(makefile), -1) {
		defines[m[1]] = true
	}
	for _, dropped := range []string{"BD_SRC", "BD_COMMIT", "POLE_RIG_GO_MODS"} {
		if defines[dropped] {
			t.Fatalf("the Makefile defines %s, so it is not an input the recipe dropped; update this test's list", dropped)
		}
		if strings.Contains(string(doc), dropped) {
			t.Errorf("plugin.md names %s, which the polecat-image recipe no longer takes", dropped)
		}
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
