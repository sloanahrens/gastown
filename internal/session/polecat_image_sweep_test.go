package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPolecatImageCopiesSweepRun pins the two halves of the tier-sweep runner
// being runnable in the image: the Dockerfile copies its entrypoint to
// /usr/local/bin/sweep-run and makes it executable, and make polecat-image
// stages the build context that holds bin/sweep-run, which the context command
// cross-compiles for the image's platform (internal/polecat/imagectx pins the
// arm64 environment and the output path). A binary built for the host would not
// start in the runner container (gt-q7wkn).
// Mutation: drop the COPY, its --chmod=0755, or the staging command from the
// recipe, or add a RUN chmod on the target, and this goes red.
func TestPolecatImageCopiesSweepRun(t *testing.T) {
	t.Parallel()
	const target = "/usr/local/bin/sweep-run"
	var copies, chmods []string
	for _, in := range dockerfileInstructions(t) {
		switch {
		case strings.HasPrefix(in, "COPY ") && strings.Contains(in, target):
			copies = append(copies, in)
		case strings.HasPrefix(in, "RUN ") && strings.Contains(in, "chmod ") && strings.Contains(in, target):
			chmods = append(chmods, in)
		}
	}
	if want := "COPY --chmod=0755 bin/sweep-run " + target; !slices.Equal(copies, []string{want}) {
		t.Errorf("the Dockerfile's copies of the sweep runner are %q, want exactly %q: bin/sweep-run is where make polecat-image writes it inside the build context, and COPY sets it executable", copies, want)
	}
	if len(chmods) != 0 {
		t.Errorf("the Dockerfile has %d RUN chmod runs naming %s, want none: a RUN chmod rewrites the runner into a second layer, so its mode comes from the COPY", len(chmods), target)
	}

	recipe := polecatImageRecipe(t)
	staged := false
	for _, line := range recipe {
		if strings.Contains(line, "./internal/polecat/imagectx/cmd/build") {
			staged = true
		}
	}
	if !staged {
		t.Fatalf("the Makefile's polecat-image recipe stages no build context, so no bin/sweep-run is built for the image:\n%s", strings.Join(recipe, "\n"))
	}
}

// polecatImageRecipe returns the Makefile's polecat-image recipe lines, with
// their leading tab kept off.
func polecatImageRecipe(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	inRecipe := false
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "\t") {
			inRecipe = strings.HasPrefix(line, "polecat-image:")
			continue
		}
		if inRecipe {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		t.Fatal("the Makefile has no polecat-image recipe")
	}
	return lines
}
