package sweeprunner

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// goldenReadme describes the fixture directory and is not a listing a test can
// open.
const goldenReadme = "README.md"

// TestMain runs the unit-tier checks, then the golden-fixture coverage check
// over what the tests opened: a listing in testdata/live-cluster that no test
// loaded is one nothing holds to the code it was captured for (gt-gg9hv.52).
func TestMain(m *testing.M) {
	code := unittier.Main(m)
	if code == 0 {
		applies, err := goldenCoverageApplies()
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "golden fixture coverage check: %v\n", err)
			code = 1
		case applies:
			if err := checkGoldenFixturesOpened(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				code = 1
			}
		}
	}
	os.Exit(code)
}

// goldenCoverageApplies reports whether this run started the package's tests
// unfiltered, so every golden test ran. A run narrowed with -run, shortened
// with -short so a test may skip itself, or asked only to -list its tests need
// not open every fixture, and the coverage check would blame the rest. A test
// flag that cannot be read is an error rather than a skip: a check that cannot
// tell whether it applies must not pass (gt-gg9hv.52).
func goldenCoverageApplies() (bool, error) {
	for _, name := range []string{"test.run", "test.list"} {
		narrowing := flag.Lookup(name)
		if narrowing == nil {
			return false, fmt.Errorf("the test binary registers no -%s flag", name)
		}
		if narrowing.Value.String() != "" {
			return false, nil
		}
	}
	if testing.Short() {
		return false, nil
	}
	return true, nil
}

// checkGoldenFixturesOpened fails when a file in testdata/live-cluster was
// opened by no test: README.md describes the directory, and every other file,
// whatever its extension, is a listing a golden test must load (gt-gg9hv.52).
func checkGoldenFixturesOpened() error {
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		return fmt.Errorf("reading the live-cluster fixtures: %w", err)
	}
	var unopened []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == goldenReadme || goldenFixtureWasOpened(name) {
			continue
		}
		unopened = append(unopened, name)
	}
	if len(unopened) == 0 {
		return nil
	}
	slices.Sort(unopened)
	return fmt.Errorf("%s holds files no test opened: %s; every file there but %s must be loaded by loadGoldenFixture", goldenDir, strings.Join(unopened, ", "), goldenReadme)
}
