package cmd

import (
	"os"
	"testing"
)

func TestEvaluateContainerSuiteCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked: bare go test on a testcontainers-backed package.
		{"go test single container package", "go test ./internal/beads/...", true},
		{"go test multiple package args, one container", "go test ./internal/daemon/... ./internal/cmd/...", true},
		{"go test ancestor dir covering container packages", "go test ./internal/...", true},
		{"go test whole repo wildcard", "go test ./...", true},
		{"go test bare dots", "go test ...", true},
		{"GOFLAGS prefixed go test on container package", "GOFLAGS=-p=6 go test ./internal/refinery/...", true},
		{"go test with -run flag on container package", "go test ./internal/beads/... -run TestFoo -v", true},

		// Blocked: bare make test (always runs go test ./... per Makefile).
		{"make test bare", "make test", true},
		{"GOFLAGS prefixed make test", "GOFLAGS=-p=6 make test", true},

		// Allowed: wrapped in gt slot run.
		{"go test wrapped in gt slot run", "gt slot run --role gastown/refinery -- go test ./internal/beads/...", false},
		{"make test wrapped in gt slot run", "gt slot run --role gastown/refinery -- GOFLAGS=-p=6 make test", false},
		{"go test whole repo wrapped", "gt slot run --role gastown/refinery -- go test ./...", false},

		// Allowed: non-container packages.
		{"go test non-container package", "go test ./internal/style/...", false},
		{"go test multiple non-container packages", "go test ./internal/style/... ./internal/version/...", false},
		{"go test bare no package args", "go test", false},
		{"go test with -v only", "go test -v", false},

		// Allowed: unrelated commands.
		{"unrelated command", "ls -la", false},
		{"empty command", "", false},
		{"go build", "go build ./...", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(tt.command)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

func TestContainerSuitePackagesIntersect(t *testing.T) {
	tests := []struct {
		name      string
		pkgArgs   []string
		wantWhole bool
		wantCount int
	}{
		{"no args", nil, false, 0},
		{"whole repo dots", []string{"./..."}, true, 0},
		{"bare ellipsis", []string{"..."}, true, 0},
		{"exact container package", []string{"./internal/beads/..."}, false, 1},
		{"ancestor covers multiple container packages", []string{"./internal/..."}, false, len(containerSuitePackages)},
		{"non-container package", []string{"./internal/style/..."}, false, 0},
		{"mixed container and non-container", []string{"./internal/style/...", "./internal/mail/..."}, false, 1},
		{"duplicate package args dedupe", []string{"./internal/beads/...", "./internal/beads/foo"}, false, 1},
		{"module-prefixed path", []string{"github.com/steveyegge/gastown/internal/polecat/..."}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wholeRepo, matched := containerSuitePackagesIntersect(tt.pkgArgs)
			if wholeRepo != tt.wantWhole {
				t.Errorf("wholeRepo = %v, want %v", wholeRepo, tt.wantWhole)
			}
			if len(matched) != tt.wantCount {
				t.Errorf("matched = %v (len %d), want len %d", matched, len(matched), tt.wantCount)
			}
		})
	}
}

func TestIsPolecatOrRefineryContext(t *testing.T) {
	tests := []struct {
		name       string
		gtPolecat  string
		gtRefinery string
		gtRole     string
		want       bool
	}{
		{"GT_POLECAT set", "topaz", "", "", true},
		{"GT_REFINERY set", "", "1", "", true},
		{"GT_ROLE refinery", "", "", "gastown/refinery", true},
		{"neither set", "", "", "", false},
		{"GT_ROLE polecat compound without GT_POLECAT", "", "", "gastown/polecats/topaz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Chdir to a neutral tmp dir so the cwd-path fallback (which
			// matches any path containing "/polecats/", same as
			// isGasTownAgentContext) can't be tripped by running this test
			// from inside a polecat worktree.
			t.Chdir(t.TempDir())
			t.Setenv("GT_POLECAT", tt.gtPolecat)
			t.Setenv("GT_REFINERY", tt.gtRefinery)
			t.Setenv("GT_ROLE", tt.gtRole)
			if got := isPolecatOrRefineryContext(); got != tt.want {
				t.Errorf("isPolecatOrRefineryContext() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsPolecatOrRefineryContext_CwdFallback pins the cwd-path fallback
// itself (same shape as isGasTownAgentContext's "/polecats/" check): a
// polecat worktree cwd is enough even with no env vars set, since a hook's
// ambient environment can't always be relied on to carry GT_POLECAT.
func TestIsPolecatOrRefineryContext_CwdFallback(t *testing.T) {
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "")
	t.Chdir(t.TempDir())
	if isPolecatOrRefineryContext() {
		t.Fatal("expected neutral tmp dir cwd to not trigger the polecat-path fallback")
	}

	polecatDir := t.TempDir() + "/gastown/polecats/topaz"
	if err := os.MkdirAll(polecatDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(polecatDir)
	if !isPolecatOrRefineryContext() {
		t.Error("expected a cwd under /polecats/ to be treated as polecat context even with no env vars set")
	}
}

// TestRunTapGuardContainerSuite_BlockedInPolecatContext is the "genuine
// positive still caught" leg of the adversarial-test-criterion: the guard
// must actually exit non-nil (blocking) end-to-end, not just when its
// internal evaluator is called directly.
func TestRunTapGuardContainerSuite_BlockedInPolecatContext(t *testing.T) {
	t.Setenv("GT_POLECAT", "topaz")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"go test ./internal/beads/..."}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err == nil {
		t.Error("expected bare go test on a container-backed package to be blocked for a polecat, got nil error")
	}
}

// TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext is the
// "false positive gone" leg: the same command that gets blocked for a
// polecat must be allowed for a role this guard doesn't cover (e.g. crew).
func TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext(t *testing.T) {
	// Chdir off the polecat worktree this test binary happens to run from —
	// otherwise the cwd-path fallback (see TestIsPolecatOrRefineryContext_CwdFallback)
	// would make this a polecat context regardless of env vars.
	t.Chdir(t.TempDir())
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/crew")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"go test ./internal/beads/..."}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err != nil {
		t.Errorf("expected non-polecat/refinery context to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardContainerSuite_WrappedAllowed is the "wrapped" leg: the
// exact same target package, wrapped in gt slot run, must be allowed even
// under a polecat context.
func TestRunTapGuardContainerSuite_WrappedAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "topaz")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/polecats/topaz -- go test ./internal/beads/..."}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err != nil {
		t.Errorf("expected gt-slot-run-wrapped command to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardContainerSuite_RefineryBlocked pins the refinery leg of the
// guard, not just polecat.
func TestRunTapGuardContainerSuite_RefineryBlocked(t *testing.T) {
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"GOFLAGS=-p=6 make test"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err == nil {
		t.Error("expected bare make test to be blocked for the refinery role, got nil error")
	}
}

// TestRunTapGuardContainerSuite_NonContainerPackageAllowed is the "novel/
// unaffected input still passes" leg: a polecat running tests scoped to a
// package with no Docker footprint must not be blocked.
func TestRunTapGuardContainerSuite_NonContainerPackageAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "topaz")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"go test ./internal/style/..."}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err != nil {
		t.Errorf("expected non-container package to be allowed, got error: %v", err)
	}
}
