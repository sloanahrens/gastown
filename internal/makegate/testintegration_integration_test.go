//go:build integration

package makegate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// The test-integration cases: one test per case in lines 605-651 of
// scripts/makefile-gate_test.sh. Six run `make -n test-integration` in the
// checkout under test through RealMake: -n prints the recipe and runs nothing,
// so no case starts a container or reaches a network. The seventh exercises
// the no-filter check itself on the recipe it must reject. Every name carries
// the TestIntegrationTestIntegration prefix the bead's `-run` covers, and every
// comment names the mutation that turns the test red.
//
// The make calls are processes, so this file is integration tier
// (docs/standards/test-engineering.md, rule 2).

// runFilterPattern matches a recipe's `-run` name filter. A filter admits only
// the tests whose name it matches, which is how integration-tagged tests ran
// nowhere until gt-ik4a1.2 removed it.
var runFilterPattern = regexp.MustCompile(`(?m)(^|[[:space:]])-(test\.)?run[ =]`)

// noRunFilter reports whether out carries no `-run` name filter.
func noRunFilter(out string) bool { return !runFilterPattern.MatchString(out) }

// townSlotsPattern finds the town slot count in the channel that defines it, so
// raising the cap without the recipes fails the pool case.
var townSlotsPattern = regexp.MustCompile(`schedulerTownSlots = make\(chan struct\{\}, ([0-9]+)\)`)

// townSlots reads the town slot count from the channel that defines it. The
// file is read here rather than at the call site so a missing cap names itself
// instead of turning the pool case into a vacuous pass.
func townSlots(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "internal", "cmd", "scheduler_integration_test.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, the file that defines the town slot cap: %v", path, err)
	}
	m := townSlotsPattern.FindSubmatch(data)
	if m == nil {
		t.Fatalf("%s holds no `schedulerTownSlots = make(chan struct{}, N)` line, so no cap is read", path)
	}
	return string(m[1])
}

// initPoolLines counts the lines in out that set GT_TEST_DOLT_INIT_CONCURRENCY
// to exactly slots: as a whole assignment, so a longer value (80 for 8) is not a
// match.
func initPoolLines(out, slots string) int {
	want := regexp.MustCompile(`(^|[ \t])GT_TEST_DOLT_INIT_CONCURRENCY=` + regexp.QuoteMeta(slots) + `([ \t;]|$)`)
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if want.MatchString(line) {
			n++
		}
	}
	return n
}

// TestIntegrationTestIntegrationDryRunExitsZero checks "make -n test-integration
// exits 0".
// Mutation: a syntax error or a $(error ...) in test-integration's recipe makes
// make -n exit non-zero.
func TestIntegrationTestIntegrationDryRunExitsZero(t *testing.T) {
	dryTarget(t, RealMake(repoRoot(t)), "test-integration")
}

// TestIntegrationTestIntegrationRunsTheIntegrationTagWithContainersOn checks
// "test-integration runs the integration tag with containers on".
// Mutation: write GT_TEST_DOCKER=0 on both suite commands, and no line of the
// target turns containers on.
func TestIntegrationTestIntegrationRunsTheIntegrationTagWithContainersOn(t *testing.T) {
	out := dryTarget(t, RealMake(repoRoot(t)), "test-integration")
	if !strings.Contains(out, "-tags integration") || !strings.Contains(out, "GT_TEST_DOCKER=1") {
		t.Fatalf("test-integration does not run the integration tag with containers on\n%s", out)
	}
}

// TestIntegrationTestIntegrationPassesNoRunNameFilter checks "test-integration
// passes no -run name filter".
// Mutation: add `-run '^TestIntegration'` to a recipe line, and the filter
// leaves every integration-tagged test it does not match running nowhere.
func TestIntegrationTestIntegrationPassesNoRunNameFilter(t *testing.T) {
	out := dryTarget(t, RealMake(repoRoot(t)), "test-integration")
	if !noRunFilter(out) {
		t.Fatalf("test-integration passes a -run name filter, so the tier is not every integration-tagged test\n%s", out)
	}
}

// TestIntegrationTestIntegrationNoFilterCheckRejectsTheRecipeThatPassedRun checks
// "the no-filter check rejects the pre-gt-ik4a1.2 recipe that passed -run".
// Mutation: return true from noRunFilter unconditionally, and the recipe this
// case exists to catch passes the very check that guards the tier.
func TestIntegrationTestIntegrationNoFilterCheckRejectsTheRecipeThatPassedRun(t *testing.T) {
	const preGtIk4a12 = "GT_TEST_DOCKER=1 go test -tags integration -run '^TestIntegration' -timeout 20m ./..."
	if noRunFilter(preGtIk4a12) {
		t.Fatalf("the no-filter check admits %q, the recipe whose -run left every other integration-tagged test running nowhere", preGtIk4a12)
	}
}

// runsPackage reports whether a recipe line in out names pkg as an argument of
// a go test command. A mention before "go test" runs no test: the recipe's
// guard line names the packages and only echoes "go test" in its refusal text,
// so a substring match over the output would read that as a run.
func runsPackage(out, pkg string) bool {
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(line, "go test")
		if i < 0 {
			continue
		}
		for _, field := range strings.Fields(line[i+len("go test"):]) {
			if field == pkg {
				return true
			}
		}
	}
	return false
}

// TestIntegrationTestIntegrationRunsEveryPackageInDockerTxt checks
// "test-integration runs every package in internal/testpolicy/docker.txt".
// Mutation: drop the recipe line taking $(DOCKER_PKGS), and each package the
// list names is left on the guard line, which runs no test.
func TestIntegrationTestIntegrationRunsEveryPackageInDockerTxt(t *testing.T) {
	root := repoRoot(t)
	out := dryTarget(t, RealMake(root), "test-integration")
	path := filepath.Join(root, "internal", "testpolicy", "docker.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s, the list of container packages: %v", path, err)
	}
	pkgs := tiersweep.DockerPackages(string(data))
	if len(pkgs) == 0 {
		t.Fatalf("%s lists no package, so this case would assert nothing", path)
	}
	var missing []string
	for _, pkg := range pkgs {
		if !runsPackage(out, pkg) {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("test-integration runs no recipe line for %v, which %s lists as a container package\n%s", missing, path, out)
	}
}

// TestIntegrationTestIntegrationVetsAndTestsTheTrackerModule checks
// "test-integration also vets and tests the tracker module with the integration
// tag".
// Mutation: drop the `for m in $(TESTED_MODULES)` recipe line, and the tracker
// module's postgres integration tier runs nowhere.
func TestIntegrationTestIntegrationVetsAndTestsTheTrackerModule(t *testing.T) {
	out := dryTarget(t, RealMake(repoRoot(t)), "test-integration")
	// The behaviour is that the tracker module is vetted and tested with the
	// integration tag; the package pattern and the timeout are not pinned.
	loop := regexp.MustCompile(`(?s)for m in tracker; do.*?go vet -tags integration .*?go test -tags integration `)
	if !loop.MatchString(out) {
		t.Fatalf("test-integration does not vet and test the tracker module with the integration tag\n%s", out)
	}
}

// TestIntegrationTestIntegrationSetsTheTestDoltInitPoolToTheTownSlotCount checks
// "test-integration sets the test-Dolt init pool to the town slot count on both
// commands".
// Mutation: drop GT_TEST_DOLT_INIT_CONCURRENCY from one suite command, and the
// count is 1; change the number without the channel, and neither line matches.
func TestIntegrationTestIntegrationSetsTheTestDoltInitPoolToTheTownSlotCount(t *testing.T) {
	root := repoRoot(t)
	out := dryTarget(t, RealMake(root), "test-integration")
	slots := townSlots(t, root)
	if got := initPoolLines(out, slots); got != 2 {
		t.Fatalf("test-integration sets GT_TEST_DOLT_INIT_CONCURRENCY=%s on %d recipe lines, want 2, one per suite command\n%s", slots, got, out)
	}
}
