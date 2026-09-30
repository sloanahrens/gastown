package formula

import (
	"testing"
)

// sequentialFormula is a small workflow of sequential steps: a -> b -> c.
const sequentialFormula = `
formula = "mol-parallel-fixture"
version = 1

[[steps]]
id = "a"
title = "A"

[[steps]]
id = "b"
title = "B"
needs = ["a"]

[[steps]]
id = "c"
title = "C"
needs = ["b"]
`

func TestParallelReadySteps(t *testing.T) {
	f, err := Parse([]byte(sequentialFormula))
	if err != nil {
		t.Fatalf("Failed to parse formula: %v", err)
	}

	for _, id := range []string{"a", "b", "c"} {
		step := f.GetStep(id)
		if step == nil {
			t.Errorf("Step %s not found", id)
			continue
		}
		if step.Parallel {
			t.Errorf("Step %s should have parallel=false", id)
		}
	}

	// After a, the next sequential step is ready and nothing runs in parallel.
	parallel, sequential := f.ParallelReadySteps(map[string]bool{"a": true})
	if len(parallel) != 0 {
		t.Errorf("Expected 0 parallel steps, got %d: %v", len(parallel), parallel)
	}
	if sequential != "b" {
		t.Errorf("Expected sequential step b, got %s", sequential)
	}

	if c := f.GetStep("c"); c == nil || len(c.Needs) != 1 || c.Needs[0] != "b" {
		t.Errorf("c should need exactly b, got %+v", c)
	}
}
