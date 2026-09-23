package guardlint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoNewFailOpenGuards is this package's own gate (gt-udrrw item 2): it
// scans internal/ for the fail-open shape and fails on any finding not
// already recorded in baseline.txt. The known instances are grandfathered
// there, each a candidate for its own migration onto guard.Result — this
// test's job is narrower, to stop new instances of the shape from landing.
//
// Run as part of `make lint` rather than only `make test`, so a fail-open
// guard is caught at the same point golangci-lint would catch it, not left
// for a later `make test` a polecat might skip.
func TestNoNewFailOpenGuards(t *testing.T) {
	root := repoInternalDir(t)
	findings, err := Check(root)
	if err != nil {
		t.Fatalf("Check(%s): %v", root, err)
	}
	baseline, err := LoadBaseline(filepath.Join(root, "guardlint", "baseline.txt"))
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}

	var unbaselined []Finding
	for _, f := range findings {
		if !baseline[f.Key()] {
			unbaselined = append(unbaselined, f)
		}
	}
	if len(unbaselined) == 0 {
		return
	}

	var b strings.Builder
	for _, f := range unbaselined {
		b.WriteString(f.String())
		b.WriteString("\n")
	}
	t.Fatalf("new fail-open guard(s), not in internal/guardlint/baseline.txt:\n%s\n"+
		"Each returns a literal true or a swallowed nil from a branch that already held a non-nil error — "+
		"the caller can no longer tell \"could not check\" from \"checked and fine\" (gt-udrrw). "+
		"Return guard.Result instead (see internal/guard), or if this is a deliberate narrow-error case "+
		"(e.g. os.IsNotExist), add its key to baseline.txt with a one-line comment saying why.",
		b.String())
}

// repoInternalDir returns the internal/ directory this test's own package
// lives under, from the working directory `go test` sets for this package —
// so the gate finds internal/ correctly however the test is invoked (from
// the repo root via `go test ./internal/guardlint/...`, or directly from
// this package's own directory).
func repoInternalDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(wd)
}
