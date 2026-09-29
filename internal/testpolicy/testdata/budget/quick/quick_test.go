// Package quick is a budget-runner fixture judged by the budget: it passes at
// once, well under any budget, and must run again on every run because the
// budget measures its test binary.
package quick

import "testing"

func TestPasses(t *testing.T) {}
