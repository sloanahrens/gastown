//go:build integration

package makegate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// cpuClassWrapper is the invocation that fronts a gate or presubmit Go step
// with the CPU class (gt-bq68w), as the Makefile's own default spells it.
const cpuClassWrapper = "go run ./internal/slot/cmd/cpuslot"

// cpuClassStep matches a recipe line running a Go build, vet or test step: the
// step is the line's own command or one inside its subshell, not a `go run`
// helper beside it.
var cpuClassStep = regexp.MustCompile(`(^|[\s(])go (build|vet|test) `)

// cpuClassBuild is the one build of the wrapper a test process makes: every
// case that runs it for real shares it.
var (
	cpuClassOnce sync.Once
	cpuClassBin  string
	cpuClassErr  error
)

// TestMain removes the wrapper the CPU-class cases built.
func TestMain(m *testing.M) {
	code := m.Run()
	if cpuClassBin != "" {
		_ = os.RemoveAll(filepath.Dir(cpuClassBin))
	}
	os.Exit(code)
}

// cpuClassBinary builds the cpuslot wrapper from the checkout under test with
// the host's go (not the cases' stub) and returns its path.
func cpuClassBinary(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	cpuClassOnce.Do(func() {
		dir, err := os.MkdirTemp("", "makegate-cpuslot-")
		if err != nil {
			cpuClassErr = err
			return
		}
		cpuClassBin = filepath.Join(dir, "cpuslot")
		cmd := exec.Command("go", "build", "-o", cpuClassBin, "./internal/slot/cmd/cpuslot")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			cpuClassErr = fmt.Errorf("build the cpuslot wrapper: %v\n%s", err, out)
		}
	})
	if cpuClassErr != nil {
		t.Fatalf("%v", cpuClassErr)
	}
	return cpuClassBin
}

// TestIntegrationCPUClassWrapsOnlyInACrewCheckout checks that the same Makefile
// fronts its go build, vet and test steps with the CPU-class wrapper when make
// runs from a crew checkout, and leaves them untouched anywhere else — the path
// a polecat's sandbox, the landing worker's gate and CI run.
// Mutation: make CPU_CLASS unconditional, and the non-crew run names the
// wrapper; make it always empty, and the crew run does not.
func TestIntegrationCPUClassWrapsOnlyInACrewCheckout(t *testing.T) {
	root := repoRoot(t)
	makefile := []byte(makefileText(t, root))
	cases := []struct {
		name    string
		dir     func(*testing.T) string
		wrapped bool
	}{
		{
			name: "crew checkout",
			dir: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "gastown", "crew", "olive")
			},
			wrapped: true,
		},
		{
			name: "outside a crew checkout",
			dir:  func(t *testing.T) string { return t.TempDir() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir(t)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), makefile, 0o644); err != nil {
				t.Fatal(err)
			}
			out := dryTarget(t, defaultsMake(t, dir), "gate")
			steps := 0
			for _, line := range strings.Split(out, "\n") {
				// An echo line names the step it is about to run; it runs nothing.
				if strings.HasPrefix(strings.TrimSpace(line), "echo ") || !cpuClassStep.MatchString(line) {
					continue
				}
				steps++
				if got := strings.Contains(line, cpuClassWrapper); got != tc.wrapped {
					t.Errorf("go step wrapped = %v, want %v: %s", got, tc.wrapped, strings.TrimSpace(line))
				}
			}
			if steps == 0 {
				t.Fatalf("the gate's dry run holds no go build, vet or test step, so this case would pass on an empty recipe\n%s", out)
			}
		})
	}
}

// TestIntegrationCPUClassFrontsTheGatesStepsInACrewCheckout runs the real gate
// from a crew checkout with the real wrapper: the stub go logs the GOFLAGS the
// wrapper set, so every go build, vet and test step is seen to have run under
// the class.
// Mutation: drop $(CPU_CLASS) from a step, and that step's call has no GOFLAGS
// line.
func TestIntegrationCPUClassFrontsTheGatesStepsInACrewCheckout(t *testing.T) {
	g := newGateSandbox(t)
	dir := g.Checkout(filepath.Join("gastown", "crew", "olive"))
	g.repo = dir
	// The town the wrapper walks up to from the crew checkout.
	g.WriteFile("mayor/town.json", "{}")
	g.Commit(dir, "gate", map[string]string{"Makefile": makefileText(t, repoRoot(t))})

	r := g.Run([]string{"-o", "lint"}, "CPU_CLASS="+cpuClassBinary(t), "GOFLAGS=")
	if r.Code != 0 {
		t.Fatalf("the crew gate exited %d on green stubs, want 0\nstdout:\n%s\nstderr:\n%s\ncalls:\n%s", r.Code, r.Stdout, r.Stderr, r.callLog())
	}
	for _, step := range []string{"go build GOFLAGS=-p=4", "go test GOFLAGS=-p=4"} {
		if !r.hasCall(step) {
			t.Errorf("a gate step ran outside the CPU class: no %q call\ncalls:\n%s", step, r.callLog())
		}
	}
}
