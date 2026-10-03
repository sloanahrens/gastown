package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makefileOptInAssignment is the opt-in assignment a suite recipe carries, e.g.
// GT_TEST_DOCKER=0. The value is one token in every form this Makefile uses,
// and the token is what the behavioral half of the test below re-executes in
// a real shell.
var makefileOptInAssignment = regexp.MustCompile(`GT_TEST_DOCKER=(\S+)`)

// initPoolEnv is the test-Dolt init pool override package beads reads at init()
// (internal/beads/test_container.go). It must be in the environment before the
// test binary starts, so the container make targets set it (gt-ik4a1.4.12),
// and the literal they set is pinned against the town slot count below.
const initPoolEnv = "GT_TEST_DOLT_INIT_CONCURRENCY"

// makefileInitPoolAssignment is that pool assignment as a suite recipe writes
// it, e.g. GT_TEST_DOLT_INIT_CONCURRENCY=4.
var makefileInitPoolAssignment = regexp.MustCompile(initPoolEnv + `=(\S+)`)

// schedulerTownSlotsSource is the declaration of the scheduler integration
// tier's town slot count — the width that bounds how many scheduler towns run
// at once. Its file is integration-tagged, so this unit-tier test cannot read
// the variable; it reads the number out of the declaration instead, which is
// what keeps the Makefile's literal equal to it.
var schedulerTownSlotsSource = regexp.MustCompile(`schedulerTownSlots = make\(chan struct\{\}, ([0-9]+)\)`)

// schedulerTownSlotCount returns that count, or fails the test.
func schedulerTownSlotCount(t *testing.T) string {
	t.Helper()
	const slotsFile = "scheduler_integration_test.go"
	b, err := os.ReadFile(slotsFile)
	if err != nil {
		t.Fatalf("reading %s (working directory %s): %v", slotsFile, mustGetwd(t), err)
	}
	m := schedulerTownSlotsSource.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("%s declares no schedulerTownSlots = make(chan struct{}, N); if the declaration moved or changed shape, move this test with it", slotsFile)
	}
	return m[1]
}

// TestMakefileHandsTheContainerOptInToTheSuite pins the Makefile link that
// gt done's slot-free path rests on (gt-wx53, gt-0ss4), in its D9 form
// (gt-ik4a1.1).
//
// The gate decides it can run a rig's suite WITHOUT the town-wide
// container-gate slot because the rig's command does not ask for containers,
// and it writes GT_TEST_DOCKER=0 into that run's environment. gastown's
// command is `make gate`, whose recipe writes the opt-in off itself, so the
// unit tier never starts a container whoever runs it: an inherited =1 must
// not reach the suite either. `make test-integration` is the other half: it
// writes the opt-in on, so the Docker-backed packages' container tests run
// there, and an inherited =0 must not skip them.
//
// So each suite line's assignment must be a literal: a shell assignment
// prefix overrides an inherited value, and one that expands a variable
// ($${GT_TEST_DOCKER:-1}) lets the inherited value decide.
//
// The container tiers carry a second literal, the test-Dolt init pool
// (gt-ik4a1.4.12): package beads reads GT_TEST_DOLT_INIT_CONCURRENCY in init()
// and otherwise caps test-Dolt inits at 4, so it has to be set before the test
// binary starts and it must track the scheduler town slot count or town setup
// queues as soon as that cap rises above the pool.
func TestMakefileHandsTheContainerOptInToTheSuite(t *testing.T) {
	t.Parallel()
	recipes := makefileRecipes(t, readRepoMakefile(t))
	townSlots := schedulerTownSlotCount(t)
	for _, tc := range []struct {
		target, want, inherited string
		// containers marks the tiers that start the container suite: their
		// suite lines must pin the test-Dolt init pool to townSlots.
		containers bool
	}{
		// make gate's suite lives in its test stage, gate-test (gt-b5ugw).
		{"gate-test", "0", "1", false},
		{"test-integration", "1", "0", true},
		{"test-integration-wall", "1", "0", true},
	} {
		lines := recipes[tc.target]
		if len(lines) == 0 {
			t.Fatalf("no recipe for the %s target — this test cannot pin a property of a target it cannot find", tc.target)
		}
		suiteLines := 0
		for _, line := range lines {
			if trimmed := strings.TrimLeft(line, "@-+ "); strings.HasPrefix(trimmed, "#") {
				continue // a recipe comment, not a command
			}
			// A suite line is one that starts the tests: the gate's recipe runs
			// `go test -timeout 20m ./...` directly (through the test cache,
			// gt-s1vff), tier-check the budget wrapper
			// (internal/testpolicy/cmd/budget), and the integration recipe
			// $(INTEGRATION_GO_TEST), go test by default. A line that merely
			// mentions go test, such as a guard's message, is not one.
			if !strings.Contains(line, "go test -timeout 20m ./...") && !strings.Contains(line, "go run ./internal/testpolicy/cmd/budget") && !strings.Contains(line, "$(INTEGRATION_GO_TEST) ") {
				continue
			}
			suiteLines++
			assignment := makefileOptInAssignment.FindStringSubmatch(line)
			if assignment == nil {
				t.Errorf("%s runs go test without deciding the container opt-in, so an inherited %s decides it:\n\t%s", tc.target, dockerTestsEnv, line)
				continue
			}
			if got := assignment[1]; strings.Contains(got, "$") || got != tc.want {
				t.Errorf("%s hands %s=%s to the suite, want the literal %s so an inherited %s=%s cannot decide it:\n\t%s",
					tc.target, dockerTestsEnv, got, tc.want, dockerTestsEnv, tc.inherited, line)
			}
			if tc.containers {
				checkInitPool(t, tc.target, line, townSlots)
			}
		}
		if suiteLines == 0 {
			t.Errorf("the %s target runs no go test, so this test pinned nothing — if the recipe moved, move this test with it", tc.target)
		}
	}
}

// checkInitPool pins one container suite line's test-Dolt init pool to the
// scheduler town slot count.
func checkInitPool(t *testing.T, target, line, townSlots string) {
	t.Helper()
	pool := makefileInitPoolAssignment.FindStringSubmatch(line)
	if pool == nil {
		t.Errorf("%s runs the container suite without setting %s, so the test-Dolt init pool keeps its default and caps town setup at that width (gt-ik4a1.4.12):\n\t%s",
			target, initPoolEnv, line)
		return
	}
	if got := pool[1]; strings.Contains(got, "$") || got != townSlots {
		t.Errorf("%s hands %s=%s to the suite, want the literal %s, the scheduler town slot count:\n\t%s",
			target, initPoolEnv, got, townSlots, line)
	}
}

// readRepoMakefile reads the repository's Makefile. Tests run with the
// working directory `go test` sets for this package, so the repo root is two
// levels up — the same path internal/guardlint and internal/plugin use.
func readRepoMakefile(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "Makefile")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s (working directory %s): %v", path, mustGetwd(t), err)
	}
	return string(b)
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return wd
}

// makefileRecipes returns each target's recipe lines, keyed by target name.
//
// A deliberately small parser: this test reads one property out of two
// targets, and the Makefile's own conditionals live in targets it does not
// name. A tab-prefixed line belongs to the target most recently seen; any
// other line ends that recipe, and a line whose first field has no `=` is a
// new target.
func makefileRecipes(t *testing.T, src string) map[string][]string {
	t.Helper()
	recipes := map[string][]string{}
	current := ""
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "\t") {
			if current != "" {
				recipes[current] = append(recipes[current], strings.TrimPrefix(line, "\t"))
			}
			continue
		}
		current = ""
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, _, ok := strings.Cut(line, ":"); ok && !strings.Contains(name, "=") {
			current = strings.TrimSpace(name)
		}
	}
	return recipes
}
