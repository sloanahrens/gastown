package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// directivesDir is the repo's town directives, relative to this package.
const directivesDir = "../../directives"

// beadTextExample is one ```bash fence of a directive, as the tap guard judged
// it: the line its opening fence sits on and the reason the polecat bead-text
// rule gives for refusing it, "" for a fence a polecat can run.
type beadTextExample struct {
	file   string
	line   int
	reason string
}

// directiveBeadTextExamples runs each ```bash fence of one directive's markdown
// through the bead-text rule. The fence is read the way the hook hands the
// command over — heredoc bodies stripped, then tokenized, then matched — so it
// fares here exactly as it would when a polecat runs it (gt-9j9vu). An
// unterminated fence is an error, not an empty example list: a fence this
// scanner fails to close is one it never judged.
func directiveBeadTextExamples(file, markdown string) ([]beadTextExample, error) {
	var examples []beadTextExample
	var body []string
	open := 0
	for i, line := range strings.Split(markdown, "\n") {
		switch {
		case open == 0 && strings.TrimSpace(line) == "```bash":
			open, body = i+1, body[:0]
		case open != 0 && strings.TrimSpace(line) == "```":
			reason, _ := matchesPolecatAttribution(
				shellTokenize(stripHeredocBodies(strings.Join(body, "\n"))), true, nil)
			examples = append(examples, beadTextExample{file: file, line: open, reason: reason})
			open = 0
		case open != 0:
			body = append(body, line)
		}
	}
	if open != 0 {
		return examples, fmt.Errorf("%s:%d: the ```bash fence is never closed", file, open)
	}
	return examples, nil
}

// TestDirectiveBeadTextExamplesRun holds the directives' own examples to the
// guard: a written rule a polecat follows and is then refused for costs the
// session that finds out (gt-9j9vu). The examples are read from the tree, not
// restated here, so an edit that brings the old recipe back fails this test.
func TestDirectiveBeadTextExamplesRun(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join(directivesDir, "*.md"))
	if err != nil {
		t.Fatalf("listing %s: %v", directivesDir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no directive under %s: the scan would pass on an empty tree", directivesDir)
	}
	judged := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		examples, err := directiveBeadTextExamples(path, string(data))
		if err != nil {
			t.Error(err)
			continue
		}
		for _, ex := range examples {
			if ex.reason != "" {
				t.Errorf("%s:%d: the tap guard refuses this example: %s; a ```bash fence is a recipe a polecat runs, so show a refused form as inline code",
					ex.file, ex.line, ex.reason)
			}
		}
		judged += len(examples)
	}
	if judged == 0 {
		t.Fatalf("no ```bash example under %s: the scan judged nothing", directivesDir)
	}
}

// The scanner must report the recipe gt-9j9vu replaced and leave the one that
// replaced it alone, or a directives tree it cannot read would look as clean as
// a correct one.
func TestDirectiveBeadTextExampleScan(t *testing.T) {
	t.Parallel()

	const oldRecipe = "## Bead text\n\n" +
		"```bash\n" +
		"cat > note.txt <<'EOF'\n" +
		"the text\n" +
		"EOF\n" +
		`gt bead note gt-abc "$(cat note.txt)"` + "\n" +
		"```\n"
	examples, err := directiveBeadTextExamples("old.md", oldRecipe)
	if err != nil {
		t.Fatalf("scanning the old recipe: %v", err)
	}
	if len(examples) != 1 || examples[0].reason != polecatUnresolvedTextReason {
		t.Fatalf("old recipe: got %+v, want one example refused as %q", examples, polecatUnresolvedTextReason)
	}

	const newRecipe = "```bash\n" +
		"gt bead note gt-abc 'line one\n" +
		"line two'\n" +
		"```\n"
	examples, err = directiveBeadTextExamples("new.md", newRecipe)
	if err != nil {
		t.Fatalf("scanning the new recipe: %v", err)
	}
	if len(examples) != 1 || examples[0].reason != "" {
		t.Fatalf("new recipe: got %+v, want one example the guard accepts", examples)
	}

	if _, err := directiveBeadTextExamples("open.md", "```bash\ngt bead note gt-abc 'x'\n"); err == nil {
		t.Error("an unclosed fence was not reported, so it was never judged")
	}
}
