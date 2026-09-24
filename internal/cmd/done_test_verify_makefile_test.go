package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// makefileOptInAssignment is the opt-in assignment a suite recipe carries, e.g.
// GT_TEST_DOCKER=${GT_TEST_DOCKER:-1}. The value is one token in every form
// this Makefile uses, and the token is what the behavioral half of the test
// below re-executes in a real shell.
var makefileOptInAssignment = regexp.MustCompile(`GT_TEST_DOCKER=(\S+)`)

// TestMakefileHandsTheContainerOptInToTheSuite pins the Makefile link that
// gt done's slot-free path rests on (gt-wx53, gt-0ss4).
//
// The gate decides it can run a rig's suite WITHOUT the town-wide
// container-gate slot because the rig's command does not ask for containers,
// and it writes GT_TEST_DOCKER=0 into that run's environment. That decision is
// only safe while the rig's own recipe hands that inherited value on to
// `go test`: gastown's `make test` defaults the variable to 1, so a recipe
// that hardcoded the opt-in — `GT_TEST_DOCKER=1 go test ./...` — would make the
// gate's =0 a no-op, and every slot-free gate run would start the container
// suite beside whatever the rest of the town is doing on the shared Docker VM,
// silently. The same change would also break the refinery's gate, which
// relies on the default being 1.
//
// So the property is behavioral, not textual: with an inherited
// GT_TEST_DOCKER=0 the recipe must hand 0 to the command it prefixes, and with
// none inherited it must hand 1. Both halves are read out of the recipe line
// the Makefile actually has, run through sh.
func TestMakefileHandsTheContainerOptInToTheSuite(t *testing.T) {
	recipes := makefileRecipes(t, readRepoMakefile(t))
	for _, target := range []string{"test", "test-changed"} {
		lines := recipes[target]
		if len(lines) == 0 {
			t.Fatalf("no recipe for the %s target — this test cannot pin a property of a target it cannot find", target)
		}
		suiteLines := 0
		for _, line := range lines {
			if !strings.Contains(line, "go test") {
				continue
			}
			suiteLines++
			assignment := makefileOptInAssignment.FindStringSubmatch(line)
			if assignment == nil {
				t.Errorf("%s runs go test without deciding the container opt-in, so an inherited %s cannot reach it:\n\t%s", target, dockerTestsEnv, line)
				continue
			}
			if !strings.Contains(assignment[1], "$") {
				t.Errorf("%s hardcodes the container opt-in (%s=%s). An inherited %s=0 then cannot turn it off, so every slot-free `gt done` gate run starts the container suite outside the slot (gt-0ss4). Default it from the environment instead:\n\t%s",
					target, dockerTestsEnv, assignment[1], dockerTestsEnv, line)
				continue
			}
			if got := optInReachingTheSuite(t, assignment[1], "0"); got != "0" {
				t.Errorf("%s with an inherited %s=0 hands %s=%s to the suite, want 0 — the slot-free gate path is running the container suite after writing the opt-in off:\n\t%s",
					target, dockerTestsEnv, dockerTestsEnv, got, line)
			}
			if got := optInReachingTheSuite(t, assignment[1], ""); got != "1" {
				t.Errorf("%s with no inherited %s hands %s=%s to the suite, want 1 (the refinery's gate and the daemon's main-branch patrol pass no value and rely on the default):\n\t%s",
					target, dockerTestsEnv, dockerTestsEnv, got, line)
			}
		}
		if suiteLines == 0 {
			t.Errorf("the %s target runs no go test, so this test pinned nothing — if the recipe moved, move this test with it", target)
		}
	}
}

// optInReachingTheSuite runs the recipe's own assignment in a real shell and
// returns the value the command it prefixes would see.
//
// make escapes a dollar as `$$`, so the loop above sees ${GT_TEST_DOCKER:-1}
// as $${GT_TEST_DOCKER:-1} and this undoes exactly that escape before handing
// the text to sh — the same two characters make strips before the recipe runs.
func optInReachingTheSuite(t *testing.T, assignmentRHS, inherited string) string {
	t.Helper()
	assignment := dockerTestsEnv + "=" + strings.ReplaceAll(assignmentRHS, "$$", "$")
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	env := []string{"PATH=" + path}
	if inherited != "" {
		env = append(env, dockerTestsEnv+"="+inherited)
	}
	cmd := exec.Command("sh", "-c", assignment+" env") //nolint:gosec // G204: the recipe line this test just read, no other input
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the recipe's assignment %q: %v", assignment, err)
	}
	// Last wins: a shell's assignment prefix overrides an inherited value, so
	// the resolved entry is the last one env printed for the name.
	value := ""
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, dockerTestsEnv+"="); ok {
			value = v
		}
	}
	if value == "" {
		t.Fatalf("the recipe's assignment %q left no %s in the command's environment:\n%s", assignment, dockerTestsEnv, out)
	}
	return value
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
