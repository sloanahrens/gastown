package land

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shippedRubricPath is the repo-root .om.json: the rubric this repo's landing
// worker hands to om. The path is resolved from this test file, so it does not
// depend on the test's working directory.
func shippedRubricPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", ".om.json")
}

// omCriterion is one rubric entry. Weight is a pointer so a missing or
// non-numeric weight is a finding rather than a silent zero.
type omCriterion struct {
	Name     string   `json:"name"`
	Weight   *float64 `json:"weight"`
	Guidance string   `json:"guidance"`
}

type omRubric struct {
	Rubric []omCriterion `json:"rubric"`
}

func shippedRubric(t *testing.T) omRubric {
	t.Helper()
	path := shippedRubricPath(t)
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path this test builds from its own file
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var r omRubric
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return r
}

// TestShippedOMRubricIsGradeable pins the shape om reads: the repo ships its
// own rubric, and a criterion missing a name, a weight, or guidance is a
// criterion no reviewer can apply or cite. The gate has no other check on
// .om.json — om parses the file itself, and a rubric that stopped parsing
// would be reported as a review that could not run, not as a red tree.
func TestShippedOMRubricIsGradeable(t *testing.T) {
	t.Parallel()
	r := shippedRubric(t)
	if len(r.Rubric) == 0 {
		t.Fatalf("%s carries no rubric criteria: om would grade every landing against nothing", shippedRubricPath(t))
	}
	for i, c := range r.Rubric {
		where := fmt.Sprintf("rubric[%d]", i)
		if c.Name != "" {
			where = fmt.Sprintf("criterion %q", c.Name)
		}
		if strings.TrimSpace(c.Name) == "" {
			t.Errorf("%s has no name", where)
		}
		if c.Weight == nil {
			t.Errorf("%s has no numeric weight", where)
		}
		if strings.TrimSpace(c.Guidance) == "" {
			t.Errorf("%s has no guidance", where)
		}
	}
}

// instructionProliferationCriterion is the criterion gt-1zff added: a new copy
// of an instruction that already exists is itself the finding, before the
// copies drift. docs-and-comments reaches the same class only after the copies
// disagree, so this pin keeps the two apart: a reword that keeps the name but
// lets "the copies agree today" answer the finding would collapse it back into
// docs-and-comments.
const instructionProliferationCriterion = "instruction-proliferation"

// TestShippedOMRubricCarriesInstructionProliferationCriterion pins the
// criterion and the things that make it gradeable: its weight, its place after
// docs-and-comments, and guidance that refuses the agreeing-copies defence and
// names the rule a reviewer cites.
func TestShippedOMRubricCarriesInstructionProliferationCriterion(t *testing.T) {
	t.Parallel()
	r := shippedRubric(t)
	idx := -1
	for i, c := range r.Rubric {
		if c.Name == instructionProliferationCriterion {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the shipped rubric %s carries no %q criterion: the class gt-1zff gated (an added copy of an existing instruction scores clean until the copies drift, and the later change that touches one is charged for it) is ungraded until it is restored", shippedRubricPath(t), instructionProliferationCriterion)
	}
	for i, c := range r.Rubric {
		if c.Name == "docs-and-comments" && i > idx {
			t.Errorf("%s appears before docs-and-comments in %s; gt-1zff appends it after, so the two read in the order they apply", instructionProliferationCriterion, shippedRubricPath(t))
		}
	}
	c := r.Rubric[idx]
	if c.Weight == nil || *c.Weight != 2 {
		t.Errorf("%s weight = %v, want 2: gt-1zff weights it to move the score without blocking alone, the shape docs-and-comments already has", instructionProliferationCriterion, c.Weight)
	}
	if !strings.Contains(c.Guidance, "agree") {
		t.Errorf("%s guidance no longer refuses the defence that the copies currently agree: %q", instructionProliferationCriterion, c.Guidance)
	}
	if !strings.Contains(c.Guidance, "source of truth") {
		t.Errorf("%s guidance no longer names the pointer that clears a copy as its source of truth: %q", instructionProliferationCriterion, c.Guidance)
	}
	if !strings.Contains(c.Guidance, "R2") {
		t.Errorf("%s guidance no longer points at the rule it enforces (R2, one place per meaning), so a reviewer has nothing to cite: %q", instructionProliferationCriterion, c.Guidance)
	}
}
