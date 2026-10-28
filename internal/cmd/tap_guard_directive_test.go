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

// beadTextJudge judges one command the way the tap guard does when a polecat
// runs it, returning the reason a rule refuses the command for and the
// alternative the hook prints beneath it. Both empty is an acceptance.
type beadTextJudge func(command string) (reason, alternative string)

// judgeAsPolecat is the judge a directive's example meets: the production hook
// entry point, in a polecat session, so the fence is read by every rule the
// real invocation meets and not just the bead-text one.
func judgeAsPolecat(command string) (reason, alternative string) {
	return evaluateDangerousCommand(command, 0, attributionPolecat)
}

// beadTextExample is one ```bash fence of a directive, as the tap guard judged
// it: the line its opening fence sits on and the verdict the polecat rule gave,
// both parts of it, so the refusal is reported with the alternative that states
// the fix.
type beadTextExample struct {
	file        string
	line        int
	reason      string
	alternative string
}

// directiveBeadTextExamples runs each ```bash fence of one directive's markdown
// through judge, the way the hook reads the command a polecat runs (gt-9j9vu).
// An unterminated fence is an error, not an empty example list: a fence this
// scanner fails to close is one it never judged. A verdict with only one of its
// two parts is an error for the same reason — the pair is the verdict, so half
// of one cannot be read as an acceptance.
func directiveBeadTextExamples(judge beadTextJudge, file, markdown string) ([]beadTextExample, error) {
	var examples []beadTextExample
	var body []string
	open := 0
	for i, line := range strings.Split(markdown, "\n") {
		switch {
		case open == 0 && strings.TrimSpace(line) == "```bash":
			open, body = i+1, body[:0]
		case open != 0 && strings.TrimSpace(line) == "```":
			reason, alternative := judge(strings.Join(body, "\n"))
			if (reason == "") != (alternative == "") {
				return examples, fmt.Errorf("%s:%d: the guard gave reason %q with alternative %q: a half verdict is not an acceptance, so this fence was never judged",
					file, open, reason, alternative)
			}
			examples = append(examples, beadTextExample{file: file, line: open, reason: reason, alternative: alternative})
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
		examples, err := directiveBeadTextExamples(judgeAsPolecat, path, string(data))
		if err != nil {
			t.Error(err)
			continue
		}
		for _, ex := range examples {
			if ex.reason != "" {
				t.Errorf("%s:%d: the tap guard refuses this example: %s — %s; a ```bash fence is a recipe a polecat runs, so show a refused form as inline code",
					ex.file, ex.line, ex.reason, ex.alternative)
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
	examples, err := directiveBeadTextExamples(judgeAsPolecat, "old.md", oldRecipe)
	if err != nil {
		t.Fatalf("scanning the old recipe: %v", err)
	}
	if len(examples) != 1 || examples[0].reason != polecatUnresolvedTextReason || examples[0].alternative == "" {
		t.Fatalf("old recipe: got %+v, want one example refused as %q with its alternative", examples, polecatUnresolvedTextReason)
	}

	const newRecipe = "```bash\n" +
		"gt bead note gt-abc 'line one\n" +
		"line two'\n" +
		"```\n"
	examples, err = directiveBeadTextExamples(judgeAsPolecat, "new.md", newRecipe)
	if err != nil {
		t.Fatalf("scanning the new recipe: %v", err)
	}
	if len(examples) != 1 || examples[0].reason != "" || examples[0].alternative != "" {
		t.Fatalf("new recipe: got %+v, want one example the guard accepts", examples)
	}

	if _, err := directiveBeadTextExamples(judgeAsPolecat, "open.md", "```bash\ngt bead note gt-abc 'x'\n"); err == nil {
		t.Error("an unclosed fence was not reported, so it was never judged")
	}

	// A verdict with one part missing is unreadable, so the example must not
	// count as accepted: the alternative alone and the reason alone are both
	// half a verdict.
	acceptsWithAlternative := func(string) (string, string) { return "", polecatCommentInputAlt }
	if _, err := directiveBeadTextExamples(acceptsWithAlternative, "half.md", newRecipe); err == nil {
		t.Error("an alternative with no reason was read as acceptance, so an unjudged example passed")
	}
	refusesWithNoAlternative := func(string) (string, string) { return polecatCommentInputReason, "" }
	if _, err := directiveBeadTextExamples(refusesWithNoAlternative, "half.md", newRecipe); err == nil {
		t.Error("a reason with no alternative was read without complaint; the verdict pair is incomplete")
	}
}
