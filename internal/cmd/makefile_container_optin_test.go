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
func TestMakefileHandsTheContainerOptInToTheSuite(t *testing.T) {
	t.Parallel()
	recipes := makefileRecipes(t, readRepoMakefile(t))
	for _, tc := range []struct {
		target, want, inherited string
	}{
		{"gate", "0", "1"},
		{"test-integration", "1", "0"},
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
		}
		if suiteLines == 0 {
			t.Errorf("the %s target runs no go test, so this test pinned nothing — if the recipe moved, move this test with it", tc.target)
		}
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
