package cmd

import (
	"os"
	"path/filepath"
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
		// The other goals that run the whole suite on the host (gt-gl3jb).
		{"make gate", "make gate", true},
		{"make gate with jobs flag", "make -j4 gate", true},
		{"make gate wrapped in slot run", "gt slot run --role gastown/zircon -- make gate", true},
		{"make gate-test", "make gate-test", true},
		{"make gate-test with jobs flag", "make -j4 gate-test", true},
		{"make gate-test wrapped in slot run", "gt slot run --role gastown/zircon -- make gate-test", true},
		{"make test-slow", "make test-slow", true},
		{"make test-slow with jobs flag", "make -j4 test-slow", true},
		{"make test-slow wrapped in slot run", "gt slot run --role gastown/zircon -- make test-slow", true},
		{"make test-integration", "make test-integration", true},
		{"make test-integration with jobs flag", "make -j4 test-integration", true},
		{"make test-integration wrapped in slot run", "gt slot run --role gastown/zircon -- make test-integration", true},
		{"make gate after another goal", "make clean gate", true},

		{"filtered heavy package", "go test ./internal/cmd/ -run 'TestApplyMQCheck|TestSlingDeadAgent'", false},
		{"filtered heavy package, -run= form", "go test -run=TestFoo ./internal/daemon/", false},
		{"filtered two heavy packages", "go test ./internal/cmd/ ./internal/polecat/ -run TestFoo -count=1", false},
		{"filtered heavy package in slot run", "gt slot run --role gastown/flint -- go test ./internal/daemon/ -run TestFeedFirstReady", false},
		{"filter passed to the test binary after --", "go test ./internal/cmd/ -- -test.run TestFoo", false},
		{"filtered heavy package, --run form", "go test --run TestOne ./internal/cmd", false},
		{"filtered heavy package, --run= form", "go test --run=TestOne ./internal/cmd", false},
		{"unfiltered heavy package after a double-dash flag", "go test --timeout 5m ./internal/cmd", true},
		{"double-dash flag values are not packages", "go test --count 1 --timeout 5m ./internal/style", false},
		{"double-dash -skip pattern is not a package", "go test --skip 'Test*' ./internal/style", false},
		{"whole light package", "go test ./internal/git/ -count=1", false},
		{"whole light packages", "go test ./internal/style/... ./internal/config/", false},
		{"go vet", "go vet ./internal/cmd/ ./internal/daemon/", false},
		{"go build", "go build ./...", false},
		{"no go test", "make build", false},
		{"make lint", "make lint", false},
		{"make presubmit", "make presubmit", false},
		{"make gate-lint", "make gate-lint", false},
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
	if blocked != 38 {
		t.Errorf("blocked %d of %d cases, want exactly 38", blocked, len(tests))
	}
}

// A bare or "." target is judged in the directory the invocation's own -C dir
// flag names, not in the one the hook runs in: Go chdirs on -C before it runs
// the command (gt-tzdpe), the reading the container rule gives the same
// spellings (TestContainerSuiteGoTestTargetFollowsTheCDirFlag). An explicit
// package argument carries its own path and keeps its reading, and a -C
// directory the guard cannot place is refused rather than read as a light one.
func TestEvaluatePolecatTestScope_TargetFollowsTheCDirFlag(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	heavy := filepath.Join(root, "internal", "cmd")
	light := filepath.Join(root, "internal", "style")
	nowhere := filepath.Join(root, "nowhere")
	heavyLink := symlinkedTree(t, heavy)

	tests := []struct {
		name    string
		cwd     string
		command string
		blocked bool
	}{
		{"-C into a heavy package, dot target", light, "go -C " + heavy + " test .", true},
		{"-C into a heavy package, dot-slash target", light, "go -C " + heavy + " test ./", true},
		{"-C into a heavy package, bare target", light, "go -C " + heavy + " test", true},
		{"-C= into a heavy package", light, "go -C=" + heavy + " test .", true},
		{"-C through a link into a heavy package", light, "go -C " + heavyLink + " test .", true},
		{"versioned binary with a -C dir", light, "go1.22.0 -C " + heavy + " test .", true},
		{"a relative -C composed onto an absolute one", light, "go -C " + root + " -C internal/cmd test .", true},
		{"-C into a heavy package after a cd away", heavy, "cd " + light + " && go -C " + heavy + " test .", true},

		// The next three rows pin a verdict the guard already gave: they were
		// added with no guard change, so they pass on the code before them and
		// guard against a later edit moving the -C dir back under the cd (gt-mbbhf).
		{"-C into a heavy package after a cd to an unrelated dir", light, "cd /tmp && go -C " + heavy + " test .", true},
		{"-C= into a heavy package after a cd to an unrelated dir", light, "cd /tmp && go -C=" + heavy + " test .", true},
		{"-C= into a heavy package after a cd away", heavy, "cd " + light + " && go -C=" + heavy + " test .", true},

		// A -run filter bounds the run wherever -C points, in every spelling
		// the carve-out reads; the unfiltered rows above are refused.
		{"-C into a heavy package with -run", light, "go -C " + heavy + " test . -run TestOne", false},
		{"-C into a heavy package with -run=", light, "go -C " + heavy + " test -run=TestOne .", false},
		{"-C into a heavy package with -test.run after -args", light, "go -C " + heavy + " test . -args -test.run TestOne", false},

		// The reverse reading: a -C dir the hook is not in stops the hook's
		// own directory from refusing the target.
		{"-C out of a heavy package, dot target", heavy, "go -C " + light + " test .", false},
		{"-C out of a heavy package, bare target", heavy, "go -C " + light + " test", false},
		{"-C to the module root", heavy, "go -C " + root + " test .", false},
		{"-C away from a heavy package after a cd into it", light, "cd " + heavy + " && go -C " + light + " test .", false},

		// An explicit package argument is read whatever -C names, so a heavy
		// one is refused under a light -C dir.
		{"heavy argument under a light -C dir", light, "go -C " + light + " test ./internal/cmd/", true},

		// A ./-relative argument is read under the -C dir as well, so a light
		// one under a heavy -C dir also names internal/cmd/internal/style, a
		// path inside the heavy package. Go finds no such package, but the
		// guard cannot tell which reading the spelling was written for and
		// refuses on either, as the container rule does (gt-q82x1).
		{"light ./ argument under a heavy -C dir is refused", light, "go -C " + heavy + " test ./internal/style/", true},

		// A -C dir the guard places only lexically (go itself fails on a
		// missing one) is judged where it names, and an explicit argument
		// carries its own path. The -C it cannot place at all is the relative
		// one under an unreadable cwd, in the next test.
		{"-C to a missing directory, light argument", light, "go -C " + nowhere + " test ./internal/style/", false},
		{"-C to a missing directory, heavy argument", light, "go -C " + nowhere + " test ./internal/cmd/", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) from %s blocked = %v (reason %q, matched %v), want %v",
					tt.command, tt.cwd, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A relative -C under a hook cwd the guard cannot read names nothing it can
// place, so a cwd-relative target is refused there (gt-tzdpe).
func TestEvaluatePolecatTestScope_RelativeCDirUnderUnplacedCwd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command string
		blocked bool
	}{
		{"go -C internal/style test .", true},
		{"go -C=internal/style test .", true},
		{"go -C internal/style test", true},
		{"go -C internal/style test ./internal/style/", false},
	}
	for _, tt := range tests {
		reason, _ := evaluatePolecatTestScope(fakeGuardProcess(nil, ""), tt.command)
		if got := reason != ""; got != tt.blocked {
			t.Errorf("evaluatePolecatTestScope(%q) from an unplaced cwd blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
		}
	}
}

// A -C directory the shell expands is unplaced, not a literal path under the
// cwd: 'go -C "$PWD/internal/cmd" test .' runs the heavy package whatever the
// text joins to, so it is refused like any other directory the guard cannot
// place, while a literal -C keeps its verdicts (gt-8a1pa).
func TestPolecatTestScopeShellExpandedCDir(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"variable, dot target", `go -C "$PWD/internal/cmd" test .`, true},
		{"variable, -C= spelling", `go -C="$PWD/internal/cmd" test .`, true},
		{"variable, bare target", `go -C "$PWD/internal/cmd" test`, true},
		{"leading tilde", "go -C ~/gt/x/internal/cmd test .", true},
		{"command substitution", `go -C "$(pwd)/internal/cmd" test .`, true},
		{"command substitution, -C= spelling", `go -C="$(pwd)/internal/cmd" test .`, true},
		{"backtick substitution", "go -C `pwd`/internal/cmd test .", true},
		{"expanded after a literal directory", `go -C internal -C "$SUB" test .`, true},
		{"variable, relative heavy argument", `go -C "$PWD/internal" test ./cmd`, true},
		{"variable, relative heavy argument with dots", `go -C "$PWD/internal" test ./cmd/...`, true},
		{"variable, parent-relative heavy argument", `go -C "$PWD/internal" test ../internal/cmd`, true},
		{"variable, relative heavy argument, -count", `go -C "$PWD/internal" test ./cmd -count=1`, true},
		{"variable, relative light argument", `go -C "$PWD/internal" test ./style`, true},
		{"variable, relative light argument with dots", `go -C "$PWD/internal" test ./style/...`, true},
		{"command substitution, relative light argument", `go -C "$(pwd)/internal" test ./style`, true},

		// A -run filter bounds an expanded -C only for an explicit package
		// argument. A bare or "." target names the directory itself, which the
		// guard cannot place, and that refusal is read before the filter.
		{"variable, dot target, -run", `go -C "$PWD/internal/cmd" test . -run X`, true},
		{"variable, -C= spelling, dot target, -run=", `go -C="$PWD/internal/cmd" test -run=X .`, true},
		{"command substitution, dot target, -test.run after -args", `go -C "$(pwd)/internal/cmd" test . -args -test.run X`, true},
		{"leading tilde, dot target, -run", "go -C ~/gt/x/internal/cmd test . -run X", true},
		{"expanded after a literal directory, dot target, -run", `go -C internal -C "$SUB" test . -run X`, true},
		{"variable, -C= spelling, relative heavy argument, -run", `go -C="$PWD/internal" test ./cmd -run X`, false},
		{"command substitution, relative heavy argument, -run=", `go -C "$(pwd)/internal" test -run=X ./cmd`, false},
		{"leading tilde, relative heavy argument, -run", "go -C ~/gt/x/internal test ./cmd -run X", false},
		{"backtick substitution, relative heavy argument, -test.run after -args", "go -C `pwd`/internal test ./cmd -args -test.run X", false},
		{"expanded after a literal directory, relative heavy argument, -run", `go -C internal -C "$SUB" test ./cmd -run X`, false},
		{"literal directory with -run", "go -C internal/cmd test . -run X", false},
		{"variable, relative heavy argument, -run", `go -C "$PWD/internal" test ./cmd -run X`, false},
		{"variable, relative heavy argument, -run before the argument", `go -C "$PWD/internal" test -run=X ./cmd`, false},
		{"variable, relative light argument, -run", `go -C "$PWD/internal" test ./style -run X`, false},
		{"literal directory, light package", "go -C internal/style test .", false},
		{"literal directory, heavy package", "go -C internal/cmd test .", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A package argument is read the way the container rule reads it: one spelled
// as a path under the run directory ("./cmd" under internal/) also names the
// package go resolves it to, and an absolute directory names the
// module-relative package it lies in. Read only as written, each blocked row
// below names internal/cmd and would pass (gt-q82x1).
func TestPolecatTestScopeRunDirRelativePackageArgs(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	internal := filepath.Join(root, "internal")
	heavyAbs := filepath.Join(root, "internal", "cmd")
	lightAbs := filepath.Join(root, "internal", "style")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"-C dir, relative heavy argument", "go -C internal test ./cmd", true},
		{"-C dir, relative heavy argument with dots", "go -C internal test ./cmd/...", true},
		{"-C dir, relative heavy argument, -count", "go -C internal test -count=1 ./cmd", true},
		{"cd into the dir, relative heavy argument", "cd " + internal + " && go test ./cmd", true},
		{"absolute heavy directory", "go test " + heavyAbs, true},
		{"absolute heavy directory with dots", "go test " + heavyAbs + "/...", true},
		{"absolute directory containing heavy packages", "go test " + internal + "/...", true},

		{"-C dir, relative heavy argument, -run", "go -C internal test ./cmd -run X", false},
		{"-C dir, relative heavy argument, -run=", "go -C internal test -run=X ./cmd", false},
		{"cd into the dir, relative heavy argument, -run", "cd " + internal + " && go test ./cmd -run X", false},
		{"absolute heavy directory, -run", "go test " + heavyAbs + " -run X", false},
		{"-C dir, light argument", "go -C internal test ./style", false},
		{"cd into the dir, light argument", "cd " + internal + " && go test ./style", false},
		{"absolute light directory", "go test " + lightAbs, false},

		// A path that leaves the module and comes back in names the package it
		// lands on, which a lexical clean of the argument alone never reaches.
		{"path out of the module root and back into a heavy package", "go test ../" + filepath.Base(root) + "/internal/cmd", true},
		{"path out of the module root and back into a heavy subtree", "go test ../" + filepath.Base(root) + "/internal/...", true},
		{"path out of the module root and back, whole repo", "go test ../" + filepath.Base(root) + "/...", true},
		{"path out of the module root and back, -run", "go test ../" + filepath.Base(root) + "/internal/cmd -run X", false},
		{"path out of the module root and back into a light package", "go test ../" + filepath.Base(root) + "/internal/style", false},
		{"path out of a run dir and back into a heavy package", "go -C internal test ../../" + filepath.Base(root) + "/internal/cmd", true},

		// Module-relative spellings name one package, so the run-dir reading
		// adds nothing to them.
		{"module-relative light argument", "go test ./internal/style", false},
		{"module-relative heavy argument, -run", "go test ./internal/cmd -run X", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A package argument with a wildcard in a path element, or one the shell
// rewrites, names a set of packages the hook cannot see into: it used to read
// as literal text that matches no heavy package, so an unfiltered run of
// internal/cmd went through. A -run filter bounds it (gt-ff2l8).
func TestPolecatTestScopeWildcardPackageArgs(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"glob in a heavy path element", "go test ./internal/c?d", true},
		{"glob star", "go test ./internal/c*", true},
		{"character class", "go test ./internal/[c]md", true},
		{"dots inside a path element", "go test ./internal/c...", true},
		{"brace expansion", "go test ./internal/{cmd,style}", true},
		{"variable prefix", `go test "$PWD/internal/cmd"`, true},
		{"command substitution", `go test "$(pwd)/internal/cmd"`, true},
		{"leading tilde", "go test ~/gt/x/internal/cmd", true},
		{"wrapped in slot run", `gt slot run --role gastown/flint -- go test "$PWD/internal/cmd"`, true},

		{"glob, -run", "go test ./internal/c?d -run TestX", false},
		{"variable prefix, -run", `go test "$PWD/internal/cmd" -run TestX`, false},
		{"glob reaching no heavy package", "go test ./internal/s*", false},
		{"glob under a light package", "go test ./internal/style/*", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A package argument spelled relative to a directory the walk cannot place
// ("cd \"$(git rev-parse --show-toplevel)/internal\" && go test ./cmd") read as
// the module-root package "cmd", which is not heavy, though go runs
// internal/cmd. Without a -run filter it is refused; a cd the walk can place
// keeps its own verdicts (gt-ff2l8).
func TestPolecatTestScopeRelativeArgUnderUnplacedCwd(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"command substitution cd, heavy argument", `cd "$(git rev-parse --show-toplevel)/internal" && go test ./cmd`, true},
		{"command substitution cd, heavy argument with dots", `cd "$(git rev-parse --show-toplevel)/internal" && go test ./cmd/...`, true},
		{"unset variable cd, relative argument", `cd "$UNSET" && go test ./cmd`, true},
		{"unset variable cd, argument that reaches no heavy package", `cd "$UNSET" && go test ./style`, false},
		{"unset variable cd, parent-relative argument", `cd "$UNSET" && go test ../cmd`, true},

		{"command substitution cd, -run", `cd "$(git rev-parse --show-toplevel)/internal" && go test ./cmd -run TestX`, false},
		{"literal cd, -run", "cd internal && go test ./cmd -run TestX", false},
		{"literal cd, light argument", "cd internal && go test ./style", false},
		{"literal cd, heavy argument", "cd internal && go test ./cmd", true},
		{"command substitution cd, import-path light package", `cd "$UNSET" && go test internal/style`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A -run whose pattern selects every test bounds nothing: the run costs what
// the unfiltered one does. A pattern that leaves tests out still bounds it, and
// the last -run on the line is the one go uses (gt-ff2l8).
func TestPolecatTestScopeMatchAllRunFilter(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"dot", "go test ./internal/cmd -run .", true},
		{"dot, = form", "go test ./internal/cmd -run=.", true},
		{"dot star", "go test ./internal/cmd -run '.*'", true},
		{"empty", "go test ./internal/cmd -run ''", true},
		{"empty, = form", "go test ./internal/cmd -run=", true},
		{"empty, double quoted", `go test ./internal/cmd -run ""`, true},
		{"caret", "go test ./internal/cmd -run '^'", true},
		{"case-insensitive flag only", "go test ./internal/cmd -run '(?i)'", true},
		{"match-all alternation", "go test ./internal/cmd -run '.|TestX'", true},
		{"match-all first element of a subtest path", "go test ./internal/cmd -run '.*/sub'", true},
		{"empty before -run value", "go test -run '' ./internal/cmd", true},
		{"test binary flag after --", "go test ./internal/cmd -- -test.run .", true},
		{"double dash spelling", "go test ./internal/cmd --run .", true},
		{"match-all after a narrow one", "go test ./internal/cmd -run TestX -run .", true},
		{"slot-wrapped", "gt slot run --role gastown/flint -- go test ./internal/daemon -run '.*'", true},
		{"variable value", `go test ./internal/cmd -run "$PATTERN"`, true},
		{"variable value, = form", `go test ./internal/cmd -run="$PATTERN"`, true},
		{"braced variable value", `go test ./internal/cmd -run "${PATTERN}"`, true},
		{"command substitution value", `go test ./internal/cmd -run "$(echo TestX)"`, true},
		{"backtick substitution value", "go test ./internal/cmd -run \"`echo TestX`\"", true},
		{"variable value after a narrow one", `go test ./internal/cmd -run TestX -run "$PATTERN"`, true},
		{"slash inside a bracket class", "go test ./internal/cmd -run '[^/]*'", true},
		{"invalid pattern", "go test ./internal/cmd -run '('", true},
		{"most leading letters", "go test ./internal/cmd -run '^Test[^Z]'", true},
		{"match-all alternative after a subtest path", "go test ./internal/cmd -run 'TestX/a|.'", true},
		{"match-all alternative before a subtest path", "go test ./internal/cmd -run '.|TestX/a'", true},
		{"empty alternative after a subtest path", "go test ./internal/cmd -run 'TestX/a|'", true},

		{"name", "go test ./internal/cmd -run TestX", false},
		{"name, = form", "go test ./internal/cmd -run=TestX", false},
		{"anchored name", "go test ./internal/cmd -run '^TestX$'", false},
		{"alternation of names", "go test ./internal/cmd -run 'TestA|TestB'", false},
		{"three names", "go test ./internal/cmd -run 'TestFoo|TestQuux|TestZeta'", false},
		{"prefix", "go test ./internal/cmd -run '^TestApply'", false},
		{"subtest path", "go test ./internal/cmd -run 'TestX/case'", false},
		{"test binary flag after --", "go test ./internal/cmd -- -test.run TestX", false},
		{"narrow after a match-all one", "go test ./internal/cmd -run . -run TestX", false},
		{"trailing anchor", "go test ./internal/cmd -run 'TestA$|TestB$'", false},
		{"variable value before a narrow one", `go test ./internal/cmd -run "$PATTERN" -run TestX`, false},
		{"slash inside a group", "go test ./internal/cmd -run '(TestA/b)'", false},
		{"alternatives each with a subtest path", "go test ./internal/cmd -run 'TestA/x|TestB/y'", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// The goals gt-gl3jb listed were not the whole set: the integration wall tier,
// the tier check and the timing run each start `go test` over ./... in a Go
// tree (gt-9udxk).
func TestPolecatTestScopeBlocksRemainingWholeSuiteGoals(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	for _, command := range []string{
		"make test-integration-wall",
		"make tier-check",
		"make test-timing",
		"make -j4 tier-check",
		"gt slot run --role gastown/flint -- make test-integration-wall",
	} {
		t.Run(command, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), command)
			if reason == "" {
				t.Errorf("evaluatePolecatTestScope(%q) allowed (matched %v), want blocked", command, matched)
			}
		})
	}
}

// make test-timing runs ./... unless PKGS narrows it (Makefile, docs/testing.md),
// so a PKGS naming light packages is a bounded run; the bare goal, the
// whole-module values, a heavy package and anything the guard cannot read stay
// refused. Only test-timing gets the exemption.
func TestPolecatTestScopeTestTimingPKGSNarrowed(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		command string
		blocked bool
	}{
		{"make test-timing PKGS=./internal/tmux/...", false},
		{"make PKGS=./internal/tmux/... test-timing", false},
		{`make test-timing PKGS="./internal/style ./internal/beads/..."`, false},
		{"make -j4 test-timing PKGS=./internal/style", false},
		{"gt slot run --role gastown/flint -- make test-timing PKGS=./internal/style", false},
		{"make test-timing PKGS=./... PKGS=./internal/style", false},

		{"make test-timing", true},
		{"make test-timing PKGS=", true},
		{"make test-timing PKGS=./...", true},
		{"make test-timing PKGS=...", true},
		{"make test-timing PKGS=./internal/style PKGS=./...", true},
		{`make test-timing PKGS="./internal/style ./..."`, true},
		{"make test-timing PKGS=./internal/cmd", true},
		{"make test-timing PKGS=./internal/...", true},
		{"make test-timing PKGS=./internal/c?d", true},
		{"make test-timing PKGS=$PKGS", true},
		{"make test-timing PKGS=-run=TestX", true},
		{"make test-timing PKGS:=./internal/style", true},
		{"make test-timing gate PKGS=./internal/style", true},
		{"make gate PKGS=./internal/style", true},
		{"make test-integration-wall PKGS=./internal/style", true},
	}
	// Another whole-suite goal beside test-timing stays refused before or after
	// it, so the exemption does not rest on where either sits in the goal list.
	for _, goal := range polecatWholeSuiteMakeGoals {
		if goal == testTimingMakeGoal {
			continue
		}
		tests = append(tests, []struct {
			command string
			blocked bool
		}{
			{"make test-timing " + goal + " PKGS=./internal/style", true},
			{"make " + goal + " test-timing PKGS=./internal/style", true},
			{"make test-timing PKGS=./internal/style && make " + goal, true},
		}...)
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) = %q (matched %v), blocked = %v, want %v", tt.command, reason, matched, blocked, tt.blocked)
			}
		})
	}
}

// A -run that selects most Test functions runs essentially the whole package
// even though it misses the Example, Benchmark and Fuzz probes, so it bounds
// nothing, and a pattern the guard cannot compile is read the same way; a
// pattern that names a few tests, or a prefix of some, still bounds the run.
func TestRunPatternBoundsRunPrefixMatchAll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		bounded bool
	}{
		{"Test", false},
		{"^Test", false},
		{"^Test.*", false},
		{"Test|Example", false},
		{"T", false},
		{"^Test[A-Z]", false},
		{"Test.*/sub", false},
		{"[^/]*", false},
		{"[/]*", false},
		{`\/*`, false},
		{"^Test[^Z]", false},
		{"^Test[A-Q]", false},
		{"(", false},

		{"^TestSpecific$", true},
		{"TestSpecific", true},
		{"^TestApply", true},
		{"TestA|TestB", true},
		{"TestSpecific/case", true},
		{"TestFoo|TestQuux|TestZeta", true},
		{"^Test[A-C]", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := runPatternBoundsRun(tt.pattern); got != tt.bounded {
				t.Errorf("runPatternBoundsRun(%q) = %v, want %v", tt.pattern, got, tt.bounded)
			}
		})
	}
}

// A "/" separates subtest elements only outside a bracket class and
// parentheses, and not when escaped, as testing splits a -run pattern.
func TestFirstSubtestElement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern string
		want    string
	}{
		{"TestX", "TestX"},
		{"TestX/case", "TestX"},
		{"TestX/a/b", "TestX"},
		{"[^/]*/sub", "[^/]*"},
		{"(a/b)/sub", "(a/b)"},
		{"([/])/sub", "([/])"},
		{`a\/b/sub`, `a\/b`},
		{"[a-z/", "[a-z/"},
		{"a]/sub", "a]"},
		{"/sub", ""},
		{"TestA|TestB/sub", "(?:TestA)|(?:TestB)"},
		{"TestX/a|.", "(?:TestX)|(?:.)"},
		{"(A|B)/sub|C", "(?:(A|B))|(?:C)"},
		{"[a|b]/sub|C/d", "(?:[a|b])|(?:C)"},
		{`a\|b/sub`, `a\|b`},
		{"TestX/a|", "(?:TestX)|(?:)"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if got := firstSubtestElement(tt.pattern); got != tt.want {
				t.Errorf("firstSubtestElement(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
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

// The goals gt-gl3jb added read the same premise as make test: a rig outside
// every Go module has nothing to refuse, a Go tree keeps the refusal, and a
// directory the guard cannot place keeps it too (gt-dieu9, gt-ofj05).
func TestPolecatTestScopeWholeSuiteGoalsFollowTheTree(t *testing.T) {
	t.Parallel()
	nonGo := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	for _, goal := range polecatWholeSuiteMakeGoals {
		tests := []struct {
			name    string
			command string
			cwd     string
			blocked bool
		}{
			{"non-Go rig", "make " + goal, nonGo, false},
			{"non-Go rig with jobs flag", "make -j4 " + goal, nonGo, false},
			{"non-Go rig, cd within it", "cd " + nonGo + " && make " + goal, nonGo, false},
			{"Go tree", "make " + goal, goRoot, true},
			{"make -C into the Go tree", "make -C " + goRoot + " " + goal, nonGo, true},
			{"cd into the Go tree", "cd " + goRoot + " && make " + goal, nonGo, true},
			{"unset variable in the cd target", `cd "$GT_PE7IY_UNSET" && make ` + goal, nonGo, true},
			{"command substitution in the cd target", `cd "$(printf /x)" && make ` + goal, nonGo, true},
			{"make -C a shell-expanded directory", `make -C "$GT_PE7IY_UNSET" ` + goal, nonGo, true},
		}
		for _, tt := range tests {
			t.Run(goal+"/"+tt.name, func(t *testing.T) {
				reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, tt.cwd), tt.command)
				if got := reason != ""; got != tt.blocked {
					t.Errorf("evaluatePolecatTestScope(%q, cwd %s) blocked = %v (reason %q, matched %v), want %v", tt.command, tt.cwd, got, reason, matched, tt.blocked)
				}
			})
		}
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

// A bash -c or sh -c payload is a command line of its own. The segment judge
// saw it as one quoted word, so a polecat ran the whole suite through a
// shell it started (gt-y1zks). The payload is judged in the segment's
// directory, and one that runs no go or make test stays allowed.
func TestPolecatTestScopeBlocksShellCPayload(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		cwd     string
		blocked bool
	}{
		{"make gate", "bash -c 'make gate'", root, true},
		{"whole repo go test", "bash -c 'go test ./...'", root, true},
		{"double quoted heavy package", `sh -c "go test ./internal/cmd"`, root, true},
		{"path-qualified shell", "/bin/bash -c 'make gate'", root, true},
		{"login shell flag cluster", "bash -lc 'make gate'", root, true},
		{"env prefix on the shell", "GOFLAGS=-p=8 bash -c 'make gate'", root, true},
		{"after another segment", "gofmt -l . && bash -c 'make gate'", root, true},
		{"nested shell", `bash -c "sh -c 'make gate'"`, root, true},
		{"payload cds into a heavy package", `bash -c "cd internal/cmd && go test"`, root, true},
		{"shell started in a heavy package", "bash -c 'go test .'", filepath.Join(root, "internal", "cmd"), true},
		{"shell started after a cd into a heavy package", "cd internal/cmd && bash -c 'go test'", root, true},
		{"slot-wrapped shell", "gt slot run --role gastown/flint -- bash -c 'make gate'", root, true},

		{"echo", "bash -c 'echo hi'", root, false},
		{"filtered heavy package", `bash -c "go test ./internal/cmd -run TestOne"`, root, false},
		{"light package", "bash -c 'go test ./internal/style'", root, false},
		{"make lint", "bash -c 'make lint'", root, false},
		{"script file, not a payload", "bash ./run.sh -c 'make gate'", root, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// An eval string and a script piped into a shell are command lines the segment
// judges read as quoted words and an echo, so both evaluators judge them as the
// scripts they are (gt-1daus).
func TestPolecatTestScopeEvalPayload(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"eval make gate", `eval "make gate"`, true},
		{"eval go test of a heavy package", `eval "go test ./internal/cmd"`, true},
		{"echo piped into sh", `echo 'make gate' | sh`, true},
		{"echo piped into bash", `echo 'go test ./internal/cmd' | bash`, true},
		{"printf piped into a path-qualified shell", `printf '%s\n' 'make gate' | /bin/sh`, true},
		{"echo piped into sh -s", `echo 'make gate' | sh -s`, true},
		{"echo piped into sh -", `echo 'make gate' | sh -`, true},
		{"piped through cat into sh", `echo 'make gate' | cat | sh`, true},
		{"echo piped into sh after a cd", `cd internal/cmd && echo 'go test .' | sh`, true},

		{"eval echo", `eval "echo hi"`, false},
		{"echo piped into sh, harmless", `echo 'make lint' | sh`, false},
		{"echo piped into sh, filtered", `echo 'go test ./internal/cmd -run TestX' | sh`, false},
		{"echo piped into grep", `echo 'make gate' | grep gate`, false},
		{"echo piped into a script file", `echo 'make gate' | sh ./run.sh`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			proc := fakeGuardProcess(nil, root)
			reason, matched := evaluatePolecatTestScope(proc, tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
			if tt.blocked {
				// The container-suite rule's make goal is test, not gate.
				command := strings.ReplaceAll(tt.command, "make gate", "make test")
				reason, matched = evaluateContainerSuiteCommand(proc, command, "1")
				if reason == "" {
					t.Errorf("evaluateContainerSuiteCommand(%q) allowed it (matched %v)", command, matched)
				}
			}
		})
	}
}

// A -run value the shell rewrites before go sees it bounds nothing: brace
// expansion turns -run={x,.} into -run=x -run=. (the last one wins), and a $@
// or $! that expands to nothing leaves -run= , which matches every test
// (gt-1daus). The "$" that closes a pattern is the regexp anchor.
func TestPolecatTestScopeRefusesExpandedRunValue(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"brace expansion, = form", "go test ./internal/cmd -run={x,.}", true},
		{"brace expansion with a prefix", "go test ./internal/cmd -run=TestX{a,b}", true},
		{"brace expansion in a separate word", "go test ./internal/cmd -run {TestX,.}", true},
		{"brace sequence", "go test ./internal/cmd -run=TestX{1..3}", true},
		{"positional parameters, = form", "go test ./internal/cmd -run=$@", true},
		{"positional parameters, separate word", "go test ./internal/cmd -run $@", true},
		{"last background pid, quoted", `go test ./internal/cmd -run="$!"`, true},
		{"all positional parameters", "go test ./internal/cmd -run=$*", true},
		{"shell pid", "go test ./internal/cmd -run=$$", true},

		{"anchored name", "go test ./internal/cmd -run '^TestX$'", false},
		{"anchored alternatives", "go test ./internal/cmd -run 'TestA$|TestB$'", false},
		{"name", "go test ./internal/cmd -run=TestX", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A cd inside "(" … ")" moves the command that follows it in the same group,
// as it does without the parentheses; a group already closed leaves the
// directory alone (gt-y1zks).
func TestPolecatTestScopePlacesCdInsideParens(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"unparenthesized", "cd internal/cmd && go test", true},
		{"parenthesized bare go test", "(cd internal/cmd && go test)", true},
		{"parenthesized dot target", "(cd internal/cmd && go test .)", true},
		{"parenthesized with spaces", "( cd internal/cmd && go test . )", true},
		{"parenthesized make gate", "(cd internal/cmd && make gate)", true},
		{"after an outer cd", "cd internal && (cd cmd && go test)", true},
		{"nested group closed before the test", "(cd internal/style && (cd ../cmd) && go test)", false},
		{"group closed before the test", "(cd internal/cmd); go test", false},
		{"light package", "(cd internal/style && go test .)", false},
		{"filtered heavy package", "(cd internal/cmd && go test -run TestOne)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, root), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A go test flag takes its value from the next token in the single-dash and
// the double-dash spelling alike (go accepts both), so a value is never a
// package argument.
func TestGoTestPackageArgsSkipsFlagValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rest []string
		want []string
	}{
		{"single-dash run value", []string{"-run", "TestOne", "./internal/cmd"}, []string{"./internal/cmd"}},
		{"double-dash run value", []string{"--run", "TestOne", "./internal/cmd"}, []string{"./internal/cmd"}},
		{"double-dash timeout value", []string{"--timeout", "5m", "./internal/cmd"}, []string{"./internal/cmd"}},
		{"double-dash skip pattern", []string{"--skip", "Test*", "./internal/style"}, []string{"./internal/style"}},
		{"double-dash value in equals form", []string{"--run=TestOne", "./internal/cmd"}, []string{"./internal/cmd"}},
		{"double-dash boolean flag keeps the next token", []string{"--race", "./internal/cmd"}, []string{"./internal/cmd"}},
		{"unknown double-dash flag is not a value flag", []string{"--nonesuch", "x", "./internal/cmd"}, []string{"x", "./internal/cmd"}},
		{"value flag with no value", []string{"--run"}, nil},
		{"arguments after -- belong to the test binary", []string{"./internal/cmd", "--", "--run", "TestOne"}, []string{"./internal/cmd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := goTestPackageArgs(tt.rest)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("goTestPackageArgs(%q) = %q, want %q", tt.rest, got, tt.want)
			}
		})
	}
}

// A -c payload whose segment directory the walk cannot place is judged as
// unplaced, not read against the hook's directory: from a rig outside every Go
// module the base says nothing about where the shell went, and a payload that
// runs the suite there is refused like the unplaceable make -C it is
// (gt-rws47). One that runs no suite stays allowed, and a placed directory is
// still what decides.
func TestPolecatTestScopeJudgesUnplacedShellCPayload(t *testing.T) {
	t.Parallel()
	nonGo := filepath.Join(t.TempDir(), "gastown", "polecats", "dag")
	if err := os.MkdirAll(nonGo, 0o755); err != nil {
		t.Fatal(err)
	}
	goTree := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		cwd     string
		blocked bool
	}{
		{"unplaced cd, payload makes the suite", `cd "$(git rev-parse --show-toplevel)" && bash -c 'make gate'`, nonGo, true},
		{"unplaced cd, nested payload", `cd "$(git rev-parse --show-toplevel)" && bash -c "sh -c 'make gate'"`, nonGo, true},
		{"unplaced cd, payload runs a heavy package", `cd "$(git rev-parse --show-toplevel)" && bash -c 'go test ./internal/cmd'`, goTree, true},
		{"unplaced cd, payload runs nothing", `cd "$(git rev-parse --show-toplevel)" && bash -c 'echo hi'`, nonGo, false},
		{"unplaced cd, filtered payload", `cd "$(git rev-parse --show-toplevel)" && bash -c 'go test ./internal/cmd -run TestOne'`, goTree, false},
		{"placed non-Go cwd, payload makes the suite", "bash -c 'make gate'", nonGo, false},
		{"placed Go cwd, payload makes the suite", "bash -c 'make gate'", goTree, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A go test or make goal inside a command substitution, backticks or eval is a
// command line of its own: the segment judge read "out=$(go" as a word that is
// no go binary, "$(make" as no make, and eval's quoted string as one opaque
// token, so a whole-suite run started on the host unblocked (gt-t1pct). The
// bodies are judged in the directory the cds ahead of them leave the shell in,
// and a body that runs no suite, or a "$(" the shell leaves literal, stays
// allowed.
func TestPolecatTestScope_NestedShapes(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		cwd     string
		blocked bool
	}{
		{"assignment, heavy package in a pipeline", `out=$(go test ./internal/cmd 2>&1 | tail -20)`, root, true},
		{"echo of make gate", `echo $(make gate)`, root, true},
		{"backticks", "`make test`", root, true},
		{"eval string", `eval "go test ./..."`, root, true},

		{"quoted substitution", `echo "$(make gate)"`, root, true},
		{"backticks in double quotes", "echo \"`make test`\"", root, true},
		{"backticks after an echo", "echo `make gate`", root, true},
		{"substitution in a later segment", `gofmt -l . && x=$(make gate)`, root, true},
		{"substitution as an argument of go", `go vet $(go test ./...)`, root, true},
		{"nested substitution", `echo $(echo $(make gate))`, root, true},
		{"body holding a nested substitution", `out=$(go test ./internal/cmd 2>&1 | tail -n $(nproc))`, root, true},
		{"body holding a grep group", `out=$(go test ./internal/cmd 2>&1 | grep -E '(ok|FAIL)')`, root, true},
		{"body holding an awk field group", `out=$(go test ./internal/cmd 2>&1 | awk '{print $(NF)}')`, root, true},
		{"body holding a quoted parenthesis", `out=$(echo ")" && go test ./internal/cmd)`, root, true},
		{"substitution inside bash -c", `bash -c 'x=$(make gate)'`, root, true},
		{"substitution in a bash -c payload in double quotes", `bash -c "echo $(make gate)"`, root, true},
		{"substitution with an env prefix", `x=$(GOFLAGS=-count=1 go test ./internal/daemon)`, root, true},
		{"eval of make gate", `eval make gate`, root, true},
		{"eval after an assignment", `X=1 eval "make gate"`, root, true},
		{"eval inside a group", `(eval "go test ./...")`, root, true},
		{"eval of a nested eval", `eval 'eval "make gate"'`, root, true},
		{"eval with a substitution", `eval "x=$(make gate)"`, root, true},
		{"eval in an if branch", `if true; then eval "make gate"; fi`, root, true},
		{"slot-wrapped substitution", `gt slot run --role gastown/flint -- echo $(make gate)`, root, true},
		{"substitution after a cd into a heavy package", `cd internal/cmd && x=$(go test)`, root, true},
		{"substitution after a cd, dot target", `cd internal/cmd && x=$(go test .)`, root, true},
		{"substitution run from a heavy package", `x=$(go test)`, filepath.Join(root, "internal", "cmd"), true},
		{"substitution after an unplaced cd", `cd "$(git rev-parse --show-toplevel)/internal/cmd" && x=$(go test)`, root, true},
		{"substitution past the nesting cap", `bash -c "bash -c 'bash -c \"echo $(pwd)\"'"`, root, true},

		{"pwd", `echo $(pwd)`, root, false},
		{"go env", `x=$(go env GOMODCACHE)`, root, false},
		{"backtick pwd", "echo `pwd`", root, false},
		{"git rev-parse", `cd "$(git rev-parse --show-toplevel)" && echo hi`, root, false},
		{"filtered heavy package", `out=$(go test ./internal/cmd -run TestOne)`, root, false},
		{"filtered heavy package, body holding a group", `out=$(go test ./internal/cmd -run TestOne 2>&1 | grep -E '(ok|FAIL)')`, root, false},
		{"body holding a nested pwd", `out=$(echo $(pwd))`, root, false},
		{"single-quoted parenthesis hiding a later substitution", `echo '$(' "$(pwd)" ')'`, root, false},
		{"light package", `out=$(go test ./internal/style)`, root, false},
		{"make lint", `x=$(make lint)`, root, false},
		{"empty substitution", `echo $()`, root, false},
		{"single-quoted substitution", `echo '$(make gate)'`, root, false},
		{"single-quoted backticks", "gt bead note gt-x 'ran `make test` here'", root, false},
		{"escaped substitution", `echo \$(make gate)`, root, false},
		{"escaped backticks in double quotes", "echo \"\\`make gate\\`\"", root, false},
		{"apostrophe inside double quotes", `echo "don't $(make gate)"`, root, true},
		{"apostrophe inside double quotes, harmless body", `echo "don't $(pwd)"`, root, false},
		{"quoted heredoc body", "cat <<'EOF'\n$(make gate)\nEOF", root, false},
		{"eval of echo", `eval "echo hi"`, root, false},
		{"eval as an argument", `grep eval notes.txt`, root, false},
		{"eval named by echo", `echo eval hi`, root, false},
		{"eval with no argument", `eval`, root, false},
		{"eval of a filtered run", `eval "go test ./internal/cmd -run TestOne"`, root, false},
		{"substitution after a cd into a light package", `cd internal/style && x=$(go test)`, root, false},
		{"substitution run from a light package", `x=$(go test)`, filepath.Join(root, "internal", "style"), false},
		{"substitution after a closed subshell cd", `(cd internal/cmd); x=$(go test ./internal/style)`, root, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}
