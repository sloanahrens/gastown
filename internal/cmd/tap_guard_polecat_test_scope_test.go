package cmd

import (
	"strings"
	"testing"
)

// The rule and its boundaries: whole-repo and unfiltered heavy-package runs
// block; filtered runs, light packages and the refinery's own commands pass.
// The count assertion fails a version that blocks everything or nothing.
func TestEvaluatePolecatTestScope(t *testing.T) {
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

		{"filtered heavy package", "go test ./internal/cmd/ -run 'TestApplyMQCheck|TestSlingDeadAgent'", false},
		{"filtered heavy package, -run= form", "go test -run=TestFoo ./internal/daemon/", false},
		{"filtered two heavy packages", "go test ./internal/cmd/ ./internal/polecat/ -run TestFoo -count=1", false},
		{"filtered heavy package in slot run", "gt slot run --role gastown/flint -- go test ./internal/daemon/ -run TestFeedFirstReady", false},
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
	if blocked != 11 {
		t.Errorf("blocked %d of %d cases, want exactly 11", blocked, len(tests))
	}
}

// Through the real hook entry point: a polecat is blocked, the refinery (which
// must run whole packages) and crew are not.
func TestRunTapGuardContainerSuite_PolecatTestScope(t *testing.T) {
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
