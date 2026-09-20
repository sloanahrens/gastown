package cmd

import (
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
		{"heavy package after another segment", "gofmt -l . && go test ./internal/refinery/", true},
		{"subpackage of a heavy package", "go test ./internal/cmd/sub/", true},
		{"ancestor wildcard covering heavy packages", "go test ./internal/...", true},
		{"bare ancestor wildcard", "go test internal/...", true},
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
			reason, matched := evaluatePolecatTestScope(tt.command)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked=%v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
			if got {
				blocked++
			}
		})
	}
	if blocked != 22 {
		t.Errorf("blocked %d of %d cases, want exactly 22", blocked, len(tests))
	}
}

// A polecat that runs the suite gets the scope rule's answer (iterate with
// -run), never the container-suite rule's "run it wrapped" line: that advice
// is what sent zircon, coral and lapis into full 77-package runs beside the
// refinery's gate, doubling its wall time (gt-v6se). The wrapped form is then
// blocked too. The refinery keeps both: bare make test blocked with the
// wrapped advice, wrapped make test allowed.
func TestRunTapGuardContainerSuite_PolecatMakeTest(t *testing.T) {
	t.Setenv(dockerTestsEnv, "")
	bare := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
	wrapped := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/zircon -- GOFLAGS=-p=8 make test"}}`

	t.Setenv("GT_POLECAT", "zircon")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/zircon")
	wholeRepo := `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`
	for name, input := range map[string]string{"bare": bare, "wrapped": wrapped, "go test whole repo": wholeRepo} {
		var err error
		stderr := captureStderr(t, func() {
			withStdin(t, input, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
		})
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

	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")
	t.Chdir(t.TempDir())
	var err error
	stderr := captureStderr(t, func() {
		withStdin(t, bare, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
	})
	if err == nil || !strings.Contains(stderr, "Run it wrapped") {
		t.Errorf("refinery bare make test must still be blocked with the wrapped advice, err=%v stderr=%s", err, stderr)
	}
	withStdin(t, wrapped, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
	if err != nil {
		t.Errorf("refinery wrapped make test must be allowed, got %v", err)
	}
}

// Through the real hook entry point: a polecat is blocked, the refinery (which
// must run whole packages) and crew are not.
func TestRunTapGuardContainerSuite_PolecatTestScope(t *testing.T) {
	t.Setenv(dockerTestsEnv, "")
	cmd := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/flint -- go test ./internal/polecat/ -count=1"}}`

	t.Setenv("GT_POLECAT", "flint")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/flint")
	var err error
	stderr := captureStderr(t, func() {
		withStdin(t, cmd, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
	})
	if err == nil {
		t.Error("polecat whole heavy package run was allowed")
	}
	if !strings.Contains(stderr, "TEST SCOPE") || !strings.Contains(stderr, "-run") {
		t.Errorf("block message must name the rule and the -run alternative, got: %s", stderr)
	}

	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")
	withStdin(t, cmd, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
	if err != nil {
		t.Errorf("refinery whole-package run must be allowed, got %v", err)
	}

	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/crew")
	t.Chdir(t.TempDir())
	withStdin(t, cmd, func() { err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil) })
	if err != nil {
		t.Errorf("crew whole-package run must be allowed, got %v", err)
	}
}

// Polecat detection must work from GT_ROLE alone (the signal every spawn
// carries), from GT_POLECAT, and from a polecats/ cwd — and not fire for
// other roles.
func TestIsPolecatContext(t *testing.T) {
	t.Chdir(t.TempDir())
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
			t.Setenv("GT_POLECAT", c.polecat)
			t.Setenv("GT_ROLE", c.role)
			if got := isPolecatContext(); got != c.want {
				t.Errorf("isPolecatContext() = %v, want %v", got, c.want)
			}
		})
	}
}
