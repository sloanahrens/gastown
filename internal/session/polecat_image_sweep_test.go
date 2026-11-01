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
// builds it for the image's platform into deploy/polecat/bin, the bin/ the
// Dockerfile's build context copies. A binary built for the host would not
// start in the runner container (gt-q7wkn).
// Mutation: drop the COPY, the chmod entry, or one of GOOS, GOARCH and
// CGO_ENABLED from the recipe line, and this goes red.
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
	if want := "COPY bin/sweep-run " + target; !slices.Equal(copies, []string{want}) {
		t.Errorf("the Dockerfile's copies of the sweep runner are %q, want exactly %q: bin/sweep-run is where make polecat-image writes it inside the build context", copies, want)
	}
	if len(chmods) != 1 {
		t.Errorf("the Dockerfile has %d chmod runs naming %s, want 1: the chmod is what makes the copied file executable whatever mode the artifact carries", len(chmods), target)
	}

	recipe := polecatImageRecipe(t)
	var build []string
	for _, line := range recipe {
		if strings.Contains(line, "./cmd/sweep-run") {
			build = strings.Fields(line)
		}
	}
	if build == nil {
		t.Fatalf("the Makefile's polecat-image recipe builds no ./cmd/sweep-run:\n%s", strings.Join(recipe, "\n"))
	}
	for _, want := range []string{"go", "build", "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "-trimpath"} {
		if !slices.Contains(build, want) {
			t.Errorf("the polecat-image recipe's sweep-run build lacks %q: %q", want, strings.Join(build, " "))
		}
	}
	// The -o path is deploy/polecat/bin/sweep-run because deploy/polecat is the
	// Docker build context: that file is the bin/sweep-run the Dockerfile copies.
	if i := slices.Index(build, "-o"); i < 0 || i+1 >= len(build) {
		t.Errorf("the polecat-image recipe's sweep-run build names no output file: %q", strings.Join(build, " "))
	} else if build[i+1] != "deploy/polecat/bin/sweep-run" {
		t.Errorf("the polecat-image recipe writes the sweep runner to %q, want deploy/polecat/bin/sweep-run, the Dockerfile's bin/sweep-run", build[i+1])
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
