package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// adviceLintSpec is a clean-shaped bead whose Goal names a probe and whose
// acceptance list says nothing about a failure: the lint's advice, not its
// refusal, is what it exercises.
func adviceLintSpec(id string) specdispatch.Spec {
	return specdispatch.Spec{
		ID:    id,
		Type:  "task",
		Title: "lint advice fixture",
		Description: "## Goal\nAdd a probe that checks the seat list.\n\n" +
			"## Constraints\nGo.\n\n" +
			"## Out of scope\nNone.\n\n" +
			"## Gate\nmake gate\n\n" +
			"## Size\none worker, one MR",
		Acceptance: "- [ ] the probe runs on a tick",
	}
}

// TestSpecLintAdvicePrintsAndKeepsExit pins the human form: a clean bead that
// triggers the failure-behavior advisory prints "advice: " on stderr after
// the verdict and still exits 0.
func TestSpecLintAdvicePrintsAndKeepsExit(t *testing.T) {
	t.Parallel()
	spec := adviceLintSpec("gt-adv")
	var out, errOut bytes.Buffer
	if err := specLint(&out, &errOut, spec.ID, spec, nil, "", nil, false); err != nil {
		t.Fatalf("clean bead with advice returned %v, want exit 0", err)
	}
	if got, want := strings.TrimSpace(out.String()), "gt-adv: spec lint ok"; got != want {
		t.Errorf("verdict = %q, want %q", got, want)
	}
	if !strings.Contains(errOut.String(), "advice: ") {
		t.Errorf("stderr is missing the advice line:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "## Goal") {
		t.Errorf("a clean bead wrote the skeleton to stderr:\n%s", errOut.String())
	}
}

// TestSpecLintAdviceJSON pins the report: --json carries advice as an array
// and omits the key when there is none.
func TestSpecLintAdviceJSON(t *testing.T) {
	t.Parallel()
	t.Run("carries advice", func(t *testing.T) {
		t.Parallel()
		spec := adviceLintSpec("gt-adv")
		var out, errOut bytes.Buffer
		if err := specLint(&out, &errOut, spec.ID, spec, nil, "", nil, true); err != nil {
			t.Fatalf("clean bead with advice returned %v, want exit 0", err)
		}
		if errOut.Len() != 0 {
			t.Errorf("--json wrote advice to stderr: %q", errOut.String())
		}
		var got specLintReport
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output is not JSON: %v\n%s", err, out.String())
		}
		if len(got.Advice) != 1 {
			t.Fatalf("advice = %q, want one line", got.Advice)
		}
	})
	t.Run("omits an empty advice list", func(t *testing.T) {
		t.Parallel()
		spec := adviceLintSpec("gt-quiet")
		spec.Description = strings.Replace(spec.Description, "Add a probe that checks the seat list.", "Add a column to the board.", 1)
		var out, errOut bytes.Buffer
		if err := specLint(&out, &errOut, spec.ID, spec, nil, "", nil, true); err != nil {
			t.Fatalf("clean bead returned %v, want exit 0", err)
		}
		if strings.Contains(out.String(), `"advice"`) {
			t.Errorf("empty advice was serialized: %s", out.String())
		}
	})
}
