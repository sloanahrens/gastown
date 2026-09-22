package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestEvaluateContainerSuiteCommand(t *testing.T) {
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked: bare go test on a testcontainers-backed package WITH the
		// container opt-in switched on (testutil.DockerTestsEnv). Without it
		// nothing can start a container, so the slot is not required.
		{"go test single container package", "GT_TEST_DOCKER=1 go test ./internal/beads/...", true},
		{"go test multiple package args, one container", "GT_TEST_DOCKER=1 go test ./internal/daemon/... ./internal/cmd/...", true},
		{"go test ancestor dir covering container packages", "GT_TEST_DOCKER=1 go test ./internal/...", true},
		{"go test whole repo wildcard", "GT_TEST_DOCKER=1 go test ./...", true},
		{"go test bare dots", "GT_TEST_DOCKER=1 go test ...", true},
		{"go test module-prefixed whole repo wildcard", "GT_TEST_DOCKER=1 go test github.com/steveyegge/gastown/...", true},
		{"go test container package with a trailing slash", "GT_TEST_DOCKER=1 go test ./internal/beads/", true},
		{"go test module-prefixed container package", "GT_TEST_DOCKER=1 go test github.com/steveyegge/gastown/internal/beads/...", true},
		{"GOFLAGS prefixed go test on container package", "GT_TEST_DOCKER=1 GOFLAGS=-p=6 go test ./internal/refinery/...", true},
		{"go test with -run flag on container package", "GT_TEST_DOCKER=1 go test ./internal/beads/... -run TestFoo -v", true},
		{"switch via export in an earlier segment", "export GT_TEST_DOCKER=1; go test ./internal/beads/...", true},
		{"switch via env(1)", "env GT_TEST_DOCKER=1 go test ./internal/beads/...", true},
		{"switch quoted", `GT_TEST_DOCKER="1" go test ./internal/beads/...`, true},

		// Allowed: the same bare runs with the switch off cannot reach Docker.
		{"bare go test container package, switch off", "go test ./internal/beads/...", false},
		{"bare go test container package, switch explicitly 0", "GT_TEST_DOCKER=0 go test ./internal/beads/...", false},
		{"bare filtered go test on internal/cmd, switch off", "go test ./internal/cmd/ -run TestFoo", false},
		{"bare go test whole repo, switch off", "go test ./...", false},

		// Blocked: bare make test (always runs go test ./... per Makefile).
		{"make test bare", "make test", true},
		{"GOFLAGS prefixed make test", "GOFLAGS=-p=6 make test", true},
		{"make test with jobs flag", "make -j4 test", true},
		{"make test with directory flag and value", "make -C . test", true},
		{"make test with valueless flags", "make -e -w -i test", true},
		{"make test with bare jobs flag", "make -j test", true},
		{"make test with spaced jobs value", "make -j 4 test", true},

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

func TestContainerSuiteTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		pkgArgs   []string
		wantWhole bool
		wantCount int
	}{
		// cwd "" is outside any module, so the argument form is the only one
		// these cases exercise; cwd resolution is TestCwdContainerPackages.
		{"no args", nil, false, 0},
		{"whole repo dots", []string{"./..."}, true, 0},
		{"bare ellipsis", []string{"..."}, true, 0},
		{"module-prefixed whole repo wildcard", []string{"github.com/steveyegge/gastown/..."}, true, 0},
		{"exact container package", []string{"./internal/beads/..."}, false, 1},
		{"ancestor covers multiple container packages", []string{"./internal/..."}, false, len(containerSuitePackages)},
		{"non-container package", []string{"./internal/style/..."}, false, 0},
		{"mixed container and non-container", []string{"./internal/style/...", "./internal/mail/..."}, false, 1},
		{"duplicate package args dedupe", []string{"./internal/beads/...", "./internal/beads/foo"}, false, 1},
		{"module-prefixed path", []string{"github.com/steveyegge/gastown/internal/polecat/..."}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wholeRepo, matched := containerSuiteTarget(tt.pkgArgs, "")
			if wholeRepo != tt.wantWhole {
				t.Errorf("wholeRepo = %v, want %v", wholeRepo, tt.wantWhole)
			}
			if len(matched) != tt.wantCount {
				t.Errorf("matched = %v (len %d), want len %d", matched, len(matched), tt.wantCount)
			}
		})
	}
}

// cwdContainerPackages is the cwd form of the container rule: the cwd's
// package is judged with packageArgCovers, the same rule an argument gets, so
// the set a "go test ." from that directory reaches is the set "go test
// ./<that dir>" reaches.
func TestCwdContainerPackages(t *testing.T) {
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name   string
		relDir string
		want   []string
	}{
		{"module root", ".", nil},
		{"container package", "internal/beads", []string{"internal/beads"}},
		{"subpackage of a container package", "internal/beads/sub", []string{"internal/beads"}},
		{"import-only container package (links testutil, no call site)", "internal/doctor", []string{"internal/doctor"}},
		{"ancestor of every container package", "internal", containerSuitePackages},
		{"light package", "internal/style", nil},
		{"light package outside internal", "cmd/gt", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cwdContainerPackages(filepath.Join(root, filepath.FromSlash(tt.relDir)))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("cwdContainerPackages(%s) = %v, want %v", tt.relDir, got, tt.want)
			}
		})
	}
}

// worktreeLayouts are the checkout shapes the guards run in, as paths to
// build under a temp dir. The first is the refinery's, where the module root
// directory is named "rig" and an outer directory is named "gastown"
// (docs/reference.md:378) — the layout that defeats a root found by matching
// the directory name "gastown" (gt-1lko).
var worktreeLayouts = []string{
	"gastown/refinery/rig",
	"gastown/polecats/topaz/gastown",
	"clone",
	"rig",
}

// fakeModule materialises a gastown module tree under path.Join(tmp,
// worktreeRel) and returns the module root: a real go.mod plus the package
// directories the guards name, so a cwd resolution under it runs the real
// walk instead of matching a fabricated path string.
func fakeModule(t *testing.T, worktreeRel string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), worktreeRel)
	for _, rel := range []string{
		"internal", "internal/beads", "internal/beads/sub", "internal/cmd",
		"internal/cmd/sub", "internal/style", "cmd/gt", "internal/doctor",
	} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}
	goMod := "module " + gastownModulePath + "\n\ngo 1.26.2\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return root
}

// cwdPackagePath is what makes "." / "./" and a bare "go test" blockable: it
// turns the caller's directory into the package path the argument form would
// have carried. Every layout is exercised, because a resolution anchored to
// the checkout's directory name answers correctly in exactly one of them.
func TestCwdPackagePath(t *testing.T) {
	for _, layout := range worktreeLayouts {
		t.Run(layout, func(t *testing.T) {
			root := fakeModule(t, layout)
			tests := []struct {
				name    string
				relDir  string
				wantPkg string
				wantOK  bool
			}{
				{"module root", ".", "", true},
				{"container package", "internal/beads", "internal/beads", true},
				{"subpackage of a container package", "internal/beads/sub", "internal/beads/sub", true},
				{"heavy package", "internal/cmd", "internal/cmd", true},
				{"light package", "internal/style", "internal/style", true},
				{"light package outside internal", "cmd/gt", "cmd/gt", true},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					pkg, ok := cwdPackagePath(filepath.Join(root, filepath.FromSlash(tt.relDir)))
					if pkg != tt.wantPkg || ok != tt.wantOK {
						t.Errorf("cwdPackagePath(%s/%s) = (%q, %v), want (%q, %v)", layout, tt.relDir, pkg, ok, tt.wantPkg, tt.wantOK)
					}
				})
			}
		})
	}
}

// A cwd outside this module has no package path to judge. Failing open there
// cannot hide a listed package: the lists are module-relative, so a cwd in
// another module or in no module at all cannot be one. A nested module (the
// plugins/dolt-snapshots submodule in this repo) ends the walk for the same
// reason.
func TestCwdPackagePath_OutsideModule(t *testing.T) {
	t.Run("no go.mod anywhere", func(t *testing.T) {
		if pkg, ok := cwdPackagePath(t.TempDir()); ok {
			t.Errorf("cwdPackagePath(%s) = (%q, true), want ok false outside any module", t.TempDir(), pkg)
		}
	})

	t.Run("another module", func(t *testing.T) {
		root := fakeModule(t, "clone")
		nested := filepath.Join(root, "plugins", "dolt-snapshots", "internal")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(nested), "go.mod"),
			[]byte("module "+gastownModulePath+"/plugins/dolt-snapshots\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if pkg, ok := cwdPackagePath(nested); ok {
			t.Errorf("cwdPackagePath(%s) = (%q, true), want ok false: the nearest go.mod declares another module", nested, pkg)
		}
	})
}

// The cwd form of the container rule, end to end through os.Getwd(): the same
// package reached as "go test ." must be judged exactly as "go test
// ./internal/beads" is. The polecat cwd is the one attempt 2's guard let
// through (gt-1lko).
func TestEvaluateContainerSuiteCommand_CwdStyle(t *testing.T) {
	t.Setenv(dockerTestsEnv, "")
	tests := []struct {
		name    string
		relDir  string
		command string
		blocked bool
	}{
		{"dot in a container package", "internal/beads", "GT_TEST_DOCKER=1 go test .", true},
		{"dot-slash in a container package", "internal/beads", "GT_TEST_DOCKER=1 go test ./", true},
		{"no package arg in a container package", "internal/beads", "GT_TEST_DOCKER=1 go test", true},
		{"dot in a subpackage of a container package", "internal/beads/sub", "GT_TEST_DOCKER=1 go test .", true},
		{"dot in an ancestor of a container package", "internal", "GT_TEST_DOCKER=1 go test .", true},
		{"dot in a container package, slot-wrapped", "internal/beads", "gt slot run --role gastown/refinery -- GT_TEST_DOCKER=1 go test .", false},
		{"dot in a light package", "internal/style", "GT_TEST_DOCKER=1 go test .", false},
		{"dot in a light package outside internal", "cmd/gt", "GT_TEST_DOCKER=1 go test .", false},
		{"dot at the module root", ".", "GT_TEST_DOCKER=1 go test .", false},
		{"dot in a container package, switch off", "internal/beads", "go test .", false},
	}
	for _, layout := range worktreeLayouts {
		t.Run(layout, func(t *testing.T) {
			root := fakeModule(t, layout)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Chdir(filepath.Join(root, filepath.FromSlash(tt.relDir)))
					reason, _ := evaluateContainerSuiteCommand(tt.command)
					if got := reason != ""; got != tt.blocked {
						t.Errorf("evaluateContainerSuiteCommand(%q) from %s blocked = %v (reason %q), want %v",
							tt.command, tt.relDir, got, reason, tt.blocked)
					}
				})
			}
		})
	}
}

// cwdHeavyPackages is the polecat scope rule's cwd form: the same set a "go
// test ./<that dir>" names, so cd-ing into the package is not a way around
// the rule.
func TestCwdHeavyPackages(t *testing.T) {
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name   string
		relDir string
		want   []string
	}{
		{"module root", ".", nil},
		{"heavy package", "internal/cmd", []string{"internal/cmd"}},
		{"subpackage of a heavy package", "internal/cmd/sub", []string{"internal/cmd"}},
		{"ancestor of every heavy package", "internal", []string{"internal/cmd", "internal/daemon", "internal/polecat", "internal/refinery"}},
		{"light package", "internal/style", nil},
		{"container package that is not heavy", "internal/beads", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cwdHeavyPackages(filepath.Join(root, filepath.FromSlash(tt.relDir)))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("cwdHeavyPackages(%s) = %v, want %v", tt.relDir, got, tt.want)
			}
		})
	}
}

// The polecat scope rule's cwd form, end to end: "go test ." and a bare "go
// test" in a heavy package are the whole-package run the rule exists to stop,
// and the -run filter is the alternative it points at. The refinery layout is
// where the guard runs, so it is where the resolution is pinned.
func TestEvaluatePolecatTestScope_CwdStyle(t *testing.T) {
	tests := []struct {
		name    string
		relDir  string
		command string
		blocked bool
	}{
		{"dot in a heavy package", "internal/cmd", "go test .", true},
		{"dot-slash in a heavy package", "internal/cmd", "go test ./", true},
		{"no package arg in a heavy package", "internal/cmd", "go test", true},
		{"no package arg with a count flag", "internal/cmd", "go test -count=1", true},
		{"dot in a subpackage of a heavy package", "internal/cmd/sub", "go test .", true},
		{"dot in an ancestor of a heavy package", "internal", "go test .", true},
		{"dot in a heavy package, slot-wrapped", "internal/cmd", "gt slot run --role gastown/flint -- go test .", true},
		{"dot in a heavy package with -run", "internal/cmd", "go test . -run 'TestOne|TestTwo'", false},
		{"dot in a light package", "internal/style", "go test .", false},
		{"dot in a light package outside internal", "cmd/gt", "go test .", false},
		{"dot at the module root", ".", "go test .", false},
	}
	for _, layout := range worktreeLayouts {
		t.Run(layout, func(t *testing.T) {
			root := fakeModule(t, layout)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Chdir(filepath.Join(root, filepath.FromSlash(tt.relDir)))
					reason, matched := evaluatePolecatTestScope(tt.command)
					if got := reason != ""; got != tt.blocked {
						t.Errorf("evaluatePolecatTestScope(%q) from %s blocked = %v (reason %q, matched %v), want %v",
							tt.command, tt.relDir, got, reason, matched, tt.blocked)
					}
					if tt.blocked && !containsSubsequence(matched, []string{"internal/cmd"}) {
						t.Errorf("evaluatePolecatTestScope(%q) blocked but matched %v, want the heavy package named", tt.command, matched)
					}
				})
			}
		})
	}
}

// The cwd the guard actually runs from: the test binary's own directory in
// this checkout, whatever the checkout directory is called. A resolution that
// reads the package path out of the enclosing go.mod answers here; one that
// matches the name "gastown" only answers in a worktree that happens to carry
// it.
func TestCwdPackagePath_ActualCheckout(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	pkg, ok := cwdPackagePath(cwd)
	if !ok {
		t.Fatalf("cwdPackagePath(%s) did not resolve a package path; the guard would fail open here", cwd)
	}
	if pkg != "internal/cmd" {
		t.Errorf("cwdPackagePath(%s) = %q, want %q", cwd, pkg, "internal/cmd")
	}
	if got := strings.Join(cwdContainerPackages(cwd), ","); got != "internal/cmd" {
		t.Errorf("cwdContainerPackages(%s) = %q, want %q", cwd, got, "internal/cmd")
	}
	if got := strings.Join(cwdHeavyPackages(cwd), ","); got != "internal/cmd" {
		t.Errorf("cwdHeavyPackages(%s) = %q, want %q", cwd, got, "internal/cmd")
	}
}

// Both guards must agree on an argument form and its cwd form: the rule that
// decides one decides the other, or a polecat cd-s into the package and runs
// the same tests the argument form blocks.
func TestCwdAndArgumentFormsAgree(t *testing.T) {
	root := fakeModule(t, "gastown/refinery/rig")
	for _, relDir := range []string{"internal", "internal/beads", "internal/beads/sub", "internal/cmd", "internal/cmd/sub", "internal/style", "cmd/gt", "."} {
		t.Run(relDir, func(t *testing.T) {
			cwd := filepath.Join(root, filepath.FromSlash(relDir))
			t.Chdir(cwd)

			argForm, _ := evaluateContainerSuiteCommand("GT_TEST_DOCKER=1 go test ./" + relDir)
			cwdForm, _ := evaluateContainerSuiteCommand("GT_TEST_DOCKER=1 go test .")
			if (argForm != "") != (cwdForm != "") {
				t.Errorf("container guard: ./%s blocked by argument form = %v (reason %q), by cwd form = %v (reason %q)",
					relDir, argForm != "", argForm, cwdForm != "", cwdForm)
			}

			argReason, _ := evaluatePolecatTestScope("go test ./" + relDir)
			scopeReason, _ := evaluatePolecatTestScope("go test .")
			if (argReason != "") != (scopeReason != "") {
				t.Errorf("polecat scope rule: ./%s blocked by argument form = %v, by cwd form = %v", relDir, argReason != "", scopeReason != "")
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
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
	t.Setenv("GT_POLECAT", "topaz")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"GT_TEST_DOCKER=1 go test ./internal/beads/..."}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil)
	})
	if err == nil {
		t.Error("expected bare go test on a container-backed package with the switch on to be blocked for a polecat, got nil error")
	}
}

// TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext is the
// "false positive gone" leg: the same command that gets blocked for a
// polecat must be allowed for a role this guard doesn't cover (e.g. crew).
func TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext(t *testing.T) {
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
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
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
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
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
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
	// The guard reads the opt-in from its own environment; `make test`
	// exports it, so pin it off or the "switch off" cases flip under the gate.
	t.Setenv(dockerTestsEnv, "")
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

// The guard spells the opt-in variable out instead of importing testutil
// (which would link testcontainers into gt); this pins the two together.
func TestDockerTestsEnvMatchesTestutil(t *testing.T) {
	t.Parallel()
	if dockerTestsEnv != testutil.DockerTestsEnv {
		t.Fatalf("guard dockerTestsEnv %q != testutil.DockerTestsEnv %q", dockerTestsEnv, testutil.DockerTestsEnv)
	}
}

// An opt-in already exported in the session environment reaches the test
// process without appearing in the command, so the guard reads its own env.
func TestEvaluateContainerSuiteCommand_EnvOptIn(t *testing.T) {
	t.Setenv(dockerTestsEnv, "1")
	if reason, _ := evaluateContainerSuiteCommand("go test ./internal/beads/..."); reason == "" {
		t.Error("bare go test on a container package with the opt-in exported in the environment was allowed")
	}
	t.Setenv(dockerTestsEnv, "")
	if reason, _ := evaluateContainerSuiteCommand("go test ./internal/beads/..."); reason != "" {
		t.Errorf("bare go test with the opt-in unset was blocked: %s", reason)
	}
}

// containerSuiteBlockCommand extracts the "Run it wrapped instead:" line from
// a block message printed by printContainerSuiteBlock.
func containerSuiteBlockCommand(t *testing.T, block string) string {
	t.Helper()
	const marker = "Run it wrapped instead:"
	for _, line := range strings.Split(block, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i+len(marker):])
		}
	}
	t.Fatalf("no %q line in block:\n%s", marker, block)
	return ""
}

// stubRecorder writes a PATH directory whose 'gt', 'go' and 'make' are shell
// scripts recording their argv (NUL-separated, program basename first) into
// the file named by the returned env assignment. The go/make stubs are there
// so that a wrap which leaks an unwrapped tail records that leak instead of
// starting a real suite.
func stubRecorder(t *testing.T, dir string) (envAssign, logPath string) {
	t.Helper()
	logPath = filepath.Join(dir, "argv.log")
	for _, name := range []string{"gt", "go", "make"} {
		// t.TempDir() paths carry no shell metacharacters, so the path needs
		// no quoting beyond the single quotes that keep it one word.
		script := "#!/bin/sh\nprintf '%s\\0' \"${0##*/}\" \"$@\" >> '" + logPath + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	return "GT_OTVB_STUB_LOG=" + logPath, logPath
}

// TestContainerSuiteBlockPrintedCommandRoundTrips is gt-otvb: the guard's
// "Run it wrapped instead:" line is handed to an operator to paste into a
// shell, so what a shell executes after the paste has to be the wrap of the
// command that was blocked — the whole command, and nothing outside the wrap.
// The test takes the printed line, runs it through a real shell against a
// stub 'gt', and compares the argv the stub received.
//
// The cases are the three shapes that used to break the promise: a leading
// VAR=value token (exec'd as the program before gt-18nx), a compound command
// (the wrap bound to the first segment only, so the rest ran bare), and a
// command whose own arguments are quoted (the wrap has to survive re-parsing
// unchanged).
func TestContainerSuiteBlockPrintedCommandRoundTrips(t *testing.T) {
	// The guard reads the container opt-in from its own environment.
	t.Setenv(dockerTestsEnv, "")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")
	// Chdir off this worktree: a cwd under /polecats/ is a polecat context,
	// and the polecat test-scope rule answers before this one.
	t.Chdir(t.TempDir())

	stubDir := t.TempDir()
	envAssign, logPath := stubRecorder(t, stubDir)

	for _, tt := range []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "leading env assignment",
			command: "GOFLAGS=-p=6 make test",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "env", "GOFLAGS=-p=6", "make", "test"},
		},
		{
			name:    "plain make test",
			command: "make test",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "make", "test"},
		},
		{
			name:    "container package with the opt-in prefix",
			command: "GT_TEST_DOCKER=1 go test ./internal/beads/...",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "env", "GT_TEST_DOCKER=1", "go", "test", "./internal/beads/..."},
		},
		{
			// The wrap binds to a command, and 'export' is a shell builtin:
			// wrapping the line's first segment asked exec to run 'export'
			// and left the suite itself running bare outside the slot.
			name:    "compound with a leading export",
			command: "export GT_TEST_DOCKER=1; go test ./internal/beads/...",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "sh", "-c", "export GT_TEST_DOCKER=1; go test ./internal/beads/..."},
		},
		{
			// A wrap that bound to the first segment ran the suite inside the
			// slot and the later 'make test' outside it — the collision the
			// guard exists to prevent.
			name:    "compound with an unrelated first segment",
			command: "ls && GOFLAGS=-p=6 make test",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "sh", "-c", "ls && GOFLAGS=-p=6 make test"},
		},
		{
			// The command's own quoting must survive the round trip intact.
			name:    "compound with a quoted argument",
			command: "go test ./internal/beads/... -run 'TestFoo' && make test",
			want:    []string{"gt", "slot", "run", "--role", "gastown/refinery", "--", "sh", "-c", "go test ./internal/beads/... -run 'TestFoo' && make test"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hook := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(tt.command) + `}}`
			var block string
			withStdin(t, hook, func() {
				block = captureStderr(t, func() {
					if err := runTapGuardContainerSuite(tapGuardContainerSuiteCmd, nil); err == nil {
						t.Errorf("command %q was not blocked at all", tt.command)
					}
				})
			})
			printed := containerSuiteBlockCommand(t, block)
			if printed == "" {
				t.Fatal("guard printed an empty remediation")
			}

			// Run the printed line the way an operator would: through a shell,
			// with a stub 'gt' (and go/make) first on PATH.
			if err := os.WriteFile(logPath, nil, 0o644); err != nil {
				t.Fatalf("truncate stub log: %v", err)
			}
			shCmd := exec.Command("sh", "-c", printed) //nolint:gosec // G204: the printed line is the value under test
			shCmd.Env = append(os.Environ(), "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"), envAssign)
			if out, err := shCmd.CombinedOutput(); err != nil {
				t.Fatalf("running printed command %q: %v\n%s", printed, err, out)
			}

			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read stub log: %v", err)
			}
			var got []string
			for _, field := range strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00") {
				if field != "" {
					got = append(got, field)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("printed command %q\nran as  %q\nwant    %q", printed, got, tt.want)
			}
		})
	}
}

// TestContainerSuiteWrapRole pins the two readings of the --role argument: the
// caller's own role when the hook carries one (so the printed line is
// runnable verbatim), and the formula's placeholder when it does not.
func TestContainerSuiteWrapRole(t *testing.T) {
	for _, tt := range []struct {
		name   string
		gtRole string
		want   string
	}{
		{"refinery", "gastown/refinery", "gastown/refinery"},
		{"polecat", "gastown/polecats/zircon", "gastown/polecats/zircon"},
		{"unset", "", "<rig>/<you>"},
		{"bare role with no rig", "mayor", "<rig>/<you>"},
		{"role needing quoting", "gastown/po lecats", "<rig>/<you>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.gtRole)
			if got := containerSuiteWrapRole(); got != tt.want {
				t.Errorf("containerSuiteWrapRole() with GT_ROLE=%q = %q, want %q", tt.gtRole, got, tt.want)
			}
		})
	}
}
