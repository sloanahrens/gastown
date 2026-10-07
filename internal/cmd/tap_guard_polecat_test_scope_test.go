package cmd

import (
	"strconv"
	"strings"
	"testing"
)

// The rule and its boundaries: whole-repo and unfiltered heavy-package runs
// block; filtered runs, light packages and the refinery's own commands pass.
// The count assertion fails a version that blocks everything or nothing.
func TestEvaluatePolecatTestScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"whole repo", "go test ./...", true},
		{"whole repo, counted", "go test -count=1 ./...", true},
		{"whole internal/cmd", "go test ./internal/cmd/", true},
		{"whole internal/cmd with dots", "go test ./internal/cmd/...", true},
		{"whole internal/daemon counted", "go test ./internal/daemon/ -count=1", true},
		{"two heavy packages", "go test ./internal/polecat/ ./internal/cmd/ -count=1", true},
		{"heavy package wrapped in slot run", "gt slot run --role gastown/flint -- go test ./internal/polecat/ -count=1", true},
		{"heavy package after another segment", "gofmt -l . && go test ./internal/daemon/", true},
		{"subpackage of a heavy package", "go test ./internal/cmd/sub/", true},
		{"ancestor wildcard covering heavy packages", "go test ./internal/...", true},
		{"bare ancestor wildcard", "go test internal/...", true},
		{"module-prefixed whole repo wildcard", "go test github.com/steveyegge/gastown/...", true},
		{"module-prefixed heavy package", "go test github.com/steveyegge/gastown/internal/cmd/", true},
		// make test is `go test ./...` by another name; the wrapper and an env
		// prefix change nothing about the host CPU it burns (gt-v6se).
		{"make test", "make test", true},
		{"make test with env prefix", "GOFLAGS=-p=8 make test", true},
		{"make test wrapped in slot run", "gt slot run --role gastown/zircon -- GOFLAGS=-p=8 make test", true},
		{"make test with jobs flag", "make -j4 test", true},
		{"make test with long flag", "make --jobs=4 test", true},
		{"make test with directory flag and value", "make -C . test", true},
		{"make test with valueless flags", "make -e -w -i test", true},
		{"make test with bare jobs flag", "make -j test", true},
		{"make test with spaced jobs value", "make -j 4 test", true},
		{"make test with keep-going and load flags", "make -k -l test", true},
		{"make test with long directory option and spaced value", "make --directory . test", true},

		{"filtered heavy package", "go test ./internal/cmd/ -run 'TestApplyMQCheck|TestSlingDeadAgent'", false},
		{"filtered heavy package, -run= form", "go test -run=TestFoo ./internal/daemon/", false},
		{"filtered two heavy packages", "go test ./internal/cmd/ ./internal/polecat/ -run TestFoo -count=1", false},
		{"filtered heavy package in slot run", "gt slot run --role gastown/flint -- go test ./internal/daemon/ -run TestFeedFirstReady", false},
		{"filter passed to the test binary after --", "go test ./internal/cmd/ -- -test.run TestFoo", false},
		{"whole light package", "go test ./internal/git/ -count=1", false},
		{"whole light packages", "go test ./internal/style/... ./internal/config/", false},
		{"go vet", "go vet ./internal/cmd/ ./internal/daemon/", false},
		{"go build", "go build ./...", false},
		{"no go test", "make build", false},
		{"empty", "", false},
	}
	blocked := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, ""), tt.command)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked=%v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
			if got {
				blocked++
			}
		})
	}
	if blocked != 24 {
		t.Errorf("blocked %d of %d cases, want exactly 24", blocked, len(tests))
	}
}

// runContainerSuiteGuard runs the container-suite guard on input as proc's
// session and returns its verdict and stderr.
func runContainerSuiteGuard(input string, proc guardProcess) (string, error) {
	var stderr strings.Builder
	err := tapGuardContainerSuite(strings.NewReader(input), &stderr, proc)
	return stderr.String(), err
}

// A polecat that runs the suite gets the scope rule's answer (iterate with
// -run), never the container-suite rule's "run it wrapped" line: that advice
// is what sent zircon, coral and lapis into full 77-package runs beside the
// refinery's gate, doubling its wall time (gt-v6se). The wrapped form is then
// blocked too.
func TestRunTapGuardContainerSuite_PolecatMakeTest(t *testing.T) {
	t.Parallel()
	bare := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
	wrapped := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/zircon -- GOFLAGS=-p=8 make test"}}`

	proc := fakeGuardProcess(map[string]string{"GT_POLECAT": "zircon", "GT_ROLE": "gastown/polecats/zircon"}, "")
	wholeRepo := `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`
	for name, input := range map[string]string{"bare": bare, "wrapped": wrapped, "go test whole repo": wholeRepo} {
		stderr, err := runContainerSuiteGuard(input, proc)
		if err == nil {
			t.Errorf("%s: polecat make test was allowed", name)
		}
		if !strings.Contains(stderr, "TEST SCOPE") || !strings.Contains(stderr, "-run") {
			t.Errorf("%s: block must be the scope rule with the -run alternative, got: %s", name, stderr)
		}
		if strings.Contains(stderr, "Run it wrapped") {
			t.Errorf("%s: block must not tell a polecat to run the suite wrapped, got: %s", name, stderr)
		}
	}
}

// A polecat in a non-Go rig runs its own rig's `make test` through the hook
// (gt-dieu9): the tree is outside every Go module, so neither the scope rule
// nor the container-suite rule has anything to refuse — the fractals rig's
// test target is `npm test` into vitest.
func TestRunTapGuardContainerSuite_NonGoRigMakeTest(t *testing.T) {
	t.Parallel()
	tree := nonGoTree(t)
	cmd := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
	proc := fakeGuardProcess(map[string]string{"GT_POLECAT": "coral", "GT_ROLE": "fractals/polecats/coral"}, tree)
	stderr, err := runContainerSuiteGuard(cmd, proc)
	if err != nil {
		t.Errorf("a non-Go rig's make test must be allowed, got %v: %s", err, stderr)
	}
}

// The same polecat cannot step into the Go tree with a -C: the hook approves
// or refuses a command by where its work happens, so the cwd the rig sits in
// must not be the only thing the guard reads (gt-dieu9 review).
func TestRunTapGuardContainerSuite_NonGoRigMakeTestDashC(t *testing.T) {
	t.Parallel()
	tree := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	command := "make -C " + goRoot + " test"
	cmd := `{"tool_name":"Bash","tool_input":{"command":` + strconv.Quote(command) + `}}`
	proc := fakeGuardProcess(map[string]string{"GT_POLECAT": "coral", "GT_ROLE": "fractals/polecats/coral"}, tree)
	stderr, err := runContainerSuiteGuard(cmd, proc)
	if err == nil {
		t.Errorf("a non-Go rig's make -C into the Go tree was allowed: %s", stderr)
	}
	if !strings.Contains(stderr, "TEST SCOPE") {
		t.Errorf("block must be the scope rule, got: %s", stderr)
	}
}

// The cd form of the same dodge: the -C case above is refused because the hook
// judges a command by where its work happens, and "cd <go tree> && make test"
// is the other way to put the work there. The rig's own tree is where the
// segment starts, so without the cd carry it reads as the rig's harmless make
// target (gt-5mc21).
func TestRunTapGuardContainerSuite_NonGoRigMakeTestAfterCd(t *testing.T) {
	t.Parallel()
	tree := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	proc := fakeGuardProcess(map[string]string{"GT_POLECAT": "coral", "GT_ROLE": "fractals/polecats/coral"}, tree)

	blocked := "cd " + goRoot + " && make test"
	cmd := `{"tool_name":"Bash","tool_input":{"command":` + strconv.Quote(blocked) + `}}`
	stderr, err := runContainerSuiteGuard(cmd, proc)
	if err == nil {
		t.Errorf("a non-Go rig's cd into the Go tree was allowed: %s", stderr)
	}
	if !strings.Contains(stderr, "TEST SCOPE") {
		t.Errorf("block must be the scope rule, got: %s", stderr)
	}

	// The same shape inside the rig's own non-Go tree has nothing to refuse.
	allowed := "cd " + tree + " && make test"
	cmd = `{"tool_name":"Bash","tool_input":{"command":` + strconv.Quote(allowed) + `}}`
	if stderr, err := runContainerSuiteGuard(cmd, proc); err != nil {
		t.Errorf("a cd within the non-Go rig was refused, got %v: %s", err, stderr)
	}
}

// Through the real hook entry point: a polecat is blocked, the refinery (which
// must run whole packages) and crew are not.
func TestRunTapGuardContainerSuite_PolecatTestScope(t *testing.T) {
	t.Parallel()
	cmd := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/flint -- go test ./internal/polecat/ -count=1"}}`

	polecat := fakeGuardProcess(map[string]string{"GT_POLECAT": "flint", "GT_ROLE": "gastown/polecats/flint"}, "")
	stderr, err := runContainerSuiteGuard(cmd, polecat)
	if err == nil {
		t.Error("polecat whole heavy package run was allowed")
	}
	if !strings.Contains(stderr, "TEST SCOPE") || !strings.Contains(stderr, "-run") {
		t.Errorf("block message must name the rule and the -run alternative, got: %s", stderr)
	}

	// The refinery must be allowed regardless of where the suite is run from
	// (gt-tmde); its cwd here is neutral so the guard's cwd-based polecat
	// fallback (isPolecatContext) does not decide it (gt-3008).
	refinery := fakeGuardProcess(map[string]string{"GT_REFINERY": "1", "GT_ROLE": "gastown/refinery"}, "/tmp/neutral")
	if _, err := runContainerSuiteGuard(cmd, refinery); err != nil {
		t.Errorf("refinery whole-package run must be allowed, got %v", err)
	}

	crew := fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/crew"}, "/tmp/neutral")
	if _, err := runContainerSuiteGuard(cmd, crew); err != nil {
		t.Errorf("crew whole-package run must be allowed, got %v", err)
	}
}

// Polecat detection must work from GT_ROLE alone (the signal every spawn
// carries), from GT_POLECAT, and from a polecats/ cwd — and not fire for
// other roles.
func TestIsPolecatContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, polecat, role string
		want                bool
	}{
		{"GT_ROLE compound", "", "gastown/polecats/topaz", true},
		{"GT_ROLE bare", "", "polecat", true},
		{"GT_POLECAT only", "topaz", "", true},
		{"refinery", "", "gastown/refinery", false},
		{"crew", "", "gastown/crew/sloan", false},
		{"nothing", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{"GT_POLECAT": c.polecat, "GT_ROLE": c.role}, "/tmp/neutral")
			if got := isPolecatContext(proc); got != c.want {
				t.Errorf("isPolecatContext() = %v, want %v", got, c.want)
			}
		})
	}
	if !isPolecatContext(fakeGuardProcess(nil, "/town/gastown/polecats/topaz/gastown")) {
		t.Error("isPolecatContext() = false from a polecats/ cwd, want true")
	}
}
