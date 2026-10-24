package cmd

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestEvaluateContainerSuiteCommand(t *testing.T) {
	t.Parallel()
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
		{"GOFLAGS prefixed go test on container package", "GT_TEST_DOCKER=1 GOFLAGS=-p=6 go test ./internal/mail/...", true},
		{"go test with -run flag on container package", "GT_TEST_DOCKER=1 go test ./internal/beads/... -run TestFoo -v", true},
		{"switch via export in an earlier segment", "export GT_TEST_DOCKER=1; go test ./internal/beads/...", true},
		{"switch via env(1)", "env GT_TEST_DOCKER=1 go test ./internal/beads/...", true},
		{"switch quoted", `GT_TEST_DOCKER="1" go test ./internal/beads/...`, true},

		// A redirection ahead of the command is setup the shell performs
		// before it runs the command, so the test run behind it is judged as
		// the plain line is (gt-spmli).
		{"go test behind a leading redirection", "GT_TEST_DOCKER=1 2>/dev/null go test ./internal/beads/...", true},
		{"go test with a redirection ahead of the opt-in", "2>/dev/null GT_TEST_DOCKER=1 go test ./internal/beads/...", true},
		{"make test behind a leading redirection", "2>/dev/null make test", true},
		{"go test non-container package behind a leading redirection", "2>/dev/null go test ./internal/style/...", false},
		{"go test container package behind a leading redirection, switch off", "2>/dev/null go test ./internal/beads/...", false},
		{"go test wrapped in gt slot run behind a leading redirection", "GT_TEST_DOCKER=1 2>/dev/null gt slot run --role gastown/refinery -- go test ./internal/beads/...", false},

		// Blocked: go's global -C dir flag may sit between the binary and the
		// subcommand, in either spelling, and the invocation must be read
		// through it — the same walk gt-t7p80/gt-soohm added for go clean
		// (gt-saorq).
		{"go -C dir flag before the subcommand", "GT_TEST_DOCKER=1 go -C /tmp test ./internal/beads/...", true},
		{"go -C=dir flag before the subcommand", "GT_TEST_DOCKER=1 go -C=/tmp test ./internal/beads/...", true},
		{"go -C dir flag, repeated", "GT_TEST_DOCKER=1 go -C /a -C /b test ./internal/beads/...", true},
		{"go -C dir flag with a whole-repo target", "GT_TEST_DOCKER=1 go -C /tmp test ./...", true},
		{"go -C dir flag with a module-prefixed container package", "GT_TEST_DOCKER=1 go -C /tmp test github.com/steveyegge/gastown/internal/beads/...", true},
		{"go -C into a directory itself named test", "GT_TEST_DOCKER=1 go -C test test ./internal/beads/...", true},
		// The walk names the binary by isGoBinaryName, so a versioned
		// toolchain owns the invocation too: it starts the same containers.
		{"versioned go binary on a container package", "GT_TEST_DOCKER=1 go1.22.0 test ./internal/beads/...", true},

		// Allowed: the same bare runs with the switch off cannot reach Docker.
		{"bare go test container package, switch off", "go test ./internal/beads/...", false},
		{"bare go test container package, switch explicitly 0", "GT_TEST_DOCKER=0 go test ./internal/beads/...", false},
		{"bare filtered go test on internal/cmd, switch off", "go test ./internal/cmd/ -run TestFoo", false},
		{"bare go test whole repo, switch off", "go test ./...", false},

		// Blocked: bare make test. cwd here is "" (unresolvable), which the
		// make rule treats as a Go tree; TestMakeTestIsWholeGoSuite covers
		// the resolved cases.
		{"make test bare", "make test", true},
		{"GOFLAGS prefixed make test", "GOFLAGS=-p=6 make test", true},
		{"make test with jobs flag", "make -j4 test", true},
		{"make test with directory flag and value", "make -C . test", true},
		{"make test with valueless flags", "make -e -w -i test", true},
		{"make test with bare jobs flag", "make -j test", true},
		{"make test with spaced jobs value", "make -j 4 test", true},
		// make runs every goal it is given, so test need not come first
		// (gt-pm4vn).
		{"make test after another goal", "make clean test", true},
		{"make test after the build goal", "make build test", true},
		{"make test after a variable assignment", "make GOFLAGS=-count=1 test", true},
		{"make test after a flag and a goal", "make -j4 clean test", true},
		{"make clean without test", "make clean", false},
		{"make clean build without test", "make clean build", false},

		// Allowed: wrapped in gt slot run.
		{"go test wrapped in gt slot run", "gt slot run --role gastown/refinery -- go test ./internal/beads/...", false},
		{"make test wrapped in gt slot run", "gt slot run --role gastown/refinery -- GOFLAGS=-p=6 make test", false},
		{"go test whole repo wrapped", "gt slot run --role gastown/refinery -- go test ./...", false},

		// Allowed: non-container packages.
		{"go test non-container package", "go test ./internal/style/...", false},
		{"go test multiple non-container packages", "go test ./internal/style/... ./internal/version/...", false},
		{"go test bare no package args", "go test", false},
		{"go test with -v only", "go test -v", false},

		// Allowed: the -C walk costs exactly the flag and its directory, so a
		// command whose container test is still hidden behind another flag is
		// not read as an invocation, and a non-test subcommand is not either.
		{"go -C dir flag, switch off", "go -C /tmp test ./internal/beads/...", false},
		{"go -C dir flag on a non-container package", "GT_TEST_DOCKER=1 go -C /tmp test ./internal/style/...", false},
		{"a flag other than -C between go and test", "GT_TEST_DOCKER=1 go -v test ./internal/beads/...", false},
		{"go -C dir flag with a non-test subcommand", "GT_TEST_DOCKER=1 go -C /tmp build ./internal/beads/...", false},
		{"go -C dir flag with no subcommand", "GT_TEST_DOCKER=1 go -C /tmp", false},

		// A bare or "." target is judged in the directory -C names, not in
		// the hook's own (unreadable here, which getwd's failure makes empty).
		// A -C dir the guard can place answers for the target; one it cannot
		// is refused, the reading an unplaceable make -C gets (gt-tzdpe).
		{"go -C to a placed dir, bare target", "GT_TEST_DOCKER=1 go -C /tmp test .", false},
		{"go -C to a relative dir under an unplaced cwd", "GT_TEST_DOCKER=1 go -C internal/beads test .", true},
		{"go -C= to a relative dir under an unplaced cwd", "GT_TEST_DOCKER=1 go -C=internal/beads test .", true},
		{"go -C to a relative dir under an unplaced cwd, no target", "GT_TEST_DOCKER=1 go -C internal/beads test", true},

		// A hook cwd the guard cannot read (getwd failed, so the walk has "")
		// is no directory to resolve a bare or "." target against either, so
		// the target is refused rather than exempted for want of one.
		{"cwd-relative go test with an unreadable cwd", "GT_TEST_DOCKER=1 go test .", true},
		{"bare go test with an unreadable cwd", "GT_TEST_DOCKER=1 go test", true},

		// Blocked: the spelling of a package argument is irrelevant. Go cleans
		// the path, so "//", "/./" and "/x/../" reach the same package as the
		// plain spelling, and a guard comparing the raw text matched none of
		// them (gt-d1e8c).
		{"double slash in a container path", "GT_TEST_DOCKER=1 go test ./internal//beads/...", true},
		{"dot element in a container path", "GT_TEST_DOCKER=1 go test ./internal/./beads", true},
		{"dot-dot element in a container path", "GT_TEST_DOCKER=1 go test ./internal/x/../beads", true},
		{"unclean path with a trailing slash and dots", "GT_TEST_DOCKER=1 go test ./internal//beads/.../", true},
		{"unclean path, module-prefixed", "GT_TEST_DOCKER=1 go test github.com/steveyegge/gastown//internal/./beads", true},
		{"unclean path climbing to the whole repo", "GT_TEST_DOCKER=1 go test ./internal/../...", true},
		{"unclean light path stays allowed", "GT_TEST_DOCKER=1 go test ./internal//style/./...", false},
		{"dot-dot element between light paths", "GT_TEST_DOCKER=1 go test ./internal/style/../version", false},

		// Blocked: a wildcard inside a path element reaches packages the guard
		// cannot name without expanding it, so it is refused whatever it
		// might reach; the shell globs are seen unexpanded (gt-d1e8c).
		{"partial wildcard reaching internal/beads", "GT_TEST_DOCKER=1 go test ./internal/b...", true},
		{"partial wildcard reaching internal/cmd", "GT_TEST_DOCKER=1 go test ./internal/c...", true},
		{"wildcard element in the middle of a path", "GT_TEST_DOCKER=1 go test ./internal/.../beads", true},
		{"wildcard element leading a path", "GT_TEST_DOCKER=1 go test ./.../beads", true},
		{"star glob", "GT_TEST_DOCKER=1 go test ./internal/b*", true},
		{"question-mark glob", "GT_TEST_DOCKER=1 go test ./internal/bead?", true},
		{"bracket glob", "GT_TEST_DOCKER=1 go test ./internal/[b]eads", true},
		{"brace expansion", "GT_TEST_DOCKER=1 go test ./internal/{beads,style}", true},
		{"star glob hiding behind a dot-dot", "GT_TEST_DOCKER=1 go test ./internal/b*/../beads", true},
		{"wildcard beside a plain container package", "GT_TEST_DOCKER=1 go test ./internal/b... ./internal/style", true},
		{"character-class glob reaching two container packages", "GT_TEST_DOCKER=1 go test ./internal/[bc]*", true},
		{"wildcard leading the path", "GT_TEST_DOCKER=1 go test ./*/beads", true},
		{"wildcard inside a subpackage of a container package", "GT_TEST_DOCKER=1 go test ./internal/beads/s*", true},

		// Blocked: an argument the shell rewrites before go sees it names a
		// package the hook cannot read from the text (gt-e40zb).
		{"command substitution in a path element", "GT_TEST_DOCKER=1 go test ./internal/$(echo beads)", true},
		{"backtick substitution in a path element", "GT_TEST_DOCKER=1 go test './internal/`echo beads`'", true},
		{"variable leading an absolute path", "GT_TEST_DOCKER=1 go test $PWD/internal/beads", true},
		{"braced variable in a path element", "GT_TEST_DOCKER=1 go test ./internal/${P}", true},
		{"home directory prefix", "GT_TEST_DOCKER=1 go test ~/x/internal/beads", true},
		{"another user's home directory prefix", "GT_TEST_DOCKER=1 go test ~root/x/internal/beads", true},
		{"variable set in an earlier segment", "P=beads; GT_TEST_DOCKER=1 go test ./internal/$P", true},
		{"variable set before an exported switch", "P=beads; export GT_TEST_DOCKER=1; go test ./internal/$P", true},
		{"expanded argument beside a light package", "GT_TEST_DOCKER=1 go test ./internal/style $PWD/internal/beads", true},

		// Allowed: a pattern the list cannot reach stays allowed — refusing it
		// would cost a wrapped run for nothing.
		{"partial wildcard reaching only a light package", "GT_TEST_DOCKER=1 go test ./internal/st...", false},
		{"star glob reaching only a light package", "GT_TEST_DOCKER=1 go test ./internal/s*", false},
		{"question-mark glob reaching only light packages", "GT_TEST_DOCKER=1 go test ./internal/sty?e", false},
		{"wildcard outside internal", "GT_TEST_DOCKER=1 go test ./c*/gt", false},
		{"wildcard beside a light package", "GT_TEST_DOCKER=1 go test ./internal/st... ./internal/version", false},
		{"wildcard, wrapped in gt slot run", "gt slot run --role gastown/refinery -- go test ./internal/b...", false},
		{"wildcard, switch off", "go test ./internal/b...", false},
		{"expanded argument, switch off", "go test ./internal/$(echo beads)", false},
		{"expanded argument, wrapped in gt slot run", "gt slot run --role gastown/refinery -- go test $PWD/internal/beads", false},
		{"pattern value of -skip is not a package", "GT_TEST_DOCKER=1 go test -skip 'Test*' ./internal/style", false},
		{"pattern value of -run is not a package", "GT_TEST_DOCKER=1 go test -run 'Foo.*' ./internal/style", false},

		// Allowed: a "$" in a flag's value is not a package argument the shell
		// rewrites (gt-yyw5e). goTestPackageArgs drops the value, so only
		// shellExpandedPackageArg on a real package argument refuses.
		{"variable as the value of -run", "GT_TEST_DOCKER=1 go test -run \"$PAT\" ./internal/style", false},
		{"variable in an =-joined flag value", "GT_TEST_DOCKER=1 go test -count=$N ./internal/style", false},
		{"command substitution as a flag value", "GT_TEST_DOCKER=1 go test -timeout $(echo 5m) ./internal/style", false},
		{"variable as the value of -skip", "GT_TEST_DOCKER=1 go test -skip \"$PAT\" ./internal/style", false},
		{"variable flag value beside a plain light package", "GT_TEST_DOCKER=1 go test -tags=$TAGS ./internal/version ./internal/style", false},
		{"variable flag value, container package still blocked", "GT_TEST_DOCKER=1 go test -run \"$PAT\" ./internal/beads", true},
		{"variable flag value, expanded package still blocked", "GT_TEST_DOCKER=1 go test -count=$N $PWD/internal/beads", true},

		// Allowed: unrelated commands.
		{"unrelated command", "ls -la", false},
		{"empty command", "", false},
		{"go build", "go build ./...", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, ""), tt.command, "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// assertBothGuardsJudge runs a case through both evaluators: the container
// suite rule and the polecat scope rule read the same invocation, so both
// must reach the same verdict.
func assertBothGuardsJudge(t *testing.T, command, cwd string, blocked bool) {
	t.Helper()
	proc := fakeGuardProcess(nil, cwd)
	reason, _ := evaluateContainerSuiteCommand(proc, command, "")
	if got := reason != ""; got != blocked {
		t.Errorf("evaluateContainerSuiteCommand(%q, cwd %s) blocked = %v (reason %q), want %v", command, cwd, got, reason, blocked)
	}
	scopeReason, _ := evaluatePolecatTestScope(proc, command)
	if got := scopeReason != ""; got != blocked {
		t.Errorf("evaluatePolecatTestScope(%q, cwd %s) blocked = %v (reason %q), want %v", command, cwd, got, scopeReason, blocked)
	}
}

// assertGuardsJudge checks each rule's verdict separately, for the cases where
// the two rules do not agree: the scope rule owns the whole-repo wildcard and
// the heavy packages, the container rule owns the testcontainers packages and
// the docker opt-in, so a shared expectation would only pin their overlap.
func assertGuardsJudge(t *testing.T, command, cwd string, containerBlocked, scopeBlocked bool) {
	t.Helper()
	proc := fakeGuardProcess(nil, cwd)
	if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); (reason != "") != containerBlocked {
		t.Errorf("evaluateContainerSuiteCommand(%q, cwd %s) blocked = %v (reason %q), want %v", command, cwd, reason != "", reason, containerBlocked)
	}
	if reason, _ := evaluatePolecatTestScope(proc, command); (reason != "") != scopeBlocked {
		t.Errorf("evaluatePolecatTestScope(%q, cwd %s) blocked = %v (reason %q), want %v", command, cwd, reason != "", reason, scopeBlocked)
	}
}

// A guarded segment is judged in the directory it runs in, not the one the
// hook was invoked from: a cd earlier on the same shell line carries forward,
// by the same walk (and the same rules) the scan rule uses. Without the carry
// a polecat in a non-Go rig runs the suite in a Go tree through a cd
// (gt-5mc21).
func TestGuardsFollowCdOnTheLine(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	nonGo := nonGoTree(t)
	// A Go tree nested inside the non-Go rig, so a relative cd has a real
	// directory to resolve against. A go.mod under a tree does not make the
	// tree itself a module — goModuleRoot walks up, not down — so nonGo stays
	// outside every module.
	nested := filepath.Join(nonGo, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/nested\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	heavy := filepath.Join(goRoot, "internal", "cmd")
	container := filepath.Join(goRoot, "internal", "beads")
	light := filepath.Join(goRoot, "internal", "style")
	// Links into and out of the Go tree. The target is a package inside the
	// tree, not its root: a link to the root would be found by the lexical
	// walk too, so it could not tell a resolved walk from an unresolved one.
	goLink := symlinkedTree(t, heavy)
	lightLink := symlinkedTree(t, light)
	nonGoLink := symlinkedTree(t, nonGo)
	// A link that sits in the Go tree but leaves it: resolving it must exempt
	// the make, which a guard that merely refused every symlinked path could
	// not do.
	outLink := symlinkAt(t, filepath.Join(goRoot, "out-of-tree"), nonGo)

	tests := []struct {
		name                           string
		cwd, command                   string
		containerBlocked, scopeBlocked bool
	}{
		// The hole: the non-Go rig's segment runs in the Go tree anyway.
		{"cd into the Go tree then make test", nonGo, "cd " + goRoot + " && make test", true, true},
		{"cd into the Go tree after a semicolon", nonGo, "cd " + goRoot + "; make test", true, true},
		{"relative cd into a nested Go tree", nonGo, "cd nested && make test", true, true},
		{"chained cds, last one into the Go tree", nonGo, "cd nested && cd " + goRoot + " && make test", true, true},

		// The same line in a tree the rule has nothing to refuse stays allowed.
		{"cd to the non-Go tree then make test", nonGo, "cd " + nonGo + " && make test", false, false},
		{"chained cds, last one out of the Go tree", nonGo, "cd " + goRoot + " && cd " + nonGo + " && make test", false, false},

		// Leaving the Go tree is judged where the command runs too, so the
		// hook's own tree no longer decides a segment that left it.
		{"cd out of the Go tree then make test", goRoot, "cd " + nonGo + " && make test", false, false},
		{"cd out of the container package", container, "cd " + light + " && GT_TEST_DOCKER=1 go test .", false, false},
		{"cd into the container package", nonGo, "cd " + container + " && GT_TEST_DOCKER=1 go test .", true, false},
		{"cd into a heavy package", nonGo, "cd " + heavy + " && go test .", false, true},

		// A cd and make's own -C compose: the -C is resolved against the
		// directory the segment runs in, which the cd is what set.
		{"cd into the Go tree, make -C back out", nonGo, "cd " + goRoot + " && make -C " + nonGo + " test", false, false},
		{"cd out of the Go tree, make -C back in", nonGo, "cd " + nonGo + " && make -C " + goRoot + " test", true, true},

		// Only "&&" and ";" carry the change — a cd in a pipeline or a
		// background job runs in a subshell of its own, so both keep the
		// directory the shell already had, and one before "||" carries its
		// own change nowhere either (scanWalkRoot's rule; gt-0lzdi).
		{"cd in a pipeline does not carry", nonGo, "cd " + goRoot + " | make test", false, false},
		{"background cd does not carry", nonGo, "cd " + goRoot + " & make test", false, false},
		{"cd before || does not carry", nonGo, "cd " + goRoot + " || make test", false, false},

		// A "||" chain runs the segment in its right-hand cd's directory only
		// when the left cd certainly failed, so the chain neither hides the
		// Go tree the shell is really in (gt-0lzdi) nor refuses a segment that
		// stays in the rig's own tree.
		{"cd out of the Go tree before a || chain", goRoot, "cd " + goRoot + " || cd " + nonGo + " && make test", true, true},
		{"cd into the Go tree before a || chain", nonGo, "cd " + nonGo + " || cd " + goRoot + " && make test", false, false},
		{"a refused cd runs make in the || chain's Go tree", nonGo, "cd /nonexistent/gt-0lzdi || cd " + goRoot + " && make test", true, true},
		{"an unresolvable cd does not name the || chain", goRoot, "cd - || cd " + nonGo + " && make test", true, true},

		// A symlinked directory is judged as the tree it names, both when make
		// changes into it and when the cd does (gt-ofj05).
		{"make -C through a link into the Go tree", nonGo, "make -C " + goLink + " test", true, true},
		{"cd through a link into the Go tree", nonGo, "cd " + goLink + " && make test", true, true},
		{"cd through a link onto a light package", nonGo, "cd " + lightLink + " && GT_TEST_DOCKER=1 go test .", false, false},
		{"symlinked session cwd in the Go tree", goLink, "make test", true, true},

		// A cd this process cannot place leaves the directory unknown, which
		// keeps the refusal an unplaceable make -C already gets — the hook's own
		// cwd no longer answers for it. A cd the shell refuses does not: it left
		// the shell where it was (gt-ofj05).
		{"unset variable in the cd target", nonGo, `cd "$GT_OFJ05_UNSET" && make test`, true, true},
		{"command substitution in the cd target", nonGo, `cd "$(printf /x)" && make test`, true, true},
		{"cwd-relative go test after an unplaceable cd", nonGo, `cd "$GT_OFJ05_UNSET" && GT_TEST_DOCKER=1 go test .`, true, true},
		{"a refused cd keeps the hook cwd, Go tree", goRoot, "cd /nonexistent/gt-5mc21 && make test", true, true},
		{"a refused cd keeps the hook cwd, non-Go tree", nonGo, "cd /nonexistent/gt-5mc21 && make test", false, false},

		// The controls: a directory the walk can place is still read on its own
		// merits, so a non-Go tree stays exempt however the line reaches it.
		{"no cd at all, non-Go hook cwd", nonGo, "make test", false, false},
		{"cd to a real non-Go directory", nonGo, "cd " + nonGo + " && make test", false, false},
		{"make -C through a link to a non-Go tree", nonGo, "make -C " + nonGoLink + " test", false, false},
		{"cd through a link to a non-Go tree", nonGo, "cd " + nonGoLink + " && make test", false, false},
		{"make -C through a link out of the Go tree", goRoot, "make -C " + outLink + " test", false, false},
		{"cd through a link out of the Go tree", goRoot, "cd " + outLink + " && make test", false, false},
		{"symlinked session cwd in a non-Go tree", nonGoLink, "make test", false, false},

		// The walk keeps going past a change it cannot resolve: a later
		// absolute cd needs no base and names the tree again, so the segment
		// after it is judged there (gt-n7ksl). A change that does not carry
		// never reaches this shell at all, resolvable or not, so it cannot
		// blank the directory either — and one carried by "&&" dead-ends the
		// list, so nothing inside the list after it runs.
		{"a missing cd, then an absolute cd into the Go tree", nonGo, "cd /nonexistent/gt-n7ksl ; cd " + goRoot + " && make test", true, true},
		{"a cd inside the dead list is not named, and the walk stays put", nonGo, "cd /nonexistent/gt-n7ksl && cd " + goRoot + " ; make test", false, false},
		{"a missing cd, then an absolute cd out of the Go tree", goRoot, "cd /nonexistent/gt-n7ksl ; cd " + nonGo + " && make test", false, false},
		{"an unresolvable cd in a pipeline keeps the Go tree", goRoot, "cd /nonexistent/gt-n7ksl | make test", true, true},

		// A cd the shell refuses leaves the shell where it was, so the walk
		// keeps the tree it had and the segment runs there (gt-34vra); the
		// "&&" list that refusal dead-ends still ends at the ";" that follows,
		// and the cd the walk reads from there is the one the segment is judged
		// by.
		{"a refused cd keeps the Go tree the walk was in", nonGo, "cd " + goRoot + " ; cd /nonexistent/gt-34vra ; make test", true, true},
		{"a refused cd keeps the non-Go tree the walk was in", goRoot, "cd " + nonGo + " ; cd /nonexistent/gt-34vra ; make test", false, false},
		{"the walk reads on from the semicolon after a dead list", nonGo, "cd /nonexistent/gt-34vra && true ; cd " + goRoot + " ; make test", true, true},
		{"the same read on out of the Go tree", goRoot, "cd /nonexistent/gt-34vra && true ; cd " + nonGo + " ; make test", false, false},

		// A cd whose separator does not carry runs in a subshell of its own, so
		// it is not gt-ofj05's unplaced cd: the segment's directory is the one
		// the shell already had, however unplaceable the cd's target is, and
		// the guard neither reads it as the Go tree nor refuses on it.
		{"an unplaceable cd in a pipeline keeps the Go tree", goRoot, `cd "$GT_OFJ05_UNSET" | make test`, true, true},
		{"an unplaceable cd in a pipeline stays exempt", nonGo, `cd "$GT_OFJ05_UNSET" | make test`, false, false},

		// pushd changes the shell's directory the same way cd does (gt-n7ksl).
		// The stack it pushes onto is one the walk does not track: a popd
		// leaves the directory unplaced, so the guards refuse rather than judge
		// the segment in the tree the pushd named, and a pushd -n leaves the
		// shell where it was.
		{"pushd into the Go tree then make test", nonGo, "pushd " + goRoot + " && make test", true, true},
		{"pushd out of the Go tree then make test", goRoot, "pushd " + nonGo + " && make test", false, false},
		{"pushd then popd leaves the segment refused", goRoot, "pushd " + nonGo + " && popd && make test", true, true},
		{"pushd -n does not leave the Go tree", goRoot, "pushd -n " + nonGo + " && make test", true, true},

		// A brace group runs in this shell, so a cd inside one reaches the
		// segment after it — a subshell's does not, and the group's own closing
		// "}" must not hide the guarded command that follows either (gt-ajyw8).
		{"cd into the Go tree inside a brace group", nonGo, "cd " + nonGo + " ; { cd " + goRoot + " ; } ; make test", true, true},
		{"cd out of the Go tree inside a brace group", goRoot, "cd " + goRoot + " ; { cd " + nonGo + " ; } ; make test", false, false},
		{"a guarded command inside a brace group runs in its cd", nonGo, "{ cd " + container + " ; GT_TEST_DOCKER=1 go test . ; }", true, false},
		{"cd into the Go tree inside a subshell does not carry", nonGo, "cd " + nonGo + " ; (cd " + goRoot + ") ; make test", false, false},
		{"cd out of the Go tree inside a subshell does not carry", goRoot, "cd " + goRoot + " ; (cd " + nonGo + ") ; make test", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGuardsJudge(t, tt.command, tt.cwd, tt.containerBlocked, tt.scopeBlocked)
		})
	}
}

// A Go tree keeps the refusal. These fixtures carry their own go.mod, so the
// verdict rests on the fixture and not on where the host put its temp dir.
func TestMakeTestInGoTreeIsRefused(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	otherModule := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherModule, "go.mod"), []byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	tests := []struct {
		name    string
		cwd     string
		command string
	}{
		{"make test at the module root", goRoot, "make test"},
		{"make test in a package of the module", filepath.Join(goRoot, "internal", "cmd"), "make test"},
		{"make test with jobs flag in the module", goRoot, "make -j4 test"},
		{"make test after another goal in the module", goRoot, "make clean test"},
		{"make test after the build goal in the module", goRoot, "make build test"},
		{"make test after a variable assignment in the module", goRoot, "make GOFLAGS=-count=1 test"},
		{"make test in another Go module", otherModule, "make test"},
		// A makefile the guard cannot read makes "test" unknown, so even the
		// tree it runs in says nothing about what the target costs.
		{"an alternate makefile", goRoot, "make -f other.mk test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertBothGuardsJudge(t, tt.command, tt.cwd, true)
		})
	}
}

// The make rule's premise holds only in the tree make runs in, and outside
// every Go module it does not hold at all: a bare "make test" there cannot
// reach a containerSuitePackages entry and the refusal's advice has nothing to
// say about it (gt-dieu9). Both directions of the -C are here, because the
// tree that decides is the one make changes into, not the one the hook's cwd
// names — keying on the cwd alone let "make -C <go tree> test" run from
// anywhere (gt-dieu9 review).
func TestMakeTestOutsideGoTree(t *testing.T) {
	t.Parallel()
	npmRoot := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		cwd     string
		command string
		blocked bool
	}{
		{"make test", npmRoot, "make test", false},
		{"make test with env prefix", npmRoot, "GOFLAGS=-p=8 make test", false},
		{"make test in a slot run wrapper", npmRoot, "gt slot run --role fractals/polecats/coral -- make test", false},
		{"make -C out of the Go tree", goRoot, "make -C " + npmRoot + " test", false},
		{"make -C into a subdirectory of the tree", npmRoot, "make -C " + filepath.Join(npmRoot, "sub") + " test", false},
		{"make -C into the Go tree", npmRoot, "make -C " + goRoot + " test", true},
		{"make --directory into the Go tree", npmRoot, "make --directory=" + goRoot + " test", true},
		{"make -C attached into the Go tree", npmRoot, "make -C" + goRoot + " test", true},
		{"make -C chaining into the Go tree", npmRoot, "make -C " + filepath.Join(goRoot, "internal") + " -C cmd test", true},
		// A -C directory the shell expands names a tree the hook cannot see, so
		// it is unplaced and refused rather than joined onto the cwd as a
		// literal that names nothing (gt-8a1pa); a literal one keeps its verdict.
		{"make -C a variable", npmRoot, `make -C "$PWD/sub" test`, true},
		{"make --directory= a variable", npmRoot, `make --directory="$PWD/sub" test`, true},
		{"make -C attached a variable", npmRoot, `make -C"$PWD/sub" test`, true},
		{"make -C a command substitution", npmRoot, `make -C "$(pwd)/sub" test`, true},
		{"make -C a backtick substitution", npmRoot, "make -C `pwd`/sub test", true},
		{"make -C a leading tilde", npmRoot, "make -C ~/sub test", true},
		{"make -C a variable after a literal directory", npmRoot, `make -C sub -C "$SUB" test`, true},
		{"make -C a literal relative directory", npmRoot, "make -C sub test", false},
		// "--" ends make's option processing, so what follows is a target it
		// never reads as a flag: the tree stays the one it started in.
		{"a -C after the end of options", npmRoot, "make -- -C " + goRoot + " test", false},
		{"an alternate makefile", npmRoot, "make -f other.mk test", true},
		{"an attached alternate makefile", npmRoot, "make -fother.mk test", true},
		{"a long-form makefile", npmRoot, "make --makefile=other.mk test", true},
		{"a makefile from stdin", npmRoot, "make - test", true},
		// Short options cluster: the -C or -f behind a valueless letter still
		// owns the next token (or the rest of this one), and a value-taking
		// option ahead of it keeps the letters after it as its own value.
		{"make -sC into the Go tree", npmRoot, "make -sC " + goRoot + " test", true},
		{"make -kC into the Go tree", npmRoot, "make -kC " + goRoot + " test", true},
		{"make -sC attached into the Go tree", npmRoot, "make -sC" + goRoot + " test", true},
		{"make -sC out of the Go tree", goRoot, "make -sC " + npmRoot + " test", false},
		{"make -sC a variable", npmRoot, `make -sC "$PWD/sub" test`, true},
		{"a clustered alternate makefile", npmRoot, "make -sf other.mk test", true},
		{"a clustered attached alternate makefile", npmRoot, "make -sfother.mk test", true},
		{"make -oC is -o with the value C", npmRoot, "make -oC " + goRoot + " test", false},
		{"make -sj4 steers nothing", npmRoot, "make -sj4 test", false},
		// A value-taking letter that ends the cluster owns the next token, so
		// the -C behind it is a value, not a directory.
		{"make -so takes the next token as its value", npmRoot, "make -so -C " + goRoot + " test", false},
		{"make -sW takes the next token as its value", npmRoot, "make -sW -C " + goRoot + " test", false},
		{"make -sj leaves a flag to be read", npmRoot, "make -sj -C " + goRoot + " test", true},
		// make has no lowercase -c or uppercase -F; the guard reads them as -C
		// and -f, as it does standing alone.
		{"a clustered lowercase c is -C", npmRoot, "make -sc " + goRoot + " test", true},
		{"a clustered lowercase c out of the Go tree", goRoot, "make -sc " + npmRoot + " test", false},
		{"a clustered uppercase F is -f", npmRoot, "make -sF other.mk test", true},
		{"a clustered attached uppercase F is -f", npmRoot, "make -sFother.mk test", true},
		// -w (print directory) and -i (ignore errors) take no value, unlike
		// the uppercase -W (what-if file) and -I (include dir): the -C behind
		// a bare -w or -i is still the run directory (gt-i6jw6).
		{"make -w -C into the Go tree", npmRoot, "make -w -C " + goRoot + " test", true},
		{"make -i -C into the Go tree", npmRoot, "make -i -C " + goRoot + " test", true},
		{"make -w -C out of the Go tree", goRoot, "make -w -C " + npmRoot + " test", false},
		// The same letters clustered: valueless -w and -i behind or beside
		// other letters leave the -C that follows, or that ends the cluster,
		// as the run directory.
		{"make -wi -C into the Go tree", npmRoot, "make -wi -C " + goRoot + " test", true},
		{"make -iw -C into the Go tree", npmRoot, "make -iw -C " + goRoot + " test", true},
		{"make -sw -C into the Go tree", npmRoot, "make -sw -C " + goRoot + " test", true},
		{"make -si -C into the Go tree", npmRoot, "make -si -C " + goRoot + " test", true},
		{"make -wi -C out of the Go tree", goRoot, "make -wi -C " + npmRoot + " test", false},
		{"make -swiC into the Go tree", npmRoot, "make -swiC " + goRoot + " test", true},
		{"make -wiC attached into the Go tree", npmRoot, "make -wiC" + goRoot + " test", true},
		{"make -iwC out of the Go tree", goRoot, "make -iwC " + npmRoot + " test", false},
		{"make -wiW takes the next token as its value", npmRoot, "make -wiW -C " + goRoot + " test", false},
		{"make -W file then -C into the Go tree", npmRoot, "make -W file.mk -C " + goRoot + " test", true},
		{"make -W file then -C out of the Go tree", goRoot, "make -W file.mk -C " + npmRoot + " test", false},
		{"make -I dir then -C out of the Go tree", goRoot, "make -I inc -C " + npmRoot + " test", false},
		{"make -W swallows a -C that is its value", goRoot, "make -W -C " + npmRoot + " test", true},
		{"a -C from an unknown cwd", "", "make -C . test", true},
		{"no cwd at all", "", "make test", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertBothGuardsJudge(t, tt.command, tt.cwd, tt.blocked)
		})
	}
}

// -O (--output-sync) takes only an attached value, unlike the -o that takes the
// next word: lowercasing it swallowed the -C behind it, and the suite was judged
// in the cwd instead of the Go tree it ran in.
func TestMakeOutputSyncFlagKeepsTheDirectory(t *testing.T) {
	t.Parallel()
	npmRoot := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		cwd     string
		command string
		blocked bool
	}{
		{"make -O -C into the Go tree", npmRoot, "make -O -C " + goRoot + " test", true},
		{"make -sO -C into the Go tree", npmRoot, "make -sO -C " + goRoot + " test", true},
		{"make -Oline -C into the Go tree", npmRoot, "make -Oline -C " + goRoot + " test", true},
		{"make -O -C out of the Go tree", goRoot, "make -O -C " + npmRoot + " test", false},
		{"make -sO -C out of the Go tree", goRoot, "make -sO -C " + npmRoot + " test", false},
		{"make -o still takes the next token as its value", npmRoot, "make -o -C " + goRoot + " test", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertBothGuardsJudge(t, tt.command, tt.cwd, tt.blocked)
		})
	}
}

// A shell option cluster holding o or O ("-eo", "-euo", "-oe", "-O") takes the
// next word as its value, so the script fed to the shell is still the script:
// the walk used to stop at the value ("pipefail") and judge nothing.
func TestShellFedScriptBehindAnOptionValueIsJudged(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"script piped into bash -eo pipefail", `echo 'make test' | bash -eo pipefail`, true},
		{"script piped into bash -euo pipefail", `echo 'make test' | bash -euo pipefail`, true},
		{"script piped into sh -o pipefail", `echo 'make test' | sh -o pipefail`, true},
		{"script piped into sh +o noglob", `echo 'make test' | sh +o noglob`, true},
		{"here-string into bash -eo pipefail", `bash -eo pipefail <<< 'make test'`, true},
		{"here-string into bash -euo pipefail", `bash -euo pipefail <<<'make test'`, true},
		{"script piped into bash -eo pipefail -s", `echo 'make test' | bash -eo pipefail -s`, true},
		{"harmless script piped into bash -eo pipefail", `echo 'make lint' | bash -eo pipefail`, false},
		{"script file after the option value", `echo 'make test' | bash -eo pipefail ./run.sh`, false},
		{"-c payload after the option value", `bash -eo pipefail -c 'make test'`, true},
		{"script piped into bash -oe pipefail", `echo 'make test' | bash -oe pipefail`, true},
		{"script piped into bash -O extglob", `echo 'make test' | bash -O extglob`, true},
		{"script piped into bash -oo with two values", `echo 'make test' | bash -oo pipefail errexit`, true},
		{"script file after two option values", `echo 'make test' | bash -oo pipefail errexit ./run.sh`, false},
		{"-c payload after a -co cluster's value", `bash -co pipefail 'make test'`, true},
		{"-c payload after a -O value", `bash -O extglob -c 'make test'`, true},
		// A producer that is not echo, printf or yes (a file or a download)
		// feeds a script nothing here can read, so it is not judged.
		{"script file piped into bash", `cat ./run.sh | bash`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A launcher in front of the shell does not hide it: timeout and doas run the
// shell they wrap, so the script fed to that shell is judged the way it is
// with no launcher there. Each row has a mirror without the launcher.
func TestShellFedScriptBehindALauncherIsJudged(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"script piped into sh", `echo 'make test' | sh`, true},
		{"script piped into timeout 5 sh", `echo 'make test' | timeout 5 sh`, true},
		{"script piped into timeout -s KILL 5 bash", `echo 'make test' | timeout -s KILL 5 bash`, true},
		{"script piped into doas -u alice bash", `echo 'make test' | doas -u alice bash`, true},
		{"harmless script piped into timeout 5 sh", `echo 'make lint' | timeout 5 sh`, false},
		{"here-string into timeout 5 bash", `timeout 5 bash <<< 'make test'`, true},
		{"here-string into timeout 5 bash, harmless", `timeout 5 bash <<< 'make lint'`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A redirection word is not an operand: "bash 2>&1" still reads its script from
// stdin, so the script fed to it is judged. A redirection that replaces stdin
// with a file feeds the shell something the guards cannot read (gt-sveq8).
func TestShellFedScriptBehindARedirectionWordIsJudged(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"here-string into bash 2>&1", `bash 2>&1 <<< "make test"`, true},
		{"script piped into bash 2>&1", `echo "make test" | bash 2>&1`, true},
		{"script piped into bash >out", `echo 'make test' | bash >out`, true},
		{"script piped into bash > out", `echo 'make test' | bash > out`, true},
		{"script piped into bash 2> err", `echo 'make test' | bash 2> err`, true},
		{"script piped into bash &>log", `echo 'make test' | bash &>log`, true},
		{"script piped into bash >>log 2>&1", `echo 'make test' | bash >>log 2>&1`, true},
		{"script piped into bash {fd}>log", `echo 'make test' | bash {fd}>log`, true},
		{"script piped into bash 3<in", `echo 'make test' | bash 3<in`, true},
		{"script piped into bash 2>&1 -s", `echo 'make test' | bash 2>&1 -s`, true},
		{"script piped into bash -e 2>&1", `echo 'make test' | bash -e 2>&1`, true},
		{"harmless script piped into bash 2>&1", `echo 'make lint' | bash 2>&1`, false},
		{"script file after a redirection word", `echo 'make test' | bash 2>&1 ./run.sh`, false},
		{"script file after a redirection with a separate target", `echo 'make test' | bash > out ./run.sh`, false},
		{"stdin file replaces the pipe", `echo 'make test' | bash < ./run.sh`, false},
		{"stdin file with 0< replaces the pipe", `echo 'make test' | bash 0<./run.sh`, false},
		{"stdin dup replaces the pipe", `echo 'make test' | bash <&3`, false},
		{"script file piped into bash 2>&1", `cat ./run.sh | bash 2>&1`, false},

		// gt-8rxxy: a redirection ahead of the command hid the command word,
		// so the script it reads went unjudged.
		{"script piped into bash behind a redirection", `2>/dev/null echo "make test" | bash`, true},
		{"here-string ahead of bash", `<<< "make test" bash`, true},
		{"here-string behind a redirection", `2>/dev/null bash <<< "make test"`, true},
		{"eval behind a redirection", `2>/dev/null eval "make test"`, true},
		{"harmless script piped into bash behind a redirection", `2>/dev/null echo "make lint" | bash`, false},
		{"harmless here-string ahead of bash", `<<< "make lint" bash`, false},
		{"harmless eval behind a redirection", `2>/dev/null eval "echo hi"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A redirection shape the walk does not name is read for what it does to the
// shell's stdin instead of passing as a script operand, which stopped the walk
// and judged nothing: a here-string on fd 0 is the script, one on another fd
// feeds that fd, and a process substitution is the script the shell runs
// (gt-hizij).
func TestShellFedScriptBehindOtherRedirectionShapesIsJudged(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"fd-qualified here-string into bash", `bash 0<<< "make test"`, true},
		{"fd-qualified here-string glued to its text", `bash 0<<<'make test'`, true},
		{"fd-qualified here-string after another redirection", `bash 2>&1 0<<<'make test'`, true},
		{"harmless fd-qualified here-string", `bash 0<<<'make lint'`, false},
		{"here-string on fd 3 leaves the pipe as the script", `echo 'make test' | bash 3<<<'echo hi'`, true},
		{"here-string on fd 3 is not the script", `echo 'make lint' | bash 3<<<'make test'`, false},
		{"here-string on a named fd is not the script", `bash {fd}<<<'make test'`, false},
		{"heredoc on fd 3 leaves the pipe as the script", "echo 'make test' | bash 3<<EOF\ntrue\nEOF", true},
		{"heredoc on stdin replaces the pipe", "echo 'make test' | bash <<EOF\ntrue\nEOF", false},
		{"heredoc on fd 0 replaces the pipe", "echo 'make test' | bash 0<<EOF\ntrue\nEOF", false},

		{"process substitution as the script file", `bash <(echo 'make test')`, true},
		{"process substitution as the script file, printf", `bash <(printf 'make test')`, true},
		{"process substitution on stdin", `bash < <(echo 'make test')`, true},
		{"process substitution on fd 0", `bash 0< <(echo 'make test')`, true},
		{"process substitution after a redirection word", `bash 2>&1 <(echo 'make test')`, true},
		{"process substitution that never closes", `bash <(echo 'make test'`, true},
		{"harmless process substitution", `bash <(echo 'make lint')`, false},
		{"process substitution of a file the guards cannot read", `bash <(cat ./run.sh)`, false},
		{"process substitution replaces the pipe", `echo 'make test' | bash <(echo true)`, false},
		{"process substitution on stdin replaces the pipe", `echo 'make test' | bash < <(echo true)`, false},
		{"process substitution is an argument of a script file", `bash ./run.sh <(echo 'make test')`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// The whole-suite goals other than test and build read the same run directory:
// a bare -i before the -C must not hide it (gt-i6jw6).
func TestMakeBareIgnoreErrorsFlagKeepsTheDirectory(t *testing.T) {
	t.Parallel()
	npmRoot := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	for _, command := range []string{
		"make -i -C " + goRoot + " gate",
		"make -w -C " + goRoot + " gate",
		"make -wi -C " + goRoot + " gate",
		"make -siC " + goRoot + " gate",
	} {
		if !makeActsOnWholeGoModule(shellTokenize(command), npmRoot) {
			t.Errorf("makeActsOnWholeGoModule(%q, cwd %s) = false, want true", command, npmRoot)
		}
	}
	if makeActsOnWholeGoModule(shellTokenize("make -i -C "+npmRoot+" gate"), goRoot) {
		t.Errorf("make -i -C <non-Go tree> gate from a Go tree = true, want false")
	}
}

// A walk that cannot finish is not evidence of absence. A directory the
// process cannot resolve part-way up the tree used to read as "no go.mod
// here", which exempted the command — the opposite of what an unresolvable
// tree should do, since it may well be a Go tree the guard is protecting
// (gt-dieu9 review). The refusal stands instead.
func TestMakeTestUnderUnresolvableTreeIsRefused(t *testing.T) {
	t.Parallel()
	// A symlink loop fails the stat of anything under it with ELOOP, which
	// the walk must not read as "no module here". An unreadable directory
	// gives the same shape, but there is no way to make one that also holds
	// for a root user, so the loop is the portable spelling.
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", loop); err != nil {
		t.Fatalf("symlink %s: %v", loop, err)
	}
	nested := filepath.Join(loop, "inner")

	if _, found, err := goModuleRoot(nested); err == nil {
		t.Errorf("goModuleRoot(%s) = found %v, err nil; an unresolvable ancestor must report an error, not an answer", nested, found)
	}
	assertBothGuardsJudge(t, "make test", nested, true)
}

// nonGoTree materialises a rig whose test target is not a Go suite: a temp
// directory holding the Makefile the fractals rig has. The directory has to
// be outside every Go module for the cases built on it to mean anything, and
// t.TempDir sits under TMPDIR, which a host may have pointed inside a
// checkout — a tree the guard would refuse the command in too. Asserting
// against it there would report the host's temp dir as a guard defect, so the
// precondition is checked and the case skipped instead.
func nonGoTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("test:\n\tnpm test\n"), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	if _, found, err := goModuleRoot(root); found || err != nil {
		//testpolicy:allow no-skip — t.TempDir sits under TMPDIR, so a host with TMPDIR inside a checkout has no directory outside a module to build a non-Go tree from; there the case is unbuildable, not failing
		t.Skipf("%s is inside a Go module (found=%v err=%v); a non-Go tree needs a temp dir outside one", root, found, err)
	}
	return root
}

// symlinkedTree returns a path that reaches target through a symlink, so a
// case can put a tree behind a link the walk has to resolve.
func symlinkedTree(t *testing.T, target string) string {
	t.Helper()
	return symlinkAt(t, filepath.Join(t.TempDir(), "link"), target)
}

// symlinkAt links link to target and returns link.
func symlinkAt(t *testing.T, link, target string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(link), err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
	return link
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

// A package argument spelled as a path under the run directory is judged
// there, which is where go resolves it, as well as at the module root, which
// is the reading an explicit argument has always carried: 'go -C internal
// test ./beads/...' runs internal/beads, and the module-root reading of the
// same argument names a package "beads" that go never starts (gt-g6yi7). A
// spelling that is not a filesystem path — a bare import path, which go
// resolves against the module path and refuses from any directory — keeps
// its own reading and does not move with the directory.
func TestContainerSuiteTargetFollowsTheRunDir(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name      string
		relDir    string
		pkgArgs   []string
		wantWhole bool
		want      []string
	}{
		{"relative path under the run dir", "internal", []string{"./beads/..."}, false, []string{"internal/beads"}},
		{"relative subpackage under the run dir", "internal/beads", []string{"./sub"}, false, []string{"internal/beads"}},
		{"relative path up out of the container package", "internal/beads/sub", []string{".."}, false, []string{"internal/beads"}},
		{"relative path out of the run dir, light", "internal", []string{"../internal/style/..."}, false, nil},
		{"relative path out of the module", "internal", []string{"../../elsewhere/..."}, false, nil},
		{"bare import path does not move with the run dir", "internal", []string{"beads"}, false, nil},
		{"module-root spelling keeps its reading beside the resolved one", "internal", []string{"./internal/beads/..."}, false, []string{"internal/beads"}},
		{"whole-repo wildcard under a run dir", "internal", []string{"./..."}, false, containerSuitePackages},
		{"whole-repo wildcard under a light run dir", "internal/style", []string{"./..."}, false, nil},
		{"whole-repo wildcard under a container run dir", "internal/beads", []string{"./..."}, false, []string{"internal/beads"}},
		{"module-prefixed wildcard under a light run dir is still the whole repo", "internal/style", []string{"github.com/steveyegge/gastown/..."}, true, nil},
		{"bare wildcard under a light run dir is still the whole repo", "internal/style", []string{"..."}, true, nil},
		{"wildcard up out of a light run dir", "internal/style", []string{"../..."}, false, containerSuitePackages},
		{"wildcard up to the module root", "internal", []string{"../..."}, true, nil},
		{"relative and bare arguments together", "internal", []string{"beads", "./daemon/..."}, false, []string{"internal/daemon"}},
		{"relative path at the module root", ".", []string{"./internal/beads/..."}, false, []string{"internal/beads"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := filepath.Join(root, filepath.FromSlash(tt.relDir))
			wholeRepo, matched := containerSuiteTarget(tt.pkgArgs, cwd)
			if wholeRepo != tt.wantWhole {
				t.Errorf("containerSuiteTarget(%v, %s) wholeRepo = %v, want %v", tt.pkgArgs, tt.relDir, wholeRepo, tt.wantWhole)
			}
			if strings.Join(matched, ",") != strings.Join(tt.want, ",") {
				t.Errorf("containerSuiteTarget(%v, %s) matched = %v, want %v", tt.pkgArgs, tt.relDir, matched, tt.want)
			}
		})
	}
}

// './...' is the run directory's own subtree, so a run that starts in a light
// package is not a whole-repo run, and one that starts in a container package
// or above it still is refused.
func TestEvaluateContainerSuiteCommand_WildcardFollowsTheRunDir(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/topaz/gastown")
	light := filepath.Join(root, "internal", "style")
	container := filepath.Join(root, "internal", "beads")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"cd into a light package", "cd " + light + " && GT_TEST_DOCKER=1 go test ./...", false},
		{"-C into a light package", "GT_TEST_DOCKER=1 go -C " + light + " test ./...", false},
		{"relative -C into a light package", "GT_TEST_DOCKER=1 go -C internal/style test ./...", false},
		{"cd into a container package", "cd " + container + " && GT_TEST_DOCKER=1 go test ./...", true},
		{"-C into a container package", "GT_TEST_DOCKER=1 go -C " + container + " test ./...", true},
		{"cd into the internal directory", "cd " + filepath.Join(root, "internal") + " && GT_TEST_DOCKER=1 go test ./...", true},
		{"cd into the module root", "cd " + root + " && GT_TEST_DOCKER=1 go test ./...", true},
		{"module-prefixed wildcard from a light package", "cd " + light + " && GT_TEST_DOCKER=1 go test github.com/steveyegge/gastown/...", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, root)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// An absolute directory argument names the package at the module-relative path
// under the module it sits in, whichever worktree that is and whatever the run
// directory: 'go test /work/rig/internal/beads' runs internal/beads, and the
// guard matched nothing for it (gt-d1e8c).
func TestContainerSuiteTargetResolvesAbsoluteDirectories(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	other := fakeModule(t, "clone")
	outside := nonGoTree(t)
	tests := []struct {
		name      string
		runDir    string
		pkgArg    string
		wantWhole bool
		want      []string
	}{
		{"absolute container package", "", root + "/internal/beads", false, []string{"internal/beads"}},
		{"absolute container package with a subtree wildcard", "", root + "/internal/beads/...", false, []string{"internal/beads"}},
		{"absolute container package with a trailing slash", "", root + "/internal/beads/", false, []string{"internal/beads"}},
		{"absolute subpackage of a container package", "", root + "/internal/beads/sub", false, []string{"internal/beads"}},
		{"absolute unclean container path", "", root + "/internal//x/../beads", false, []string{"internal/beads"}},
		{"absolute ancestor of every container package", "", root + "/internal/...", false, containerSuitePackages},
		{"absolute module root subtree", "", root + "/...", true, nil},
		{"absolute module root, repo-root package", "", root, false, nil},
		{"absolute light package", "", root + "/internal/style", false, nil},
		{"absolute light package, subtree", "", root + "/cmd/gt/...", false, nil},
		{"absolute path into another worktree of the module", root, other + "/internal/beads", false, []string{"internal/beads"}},
		{"absolute path from inside the container package", root + "/internal/beads", root + "/internal/cmd", false, []string{"internal/cmd"}},
		{"absolute path outside any module", "", outside + "/internal/beads", false, nil},
		{"absolute path that does not exist", "", root + "/internal/doltserver", false, []string{"internal/doltserver"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wholeRepo, matched := containerSuiteTarget([]string{tt.pkgArg}, tt.runDir)
			if wholeRepo != tt.wantWhole {
				t.Errorf("containerSuiteTarget(%q) wholeRepo = %v, want %v", tt.pkgArg, wholeRepo, tt.wantWhole)
			}
			if strings.Join(matched, ",") != strings.Join(tt.want, ",") {
				t.Errorf("containerSuiteTarget(%q) matched = %v, want %v", tt.pkgArg, matched, tt.want)
			}
		})
	}
}

// The same absolute directories, through the whole guard from the command line.
func TestEvaluateContainerSuiteCommand_AbsoluteDirectory(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/topaz/gastown")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"absolute container package", "GT_TEST_DOCKER=1 go test " + root + "/internal/beads", true},
		{"absolute container package under -C", "GT_TEST_DOCKER=1 go -C /tmp test " + root + "/internal/beads/...", true},
		{"absolute whole-repo subtree", "GT_TEST_DOCKER=1 go test " + root + "/...", true},
		{"absolute light package", "GT_TEST_DOCKER=1 go test " + root + "/internal/style/...", false},
		{"absolute container package, switch off", "go test " + root + "/internal/beads", false},
		{"absolute container package, wrapped", "gt slot run --role gastown/polecats/topaz -- go test " + root + "/internal/beads", false},
		{"absolute wildcard element", "GT_TEST_DOCKER=1 go test " + root + "/internal/b...", true},
		{"absolute glob reaching only a light package", "GT_TEST_DOCKER=1 go test " + root + "/internal/s*", false},
		{"absolute path with a glob above the module root", "GT_TEST_DOCKER=1 go test /nonexistent-d1e8c/*/internal/beads", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, ""), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// A wildcard spelled as a path under the run directory is matched where go
// resolves it as well as at the module root, the way a plain argument is.
func TestPackageWildcardFollowsTheRunDir(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name   string
		relDir string
		arg    string
		want   bool
	}{
		{"glob under internal reaching beads", "internal", "./b*", true},
		{"glob under internal reaching only a light package", "internal", "./s*", false},
		{"ellipsis under internal reaching cmd", "internal", "./c...", true},
		{"glob one level up from a container package", "internal/beads", "../d*", true},
		{"glob at the module root", ".", "./internal/b*", true},
		{"glob under a light directory", "cmd", "./g*", false},
		{"glob under an unlisted subpackage dir still sees the root reading", "cmd", "./internal/b*", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd := filepath.Join(root, filepath.FromSlash(tt.relDir))
			if got := packageWildcardMayReachContainerSuite(tt.arg, cwd); got != tt.want {
				t.Errorf("packageWildcardMayReachContainerSuite(%q, %s) = %v, want %v", tt.arg, tt.relDir, got, tt.want)
			}
		})
	}
}

func TestPackagePatternReaches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern, pkg string
		want         bool
	}{
		{"internal/beads", "internal/beads", true},
		{"internal/b*", "internal/beads", true},
		{"internal/b...", "internal/beads", true},
		{"internal/s*", "internal/beads", false},
		{"internal/[bc]eads", "internal/beads", true},
		{"internal/bead?", "internal/beads", true},
		{"internal/bead??", "internal/beads", false},
		{"inter*", "internal/beads", true},
		{"internal/beads/s*", "internal/beads", true},
		{"internal/.../x", "internal/beads", true},
		{"i...", "internal/beads", true},
		{"x...", "internal/beads", false},
		{"internal/[", "internal/beads", true},
		{"cmd/g*", "internal/beads", false},
	}
	for _, tt := range tests {
		if got := packagePatternReaches(tt.pattern, tt.pkg); got != tt.want {
			t.Errorf("packagePatternReaches(%q, %q) = %v, want %v", tt.pattern, tt.pkg, got, tt.want)
		}
	}
}

func TestNormalizeGoPackageArg(t *testing.T) {
	t.Parallel()
	tests := []struct{ arg, want string }{
		{"./internal/beads/...", "internal/beads"},
		{"internal/beads/", "internal/beads"},
		{"internal/beads", "internal/beads"},
		{"github.com/steveyegge/gastown/internal/beads", "internal/beads"},
		{"./internal//beads", "internal/beads"},
		{"./internal/./beads", "internal/beads"},
		{"./internal/x/../beads", "internal/beads"},
		{"./internal//beads/.../", "internal/beads"},
		{"github.com/steveyegge/gastown//internal/beads", "internal/beads"},
		{"github.com/steveyegge/gastown/...", wholeRepoPackageArg},
		{"./internal/../...", wholeRepoPackageArg},
		{"./...", wholeRepoPackageArg},
		{"...", wholeRepoPackageArg},
		{"", cwdPackageArg},
		{".", cwdPackageArg},
		{"./", cwdPackageArg},
		{"./internal/..", cwdPackageArg},
		{"../beads", "../beads"},
	}
	for _, tt := range tests {
		if got := normalizeGoPackageArg(tt.arg); got != tt.want {
			t.Errorf("normalizeGoPackageArg(%q) = %q, want %q", tt.arg, got, tt.want)
		}
	}
}

func TestPackageArgWildcard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arg  string
		want bool
	}{
		{"./...", false},
		{"...", false},
		{"./internal/beads/...", false},
		{"./internal/beads/.../", false},
		{"github.com/steveyegge/gastown/...", false},
		{"../...", false},
		{"/abs/internal/beads/...", false},
		{"./internal/beads", false},
		{"./internal/../beads", false},
		{"./internal/b...", true},
		{"./internal/.../beads", true},
		{"./.../beads", true},
		{"./internal/b.../...", true},
		{"./internal/b*", true},
		{"./internal/bead?", true},
		{"./internal/[b]eads", true},
		{"./internal/{beads,style}", true},
		{"/abs/internal/b...", true},
		{"./internal/$(echo beads)", true},
		{"./internal/$P", true},
		{"$PWD/internal/beads", true},
		{"./internal/`echo beads`", true},
		{"~/x/internal/beads", true},
		{"~", true},
		{"./internal/~beads", false},
	}
	for _, tt := range tests {
		if got := packageArgWildcard(tt.arg); got != tt.want {
			t.Errorf("packageArgWildcard(%q) = %v, want %v", tt.arg, got, tt.want)
		}
	}
}

// cwdContainerPackages is the cwd form of the container rule: the cwd's
// package is judged with packageArgCovers, the same rule an argument gets, so
// the set a "go test ." from that directory reaches is the set "go test
// ./<that dir>" reaches.
func TestCwdContainerPackages(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
// another module or in no module at all cannot be one. A nested module (one
// under plugins/, say) ends the walk for the same reason.
func TestCwdPackagePath_OutsideModule(t *testing.T) {
	t.Parallel()
	t.Run("no go.mod anywhere", func(t *testing.T) {
		if pkg, ok := cwdPackagePath(t.TempDir()); ok {
			t.Errorf("cwdPackagePath(%s) = (%q, true), want ok false outside any module", t.TempDir(), pkg)
		}
	})

	t.Run("another module", func(t *testing.T) {
		root := fakeModule(t, "clone")
		nested := filepath.Join(root, "plugins", "nested", "internal")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(nested), "go.mod"),
			[]byte("module "+gastownModulePath+"/plugins/nested\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if pkg, ok := cwdPackagePath(nested); ok {
			t.Errorf("cwdPackagePath(%s) = (%q, true), want ok false: the nearest go.mod declares another module", nested, pkg)
		}
	})
}

// The cwd form of the container rule, end to end from the invocation's cwd: the same
// package reached as "go test ." must be judged exactly as "go test
// ./internal/beads" is. The polecat cwd is the one attempt 2's guard let
// through (gt-1lko).
func TestEvaluateContainerSuiteCommand_CwdStyle(t *testing.T) {
	t.Parallel()
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

		// A package argument spelled as a path under the directory the
		// invocation runs in names the package go runs there, not a
		// module-root one: both of these runs test internal/beads, and the
		// module-root reading of "./beads/..." names a package "beads" that
		// go never starts, so they used to get through unwrapped (gt-g6yi7).
		{"relative package arg under the run dir", "internal", "GT_TEST_DOCKER=1 go test ./beads/...", true},
		{"relative package arg after a cd", ".", "cd internal && GT_TEST_DOCKER=1 go test ./beads/...", true},

		// The module-root reading stands beside the resolved one, so the run
		// that spells a relative path out of reach — this one would test
		// internal/internal/beads, which is nothing — is refused as it always
		// was, and the rule only ever blocks more.
		{"relative package arg spelled for the module root", ".", "cd internal && GT_TEST_DOCKER=1 go test ./internal/beads/...", true},
	}
	for _, layout := range worktreeLayouts {
		t.Run(layout, func(t *testing.T) {
			root := fakeModule(t, layout)
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					cwd := filepath.Join(root, filepath.FromSlash(tt.relDir))
					reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, cwd), tt.command, "")
					if got := reason != ""; got != tt.blocked {
						t.Errorf("evaluateContainerSuiteCommand(%q) from %s blocked = %v (reason %q), want %v",
							tt.command, tt.relDir, got, reason, tt.blocked)
					}
				})
			}
		})
	}
}

// A bare or "." target is judged in the directory the invocation's own -C dir
// flag names, not in the one the hook runs in: Go chdirs on -C before it runs
// the command, so the shell's cwd has no say in what the target tests
// (gt-tzdpe). An explicit package argument carries its own path and keeps its
// reading, and one spelled as a path under that -C directory ("./beads/...")
// is judged where it lands as well (gt-g6yi7). The polecat scope rule reads the
// bare and "." forms the same way, pinned in
// TestEvaluatePolecatTestScope_TargetFollowsTheCDirFlag.
func TestContainerSuiteGoTestTargetFollowsTheCDirFlag(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	container := filepath.Join(root, "internal", "beads")
	light := filepath.Join(root, "internal", "style")
	containerLink := symlinkedTree(t, container)

	tests := []struct {
		name    string
		cwd     string
		command string
		blocked bool
	}{
		{"-C into the container package", light, "GT_TEST_DOCKER=1 go -C " + container + " test .", true},
		{"-C= into the container package", light, "GT_TEST_DOCKER=1 go -C=" + container + " test ./", true},
		{"-C into the container package, bare target", light, "GT_TEST_DOCKER=1 go -C " + container + " test", true},
		{"versioned binary with a -C dir", light, "GT_TEST_DOCKER=1 go1.22.0 -C " + container + " test .", true},
		{"-C through a link into the container package", light, "GT_TEST_DOCKER=1 go -C " + containerLink + " test .", true},
		{"a relative -C composed onto an absolute one", light, "GT_TEST_DOCKER=1 go -C " + root + " -C internal/beads test .", true},

		// The reverse reading: a -C dir the hook is not in stops the hook's
		// own directory from refusing the target, and the switch still
		// decides whether any of it can reach Docker.
		{"-C out of the container package", container, "GT_TEST_DOCKER=1 go -C " + light + " test .", false},
		{"-C out of the container package, bare target", container, "GT_TEST_DOCKER=1 go -C " + light + " test", false},
		{"-C to the module root", container, "GT_TEST_DOCKER=1 go -C " + root + " test .", false},
		{"-C away with the switch off", light, "go -C " + container + " test .", false},
		{"explicit container argument read whatever -C names", light, "GT_TEST_DOCKER=1 go -C " + light + " test ./internal/beads/...", true},

		// A package argument spelled as a path under the -C directory is
		// judged there, which is where go resolves it: this run tests
		// internal/beads, and the module-root reading of "./beads/..." names
		// a package go never starts (gt-g6yi7).
		{"-C dir flag with a relative package argument", root, "GT_TEST_DOCKER=1 go -C internal test ./beads/...", true},
		{"relative argument out of the -C directory", root, "GT_TEST_DOCKER=1 go -C internal test ../internal/style/...", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, tt.cwd), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) from %s blocked = %v (reason %q), want %v",
					tt.command, tt.cwd, got, reason, tt.blocked)
			}
		})
	}
}

// A directory the walk cannot place (cd "$UNSET", cd into a command
// substitution) leaves a package argument spelled as a path under it
// ("./beads/...") meaning any package in any tree, but it normalizes to the
// module-root package "beads", which is on no list, so the run went through
// unwrapped. Only a "." or bare target was refused (gt-ff2l8). An import path
// is module-absolute and keeps its own reading.
func TestContainerSuiteGuardRelativeArgUnderUnplacedCwd(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"relative container argument", `cd "$UNSET" && GT_TEST_DOCKER=1 go test ./beads/...`, true},
		{"relative container argument, no dots", `cd "$UNSET" && GT_TEST_DOCKER=1 go test ./beads`, true},
		{"parent-relative argument", `cd "$UNSET" && GT_TEST_DOCKER=1 go test ../beads`, true},
		{"relative argument after a command substitution", `cd "$(git rev-parse --show-toplevel)/internal" && GT_TEST_DOCKER=1 go test ./beads`, true},
		{"dot target", `cd "$UNSET" && GT_TEST_DOCKER=1 go test .`, true},

		{"switch off", `cd "$UNSET" && go test ./beads/...`, false},
		{"wrapped in slot run", `cd "$UNSET" && gt slot run --role gastown/refinery -- GT_TEST_DOCKER=1 go test ./beads/...`, false},
		{"import-path light package", `cd "$UNSET" && GT_TEST_DOCKER=1 go test internal/style`, false},
		{"relative argument that reaches no listed package", `cd "$UNSET" && GT_TEST_DOCKER=1 go test ./style`, false},
		{"module-rooted relative light argument", `cd "$UNSET" && GT_TEST_DOCKER=1 go test ./internal/style`, false},
		{"relative argument after a literal cd", `cd internal && GT_TEST_DOCKER=1 go test ./style`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, matched := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// A -C directory the shell expands ("$PWD/internal", "~/rig", "$(pwd)") is a
// run directory the hook cannot place, the way `cd $DIR` is: it was joined onto
// the cwd as a literal and read as a placed, nonexistent directory, so
// 'go -C "$PWD/internal" test ./beads' passed although go runs internal/beads
// (gt-8a1pa). A literal -C keeps the verdicts above.
func TestContainerSuiteGuardShellExpandedCDir(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"variable, package under the directory", `GT_TEST_DOCKER=1 go -C "$PWD/internal" test ./beads`, true},
		{"variable, -C= spelling", `GT_TEST_DOCKER=1 go -C="$PWD/internal" test ./beads`, true},
		{"variable, bare target", `GT_TEST_DOCKER=1 go -C "$PWD/internal/beads" test`, true},
		{"variable, dot target", `GT_TEST_DOCKER=1 go -C "$PWD/internal/beads" test .`, true},
		{"leading tilde", "GT_TEST_DOCKER=1 go -C ~/gt/x/internal test ./beads", true},
		{"command substitution", `GT_TEST_DOCKER=1 go -C "$(pwd)/internal" test ./beads`, true},
		{"command substitution, -C= spelling", `GT_TEST_DOCKER=1 go -C="$(pwd)/internal" test ./beads`, true},
		{"backtick substitution", "GT_TEST_DOCKER=1 go -C `pwd`/internal test ./beads", true},
		{"expanded directory after a literal one", `GT_TEST_DOCKER=1 go -C internal -C "$SUB" test ./beads`, true},
		{"literal directory, same package", "GT_TEST_DOCKER=1 go -C internal test ./beads", true},

		// The switch decides first, and an argument that is an import path
		// carries its own location whatever the directory is.
		{"switch off", `go -C "$PWD/internal" test ./beads`, false},
		{"module-prefixed light package", `GT_TEST_DOCKER=1 go -C "$PWD/internal" test github.com/steveyegge/gastown/internal/style`, false},
		{"literal directory, light package", "GT_TEST_DOCKER=1 go -C internal test ./style", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// cwdHeavyPackages is the polecat scope rule's cwd form: the same set a "go
// test ./<that dir>" names, so cd-ing into the package is not a way around
// the rule.
func TestCwdHeavyPackages(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name   string
		relDir string
		want   []string
	}{
		{"module root", ".", nil},
		{"heavy package", "internal/cmd", []string{"internal/cmd"}},
		{"subpackage of a heavy package", "internal/cmd/sub", []string{"internal/cmd"}},
		{"ancestor of every heavy package", "internal", []string{"internal/cmd", "internal/daemon", "internal/polecat"}},
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
	t.Parallel()
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
					cwd := filepath.Join(root, filepath.FromSlash(tt.relDir))
					reason, matched := evaluatePolecatTestScope(fakeGuardProcess(nil, cwd), tt.command)
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
	t.Parallel()
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
	t.Parallel()
	root := fakeModule(t, "gastown/refinery/rig")
	for _, relDir := range []string{"internal", "internal/beads", "internal/beads/sub", "internal/cmd", "internal/cmd/sub", "internal/style", "cmd/gt", "."} {
		t.Run(relDir, func(t *testing.T) {
			cwd := filepath.Join(root, filepath.FromSlash(relDir))
			proc := fakeGuardProcess(nil, cwd)

			argForm, _ := evaluateContainerSuiteCommand(proc, "GT_TEST_DOCKER=1 go test ./"+relDir, "")
			cwdForm, _ := evaluateContainerSuiteCommand(proc, "GT_TEST_DOCKER=1 go test .", "")
			if (argForm != "") != (cwdForm != "") {
				t.Errorf("container guard: ./%s blocked by argument form = %v (reason %q), by cwd form = %v (reason %q)",
					relDir, argForm != "", argForm, cwdForm != "", cwdForm)
			}

			argReason, _ := evaluatePolecatTestScope(proc, "go test ./"+relDir)
			scopeReason, _ := evaluatePolecatTestScope(proc, "go test .")
			if (argReason != "") != (scopeReason != "") {
				t.Errorf("polecat scope rule: ./%s blocked by argument form = %v, by cwd form = %v", relDir, argReason != "", scopeReason != "")
			}
		})
	}
}

// TestIsPolecatOrRefineryContext_CwdFallback pins the cwd-path fallback
// itself (same shape as isGasTownAgentContext's "/polecats/" check): a
// polecat worktree cwd is enough even with no env vars set, since a hook's
// ambient environment can't always be relied on to carry GT_POLECAT.
func TestIsPolecatOrRefineryContext_CwdFallback(t *testing.T) {
	t.Parallel()
	if isPolecatOrRefineryContext(fakeGuardProcess(nil, "/tmp/neutral")) {
		t.Fatal("expected neutral tmp dir cwd to not trigger the polecat-path fallback")
	}

	if !isPolecatOrRefineryContext(fakeGuardProcess(nil, "/tmp/gastown/polecats/topaz")) {
		t.Error("expected a cwd under /polecats/ to be treated as polecat context even with no env vars set")
	}
}

// TestRunTapGuardContainerSuite_BlockedInPolecatContext is the "genuine
// positive still caught" leg of the adversarial-test-criterion: the guard
// must actually exit non-nil (blocking) end-to-end, not just when its
// internal evaluator is called directly.
func TestRunTapGuardContainerSuite_BlockedInPolecatContext(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "topaz",
		"GT_ROLE":    "gastown/polecats/topaz",
	}, "")
	hookInput := `{"tool_name":"Bash","tool_input":{"command":"GT_TEST_DOCKER=1 go test ./internal/beads/..."}}`
	err := tapGuardContainerSuite(strings.NewReader(hookInput), io.Discard, proc)
	if err == nil {
		t.Error("expected bare go test on a container-backed package with the switch on to be blocked for a polecat, got nil error")
	}
}

// TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext is the
// "false positive gone" leg: the same command that gets blocked for a
// polecat must be allowed for a role this guard doesn't cover (e.g. crew).
func TestRunTapGuardContainerSuite_AllowedOutsidePolecatOrRefineryContext(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_ROLE": "gastown/crew",
	}, "")
	hookInput := `{"tool_name":"Bash","tool_input":{"command":"go test ./internal/beads/..."}}`
	err := tapGuardContainerSuite(strings.NewReader(hookInput), io.Discard, proc)
	if err != nil {
		t.Errorf("expected non-polecat/refinery context to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardContainerSuite_WrappedAllowed is the "wrapped" leg: the
// exact same target package, wrapped in gt slot run, must be allowed even
// under a polecat context.
func TestRunTapGuardContainerSuite_WrappedAllowed(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "topaz",
		"GT_ROLE":    "gastown/polecats/topaz",
	}, "")
	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gt slot run --role gastown/polecats/topaz -- go test ./internal/beads/..."}}`
	err := tapGuardContainerSuite(strings.NewReader(hookInput), io.Discard, proc)
	if err != nil {
		t.Errorf("expected gt-slot-run-wrapped command to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardContainerSuite_NonContainerPackageAllowed is the "novel/
// unaffected input still passes" leg: a polecat running tests scoped to a
// package with no Docker footprint must not be blocked.
func TestRunTapGuardContainerSuite_NonContainerPackageAllowed(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "topaz",
		"GT_ROLE":    "gastown/polecats/topaz",
	}, "")
	hookInput := `{"tool_name":"Bash","tool_input":{"command":"go test ./internal/style/..."}}`
	err := tapGuardContainerSuite(strings.NewReader(hookInput), io.Discard, proc)
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
	t.Parallel()
	proc := fakeGuardProcess(nil, "")
	if reason, _ := evaluateContainerSuiteCommand(proc, "go test ./internal/beads/...", "1"); reason == "" {
		t.Error("bare go test on a container package with the opt-in exported in the environment was allowed")
	}
	if reason, _ := evaluateContainerSuiteCommand(proc, "go test ./internal/beads/...", ""); reason != "" {
		t.Errorf("bare go test with the opt-in unset was blocked: %s", reason)
	}
}

// TestContainerSuiteWrapRole pins the two readings of the --role argument: the
// caller's own role when the hook carries one (so the printed line is
// runnable verbatim), and the formula's placeholder when it does not.
func TestContainerSuiteWrapRole(t *testing.T) {
	t.Parallel()
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
			if got := containerSuiteWrapRole(tt.gtRole); got != tt.want {
				t.Errorf("containerSuiteWrapRole() with GT_ROLE=%q = %q, want %q", tt.gtRole, got, tt.want)
			}
		})
	}
}

// The refusal a polecat actually receives must name every sanctioned path, not
// just the hardest one (overseer hq-wisp-32rsm). This is driven through the
// real hook entry point and asserts on the PreToolUse stderr rather than on
// the Go source, because the banner text is the whole remediation: six of six
// local polecats hit this refusal on 2026-09-17 and improvised around it —
// granite's spiral to a direct main push started here.
func TestRunTapGuardContainerSuite_RefusalNamesTheSanctionedPaths(t *testing.T) {
	t.Parallel()
	// A neutral cwd: the guard also reads the working directory, and a
	// polecat worktree's path contains "/polecats/" (gt-3008).
	proc := fakeGuardProcess(map[string]string{
		dockerTestsEnv: "1",
		"GT_POLECAT":   "granite",
		"GT_ROLE":      "gastown/polecats/granite",
	}, t.TempDir())

	command := "go test ./internal/beads/..."
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
	var buf strings.Builder
	err := tapGuardContainerSuite(strings.NewReader(input), &buf, proc)
	stderr := buf.String()
	if err == nil {
		t.Fatalf("bare container-package run was allowed through the hook: %s", stderr)
	}
	for _, want := range []string{
		"CONTAINER-SUITE COMMAND BLOCKED",
		"Run it wrapped instead: gt slot run --role gastown/polecats/granite -- go test ./internal/beads/...",
		"run the non-container packages directly",
		"let `gt done` gate the",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal is missing %q:\n%s", want, stderr)
		}
	}
}

// A heredoc body is data unless a shell invoker reads it (gt-ohe8n): a bead
// description, a doc or a formula that merely spells a suite invocation is
// prose, while "bash <<EOF" and "cat <<EOF | bash" feed the body to a shell,
// so that body is code and stays in scope.
func TestContainerSuiteGuardsStripHeredocBodies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		command      string
		wantRefused  bool
		wantScopeOut bool
	}{
		{
			name:        "bead description quoting make test is data",
			command:     "cat > /tmp/bead.md <<'EOF'\nreproduce with make test\nEOF\n",
			wantRefused: false, wantScopeOut: false,
		},
		{
			name:        "doc quoting the suite invocation is data",
			command:     "cat > /tmp/notes.md <<'EOF'\nGT_TEST_DOCKER=1 go test ./internal/beads/...\nEOF\n",
			wantRefused: false, wantScopeOut: false,
		},
		{
			name:        "whole-repo and make test in one body are data",
			command:     "cat > /tmp/notes.md <<'EOF'\nmake test\ngo test ./...\nEOF\n",
			wantRefused: false, wantScopeOut: false,
		},
		{
			name:        "live invocation after the terminator still refused",
			command:     "cat > /tmp/notes.md <<'EOF'\nmake test\nEOF\nmake test\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "shell-fed body is a script, not data",
			command:     "bash <<'EOF'\nmake test\nEOF\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "body piped into a shell is a script",
			command:     "cat <<'EOF' | bash\nmake test\nEOF\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "shell-fed body inside an unquoted substitution is a script",
			command:     "x=$(bash <<'EOF'\nmake test\nEOF\n)\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "shell-fed body may itself carry a heredoc",
			command:     "bash <<'OUTER'\nbash <<'INNER'\nmake test\nINNER\nOUTER\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "shell-fed container run is refused",
			command:     "bash <<'EOF'\nGT_TEST_DOCKER=1 go test ./internal/beads/...\nEOF\n",
			wantRefused: true, wantScopeOut: false,
		},
		// The body is data even inside a double-quoted substitution, where its
		// apostrophe used to open a quote that ran past the ")" (gt-lx8i8).
		{
			name:        "container run in a body inside a double-quoted substitution is data",
			command:     "x=\"$(cat <<'EOF'\nit's GT_TEST_DOCKER=1 go test ./internal/beads/...\nEOF\n)\"",
			wantRefused: false, wantScopeOut: false,
		},
		{
			name:        "shell-fed container run inside a double-quoted substitution is refused",
			command:     "x=\"$(bash <<'EOF'\nGT_TEST_DOCKER=1 go test ./internal/beads/...\nEOF\n)\"",
			wantRefused: true, wantScopeOut: false,
		},
		{
			name:        "live container run after a body inside a double-quoted substitution is refused",
			command:     "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"\nGT_TEST_DOCKER=1 go test ./internal/beads/...",
			wantRefused: true, wantScopeOut: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, ""), tt.command, "")
			if got := reason != ""; got != tt.wantRefused {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.wantRefused)
			}
			scopeReason, _ := evaluatePolecatTestScope(fakeGuardProcess(nil, ""), tt.command)
			if got := scopeReason != ""; got != tt.wantScopeOut {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q), want %v", tt.command, got, scopeReason, tt.wantScopeOut)
			}
		})
	}
}

// A shell-fed heredoc body is a script the reader shell runs, so it inherits
// the container opt-in the reading line carries, as a -c payload and a command
// substitution do (gt-zyccn). The three cases whose opt-in sits ahead of the
// body pass no hook-environment opt-in, so they reach it only through the
// line's own GT_TEST_DOCKER=1, typed before the reader word or exported by a
// segment ahead of it. A body naming no container-backed package stays allowed.
func TestContainerSuiteHeredocBodyHonoursDockerOptIn(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/nux/gastown")
	tests := []struct {
		name     string
		command  string
		envOptIn string
		blocked  bool
	}{
		{
			name:    "opt-in on the reader line reaches the body",
			command: "GT_TEST_DOCKER=1 bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			blocked: true,
		},
		{
			name:    "exported opt-in on the reader line reaches the body",
			command: "export GT_TEST_DOCKER=1; bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			blocked: true,
		},
		{
			name:    "opt-in exported two segments before the reader reaches the body",
			command: "export GT_TEST_DOCKER=1; echo ready; bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			blocked: true,
		},
		{
			name:    "opt-in in the body's own text",
			command: "bash <<'EOF'\nGT_TEST_DOCKER=1 go test ./internal/beads\nEOF\n",
			blocked: true,
		},
		{
			name:     "hook environment opt-in reaches the body",
			command:  "bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			envOptIn: "1",
			blocked:  true,
		},
		{
			name:    "no opt-in anywhere",
			command: "bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			blocked: false,
		},
		{
			name:    "opt-in but the body names no container-backed package",
			command: "GT_TEST_DOCKER=1 bash <<'EOF'\ngo test ./internal/style\nEOF\n",
			blocked: false,
		},
		{
			name:    "slot-wrapped body with no opt-in",
			command: "gt slot run --role gastown/nux -- bash <<'EOF'\ngo test ./internal/beads\nEOF\n",
			blocked: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, tt.envOptIn)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q, envOptIn %q) blocked = %v (reason %q, matched %v), want %v", tt.command, tt.envOptIn, got, reason, matched, tt.blocked)
			}
			if !tt.blocked {
				return
			}
			// Every blocked case here names a container-backed package, so the
			// verdict must be that package rule, not some neighbouring refusal.
			if !strings.Contains(reason, "targets testcontainers-backed package(s)") {
				t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want the testcontainers-backed package verdict", tt.command, reason)
			}
		})
	}
}

// Shell-fed heredocs are followed to a bounded depth, the same bound and the
// same shape matchesPRWorkflowCommand places on the identical recursion:
// maxTestGuardNestDepth levels are inspected, so an invocation three shells
// deep is still refused and a deeper tower terminates instead of recursing
// without end.
func TestContainerSuiteGuardsBoundHeredocRecursion(t *testing.T) {
	t.Parallel()
	command := "bash <<'L0'\nbash <<'L1'\nbash <<'L2'\nmake test\nL2\nL1\nL0\n"
	proc := fakeGuardProcess(nil, "")
	if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); reason == "" {
		t.Errorf("make test nested %d shell-fed heredocs deep was allowed", maxTestGuardNestDepth)
	}
	if reason, _ := evaluatePolecatTestScope(proc, command); reason == "" {
		t.Errorf("scope: make test nested %d shell-fed heredocs deep was allowed", maxTestGuardNestDepth)
	}
}

// An unquoted heredoc body is scanned for the substitutions the shell expands
// while reading it, so the body of a backtick span there is judged the way the
// shell parses it — with the backslash that escapes a nested backtick pair
// dropped. Cut raw, the body handed to the evaluator held "\`", which opens no
// substitution, so "echo \`make test\`" written inside a heredoc ran make test
// with nothing refusing it, while the same text on a command line
// (substitutionScripts) was refused (gt-h3312). The pair below pins that the
// unquoted body is judged and the quoted one stays data.
func TestContainerSuiteHeredocEscapedNestedBacktickIsJudged(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "gastown/refinery/rig")
	proc := fakeGuardProcess(nil, goTree)
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{
			name:    "escaped nested backtick in an unquoted body",
			command: "cat > f <<EOF\necho `echo \\`make test\\``\nEOF\n",
			blocked: true,
		},
		{
			name:    "the same body quoted stays data",
			command: "cat > f <<'EOF'\necho `echo \\`make test\\``\nEOF\n",
			blocked: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateContainerSuiteCommand(proc, tt.command, ""); (reason != "") != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
			if reason, _ := evaluatePolecatTestScope(proc, tt.command); (reason != "") != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
		})
	}
}

// A shell-fed heredoc's body runs in the directory its reader line left the
// shell in, so the cd on that line is not a way around the guards: a polecat
// in a non-Go rig cannot cd into a Go tree and feed the suite to bash, which
// gt-5mc21's per-segment walk does not reach (the body is not a segment of the
// reader line) and gt-1cvqj fixes. The neighbours pin that the directory — not
// the heredoc form — decides, and that the directory is the reader line's own:
// a cd after the body has been read does not move it, and a body whose reader
// line stays in the non-Go tree is still allowed.
func TestHeredocBodyIsJudgedInReaderLineCwd(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "gastown/refinery/rig")
	heavyPkg := filepath.Join(goTree, "internal", "cmd")
	containers := filepath.Join(goTree, "internal", "beads")
	nonGo := nonGoTree(t)
	tests := []struct {
		name         string
		cwd          string
		command      string
		wantRefused  bool
		wantScopeOut bool
	}{
		{
			name:         "cd into the Go tree then feed make test to bash",
			cwd:          nonGo,
			command:      "cd " + goTree + " && bash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			name:         "the same form whose reader line stays in the non-Go tree",
			cwd:          nonGo,
			command:      "bash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "a cd out of the Go tree is judged in the non-Go tree",
			cwd:          goTree,
			command:      "cd " + nonGo + " && bash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "a cd after the body is read does not move it",
			cwd:          nonGo,
			command:      "bash <<'EOF' && cd " + goTree + "\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "cd into a heavy package then feed the cwd form",
			cwd:          nonGo,
			command:      "cd " + heavyPkg + " && bash <<'EOF'\ngo test .\nEOF\n",
			wantRefused:  false,
			wantScopeOut: true,
		},
		{
			name:         "cd into a container package then feed the cwd form",
			cwd:          nonGo,
			command:      "cd " + containers + " && bash <<'EOF'\nGT_TEST_DOCKER=1 go test .\nEOF\n",
			wantRefused:  true,
			wantScopeOut: false,
		},
		{
			name:         "the reader line's cd carries into a nested body",
			cwd:          nonGo,
			command:      "cd " + goTree + " && bash <<'OUTER'\nbash <<'INNER'\nmake test\nINNER\nOUTER\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			// A cd the walk cannot place is carried into the body as an
			// unplaced directory, so the body is judged as unplaced rather
			// than read against the invocation's own — the handoff the -c
			// payload and the substitutions inside an unquoted body already
			// get (gt-jj3or, heredocSubstitutionScripts). An unplaceable
			// directory is no more evidence that the body runs in a non-Go
			// tree than that it runs in a Go one, the reading gt-ofj05 gives
			// every other directory the walk cannot place.
			name:         "a cd the guard cannot place leaves the body unplaced",
			cwd:          nonGo,
			command:      "cd \"$BUILD_DIR\" && bash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, tt.cwd)
			reason, _ := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.wantRefused {
				t.Errorf("evaluateContainerSuiteCommand(%q) from %s blocked = %v (reason %q), want %v",
					tt.command, tt.cwd, got, reason, tt.wantRefused)
			}
			scopeReason, _ := evaluatePolecatTestScope(proc, tt.command)
			if got := scopeReason != ""; got != tt.wantScopeOut {
				t.Errorf("evaluatePolecatTestScope(%q) from %s blocked = %v (reason %q), want %v",
					tt.command, tt.cwd, got, scopeReason, tt.wantScopeOut)
			}
		})
	}
}

// The shell keeps one working directory from one command to the next, so a cd
// on its own line before the heredoc's reader line still decides the directory
// the body runs in. gt-1cvqj read only the reader line, which left "cd <Go
// tree>" + newline + "bash <<EOF" open from a non-Go rig, and the mirror image
// — a cd out of a Go tree on an earlier line — open in the other direction
// (gt-v02wh). The preceding text is read with its heredoc bodies stripped, so
// the neighbours below pin that a cd the shell never runs stays data and that
// an earlier cd the walk cannot place leaves the body unplaced (gt-jj3or).
func TestHeredocBodyIsJudgedInDirectoryOfPrecedingLines(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "gastown/refinery/rig")
	heavyPkg := filepath.Join(goTree, "internal", "cmd")
	nonGo := nonGoTree(t)
	tests := []struct {
		name         string
		cwd          string
		command      string
		wantRefused  bool
		wantScopeOut bool
	}{
		{
			name:         "cd into the Go tree on the line before the reader line",
			cwd:          nonGo,
			command:      "cd " + goTree + "\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			name:         "earlier lines that stay in the non-Go tree are allowed",
			cwd:          nonGo,
			command:      "echo starting\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "a cd out of the Go tree on an earlier line",
			cwd:          goTree,
			command:      "cd " + nonGo + "\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "an earlier cd into a heavy package feeds the cwd form",
			cwd:          nonGo,
			command:      "cd " + heavyPkg + "\nbash <<'EOF'\ngo test .\nEOF\n",
			wantRefused:  false,
			wantScopeOut: true,
		},
		{
			// A cd after the body has been read must not undo the earlier one:
			// the walk still stops at the invoker's segment.
			name:         "a cd after the invoker does not undo an earlier one",
			cwd:          nonGo,
			command:      "cd " + goTree + "\nbash <<'EOF' && cd " + nonGo + "\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			// The bodies are stripped from the preceding text too, so a cd the
			// shell never runs — one written inside a data body — decides
			// nothing.
			name:         "a cd inside a data body on an earlier line does not leak",
			cwd:          nonGo,
			command:      "cat > /tmp/notes.md <<'DATA'\ncd " + goTree + "\nDATA\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
		},
		{
			name:         "an earlier variable assignment feeds the cd the walk reads",
			cwd:          nonGo,
			command:      "BUILD_DIR=" + goTree + "\ncd $BUILD_DIR\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			// The same handoff reached through the preceding text: an
			// unplaceable directory is evidence of neither a Go tree nor a
			// non-Go one, so the body is judged as unplaced (gt-jj3or).
			name:         "an earlier cd the guard cannot place leaves the body unplaced",
			cwd:          nonGo,
			command:      "cd \"$BUILD_DIR\"\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			name:         "an earlier cd carries into a nested body",
			cwd:          nonGo,
			command:      "cd " + goTree + "\nbash <<'OUTER'\nbash <<'INNER'\nmake test\nINNER\nOUTER\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			// The reader line's own cd is reached through the continuation
			// walk-back, not through the preceding text, so it must not be
			// counted twice or dropped.
			name:         "a cd joined to the reader line by a continuation",
			cwd:          nonGo,
			command:      "cd " + goTree + " && \\\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
		{
			name:         "an earlier cd carries into a body piped to a shell",
			cwd:          nonGo,
			command:      "cd " + goTree + "\ncat <<'EOF' | bash\nmake test\nEOF\n",
			wantRefused:  true,
			wantScopeOut: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, tt.cwd)
			reason, _ := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.wantRefused {
				t.Errorf("evaluateContainerSuiteCommand(%q) from %s blocked = %v (reason %q), want %v",
					tt.command, tt.cwd, got, reason, tt.wantRefused)
			}
			scopeReason, _ := evaluatePolecatTestScope(proc, tt.command)
			if got := scopeReason != ""; got != tt.wantScopeOut {
				t.Errorf("evaluatePolecatTestScope(%q) from %s blocked = %v (reason %q), want %v",
					tt.command, tt.cwd, got, scopeReason, tt.wantScopeOut)
			}
		})
	}
}

// The acceptance case of gt-v02wh through the hook: the two-line form is
// refused by both rules from a non-Go rig, and the same form whose earlier
// lines stay in the non-Go tree is allowed.
func TestRunTapGuardContainerSuite_HeredocEarlierLineCdRefused(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "gastown/refinery/rig")
	nonGo := nonGoTree(t)
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "rictus",
		"GT_ROLE":    "gastown/polecats/rictus",
	}, nonGo)

	refused := "cd " + goTree + "\nbash <<'EOF'\nmake test\nEOF\n"
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(refused) + `}}`
	stderr, err := runContainerSuiteGuard(input, proc)
	if err == nil {
		t.Errorf("a shell-fed make test behind a cd on the previous line was allowed")
	} else if !strings.Contains(stderr, "TEST SCOPE") || !strings.Contains(stderr, "-run") {
		t.Errorf("the block must be the scope rule with the -run alternative, got: %s", stderr)
	}

	allowed := "echo starting\nbash <<'EOF'\nmake test\nEOF\n"
	input = `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(allowed) + `}}`
	if stderr, err := runContainerSuiteGuard(input, proc); err != nil {
		t.Errorf("a shell-fed make test whose earlier lines stay in the non-Go tree was refused: %s", stderr)
	}
}

// The acceptance case through the hook a polecat actually talks to: from a
// non-Go rig, cd-ing into a Go tree and feeding "make test" to bash is
// refused, while the identical body with a reader line that stays put is
// allowed (gt-1cvqj).
func TestRunTapGuardContainerSuite_HeredocCdIntoGoTreeRefused(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "gastown/refinery/rig")
	nonGo := nonGoTree(t)
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "furiosa",
		"GT_ROLE":    "gastown/polecats/furiosa",
	}, nonGo)

	refused := "cd " + goTree + " && bash <<'EOF'\nmake test\nEOF\n"
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(refused) + `}}`
	if stderr, err := runContainerSuiteGuard(input, proc); err == nil {
		t.Errorf("a shell-fed make test behind a cd into a Go tree was allowed: %s", stderr)
	}

	allowed := "bash <<'EOF'\nmake test\nEOF\n"
	input = `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(allowed) + `}}`
	if stderr, err := runContainerSuiteGuard(input, proc); err != nil {
		t.Errorf("a shell-fed make test whose reader line stays in the non-Go tree was refused: %s", stderr)
	}
}

// The acceptance case, driven through the real hook entry point: a polecat
// writing a file whose heredoc body quotes the bare suite invocation is
// allowed, while the same invocation typed live on the line is not.
func TestRunTapGuardContainerSuite_HeredocBodyIsData(t *testing.T) {
	t.Parallel()
	// cwd "" is unresolvable and judged as a Go tree, the same convention the
	// sibling make-test hook test uses: the live invocation below must be
	// refused, so the only variable left is the heredoc.
	proc := fakeGuardProcess(map[string]string{
		"GT_POLECAT": "nux",
		"GT_ROLE":    "gastown/polecats/nux",
	}, "")

	write := "cat > /tmp/bead.md <<'EOF'\nreproduce with make test\nEOF\n"
	input := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(write) + `}}`
	var buf strings.Builder
	if err := tapGuardContainerSuite(strings.NewReader(input), &buf, proc); err != nil {
		t.Fatalf("writing a file whose heredoc body quotes the suite was refused: %s", buf.String())
	}

	live := "make test"
	input = `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(live) + `}}`
	buf.Reset()
	if err := tapGuardContainerSuite(strings.NewReader(input), &buf, proc); err == nil {
		t.Fatalf("live make test was allowed through the hook: %s", buf.String())
	}
}

// findInvocation ends a make goal list at a shellCommandSeparators token, so
// the tokenizer must hand it a separator written without spaces as its own
// token: "make build;git add test" has no test goal (gt-xdcrl).
func TestFindInvocation_SeparatorsWrittenWithoutSpaces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    int
	}{
		{"semicolon", "make build;git add test", -1},
		{"and", "make build&&git add test", -1},
		{"pipe", "make build|tee test", -1},
		{"newline", "make build\ngit add test", -1},
		{"second make after a semicolon", "make build;make test", 3},
		{"second make after an and", "make build&&make test", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findInvocation(lowerTokens(tt.command), "make", "test"); got != tt.want {
				t.Errorf("findInvocation(%q) = %d, want %d (tokens %q)", tt.command, got, tt.want, lowerTokens(tt.command))
			}
		})
	}
}

// TestFindInvocation pins findInvocation at its edges: a tool with nothing after
// it is not an invocation (and must not index past the slice), the go row stays
// adjacent-only, and the make row finds the goal anywhere after the program
// without matching a word that merely contains it (gt-pm4vn).
func TestFindInvocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tokens []string
		tool   string
		target string
		want   int
	}{
		{"empty tokens", nil, "make", "test", -1},
		{"make is the last token", []string{"make"}, "make", "test", -1},
		{"go is the last token", []string{"go"}, "go", "test", -1},
		{"which go ends on the tool", []string{"which", "go"}, "go", "test", -1},
		{"echo make ends on the tool", []string{"echo", "make"}, "make", "test", -1},
		{"make test", []string{"make", "test"}, "make", "test", 0},
		{"make test after a goal", []string{"make", "clean", "test"}, "make", "test", 0},
		{"make test after an assignment", []string{"make", "goflags=-count=1", "test"}, "make", "test", 0},
		{"make test after a program prefix", []string{"env", "make", "clean", "test"}, "make", "test", 1},
		{"make goals without the target", []string{"make", "clean", "build"}, "make", "test", -1},
		{"make target as a substring of a goal", []string{"make", "test-slow"}, "make", "test", -1},
		{"make target as a variable value", []string{"make", "deploy", "env=test"}, "make", "test", -1},
		{"make target before the program", []string{"test", "make", "clean"}, "make", "test", -1},
		{"second make carries the target", []string{"make", "clean", "&&", "make", "test"}, "make", "test", 3},
		{"second make after a semicolon", []string{"make", "clean", ";", "make", "test"}, "make", "test", 3},
		{"target in a later command is not a goal", []string{"eval", "make", "build", "&&", "git", "add", "test"}, "make", "test", -1},
		{"target after a pipe is not a goal", []string{"make", "build", "|", "tee", "test"}, "make", "test", -1},
		{"target after a semicolon is not a goal", []string{"make", "build", ";", "git", "add", "test"}, "make", "test", -1},
		{"go test is adjacent", []string{"go", "test", "./..."}, "go", "test", 0},
		{"go with a flag between stays unmatched", []string{"go", "-v", "test"}, "go", "test", -1},
		{"go build is not go test", []string{"go", "build", "./..."}, "go", "test", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := findInvocation(tt.tokens, tt.tool, tt.target); got != tt.want {
				t.Errorf("findInvocation(%q, %q, %q) = %d, want %d", tt.tokens, tt.tool, tt.target, got, tt.want)
			}
		})
	}
}

// make is found by path and as gmake, with its goals read as they are after a
// bare make, while other words that merely end in make stay unmatched
// (gt-y1zks).
func TestFindInvocationPathQualifiedMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tokens []string
		want   int
	}{
		{"bare make", []string{"make", "test"}, 0},
		{"usr bin make", []string{"/usr/bin/make", "test"}, 0},
		{"homebrew make", []string{"/opt/homebrew/bin/make", "-j4", "test"}, 0},
		{"gmake", []string{"gmake", "test"}, 0},
		{"path-qualified gmake", []string{"/usr/local/bin/gmake", "clean", "test"}, 0},
		{"after an env prefix", []string{"env", "/usr/bin/make", "test"}, 1},
		{"path-qualified make without the goal", []string{"/usr/bin/make", "build"}, -1},
		{"word ending in make", []string{"cmake", "test"}, -1},
		{"word starting with make", []string{"makefile", "test"}, -1},
		{"directory named make", []string{"/opt/make/bin/tool", "test"}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := findInvocation(tt.tokens, "make", "test"); got != tt.want {
				t.Errorf("findInvocation(%q, make, test) = %d, want %d", tt.tokens, got, tt.want)
			}
		})
	}
}

// The container rule reads a -c payload and a parenthesized cd as the scope
// rule does, and a slot-run wrapper still exempts the shell it starts
// (gt-y1zks).
func TestContainerSuiteShellCPayloadAndParens(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"make test in a shell", "bash -c 'make test'", true},
		{"gmake test", "gmake test", true},
		{"path-qualified make test", "/usr/bin/make test", true},
		{"container package in a shell, opt-in in the payload", `sh -c "GT_TEST_DOCKER=1 go test ./internal/beads"`, true},
		{"container package in a shell, opt-in on the shell", `GT_TEST_DOCKER=1 sh -c "go test ./internal/beads"`, true},
		{"container package in a parenthesized cd", "(cd internal/beads && GT_TEST_DOCKER=1 go test)", true},
		{"slot-wrapped shell", `gt slot run --role gastown/flint -- bash -c "GT_TEST_DOCKER=1 go test ./internal/beads"`, false},
		{"no opt-in, no container", "bash -c 'go test ./internal/beads'", false},
		{"echo", "bash -c 'echo hi'", false},
		{"light package in a parenthesized cd", "(cd internal/style && GT_TEST_DOCKER=1 go test .)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// The nesting cap is a limit on how far the guards read, not a place to run
// the suite from: a shell still nested past maxTestGuardNestDepth is refused,
// because the script inside it was never inspected, and a command at the cap
// with no shell left in it is judged as usual (gt-rws47).
func TestTestGuardsRefuseShellsNestedPastTheCap(t *testing.T) {
	t.Parallel()
	proc := fakeGuardProcess(nil, "")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"heredoc tower one past the cap", "bash <<'L0'\nbash <<'L1'\nbash <<'L2'\nbash <<'L3'\necho hi\nL3\nL2\nL1\nL0\n", true},
		{"heredoc tower at the cap, nothing inside", "bash <<'L0'\nbash <<'L1'\nbash <<'L2'\necho hi\nL2\nL1\nL0\n", false},
		{"heredoc tower at the cap, suite inside", "bash <<'L0'\nbash <<'L1'\nbash <<'L2'\nmake test\nL2\nL1\nL0\n", true},
		{"c payload tower one past the cap", `bash -c "bash -c \"bash -c 'bash -c \\\"echo hi\\\"'\""`, true},
		{"c payload tower at the cap, nothing inside", `bash -c "bash -c 'bash -c \"echo hi\"'"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if reason, _ := evaluateContainerSuiteCommand(proc, tt.command, ""); (reason != "") != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) = %q, blocked want %v", tt.command, reason, tt.blocked)
			}
			if reason, _ := evaluatePolecatTestScope(proc, tt.command); (reason != "") != tt.blocked {
				t.Errorf("evaluatePolecatTestScope(%q) = %q, blocked want %v", tt.command, reason, tt.blocked)
			}
		})
	}
}

// commandSegment drops the ")" that closes the subshell a segment sits in and
// what follows it, and keeps a group the segment opened itself whole, so the
// words after that group still reach the rule (gt-y1zks, gt-rws47).
func TestCommandSegment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"no parens", []string{"go", "test", "./a"}, []string{"go", "test", "./a"}},
		{"closer of the enclosing group", []string{"go", "test", ")"}, []string{"go", "test"}},
		{"opener and closer", []string{"(", "go", "test", "./a", ")"}, []string{"(", "go", "test", "./a"}},
		{"nested openers lead", []string{"(", "(", "go", "test", ")", ")"}, []string{"(", "(", "go", "test"}},
		{"words after the closer are cut", []string{"go", "test", ")", "2>&1"}, []string{"go", "test"}},
		{"balanced group inside is kept", []string{"go", "test", "(", "x", ")", "./internal/cmd"}, []string{"go", "test", "(", "x", ")", "./internal/cmd"}},
		{"balanced group then the closer", []string{"go", "test", "(", "x", ")", "./a", ")"}, []string{"go", "test", "(", "x", ")", "./a"}},
		{"only a closer", []string{")"}, []string{}},
		{"empty", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := commandSegment(tt.in); !reflect.DeepEqual(got, tt.want) && (len(got) != 0 || len(tt.want) != 0) {
				t.Errorf("commandSegment(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A container-bearing go test or make test inside a command substitution,
// backticks or eval starts the same unwrapped containers as one on its own
// line; the segment judge saw "out=$(go" and eval's quoted string as words that
// are no go binary (gt-t1pct). A slot-run inside the substitution still
// exempts its own go test, and a literal "$(" does not run anything.
func TestEvaluateContainerSuiteCommand_NestedShapes(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dag")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"substitution, container package", `GT_TEST_DOCKER=1 out=$(go test ./internal/beads/... 2>&1 | tail -5)`, true},
		{"substitution, make test", `echo $(make test)`, true},
		{"substitution, pattern group in the -run filter", `GT_TEST_DOCKER=1 out=$(go test ./internal/beads -run 'Test(A|B)' 2>&1)`, true},
		{"substitution, grep group after the run", `GT_TEST_DOCKER=1 out=$(go test ./internal/beads 2>&1 | grep -E '(ok|FAIL)')`, true},
		{"substitution, awk field group after the run", `GT_TEST_DOCKER=1 out=$(go test ./internal/beads 2>&1 | awk '{print $(NF)}')`, true},
		{"substitution, nested substitution after the run", `GT_TEST_DOCKER=1 out=$(go test ./internal/beads 2>&1 | tail -n $(nproc))`, true},
		{"substitution, run nested in an echo", `GT_TEST_DOCKER=1 echo "$(echo $(go test ./internal/beads))"`, true},
		{"substitution, unterminated", `echo $(make test`, true},
		{"substitution, unterminated backticks", "echo `make test", true},
		{"backticks, make test", "`make test`", true},
		{"eval, container package", `GT_TEST_DOCKER=1 eval "go test ./internal/beads/..."`, true},
		{"eval, make test", `eval "make test"`, true},
		{"opt-in earlier on the line", `export GT_TEST_DOCKER=1; x=$(go test ./internal/beads/...)`, true},
		{"substitution after a cd into a container package", `cd internal/beads && GT_TEST_DOCKER=1 x=$(go test .)`, true},
		{"slot-wrapped substitution", `gt slot run --role gastown/flint -- echo $(make test)`, true},
		{"unquoted heredoc body, make test", "cat > note.md <<EOF\nrun $(make test)\nEOF", true},
		{"unquoted heredoc body, container package", "GT_TEST_DOCKER=1 cat > note.md <<EOF\n$(go test ./internal/beads/...)\nEOF", true},
		{"unquoted heredoc body, backticks", "cat <<EOF\n`make test`\nEOF", true},
		{"unquoted heredoc body, <<- form", "cat <<-EOF\n\t$(make test)\n\tEOF", true},
		{"unquoted heredoc body, after an apostrophe", "cat <<EOF\ndon't $(make test)\nEOF", true},
		{"unquoted heredoc body, after a double quote", "cat <<EOF\nsay \"hi $(make test)\nEOF", true},
		{"unquoted heredoc body, nested substitution", "cat <<EOF\n$(echo $(make test))\nEOF", true},
		{"unquoted heredoc body, later command", "cat <<EOF\nplain\nEOF\necho $(make test)", true},
		{"unquoted heredoc body, past the nesting cap", "bash -c \"bash -c 'bash -c \\\"cat <<EOF\n$(pwd)\nEOF\\\"'\"", true},

		{"slot run inside the substitution", `x=$(gt slot run --role gastown/flint -- make test)`, false},
		{"slot run inside eval", `eval "gt slot run --role gastown/flint -- make test"`, false},
		{"no opt-in, bare go test", `x=$(go test ./internal/beads/...)`, false},
		{"substitution with a group, no opt-in", `out=$(go test ./internal/beads -run 'Test(A|B)' 2>&1)`, false},
		{"substitution with a group, opt-in off", `GT_TEST_DOCKER=0 out=$(go test ./internal/beads -run 'Test(A|B)' 2>&1)`, false},
		{"pwd", `echo $(pwd)`, false},
		{"single-quoted substitution", `echo '$(make test)'`, false},
		{"escaped substitution", `echo \$(make test)`, false},
		{"eval of echo", `eval "echo hi"`, false},
		{"eval as an argument", `grep eval notes.txt`, false},
		{"quoted heredoc body", "cat > note.md <<'EOF'\nrun $(make test)\nEOF", false},
		{"double-quoted heredoc tag", "cat > note.md <<\"EOF\"\nrun $(make test)\nEOF", false},
		{"escaped substitution in an unquoted body", "cat <<EOF\nrun \\$(make test)\nEOF", false},
		{"harmless substitution in an unquoted body", "cat <<EOF\nin $(pwd)\nEOF", false},
		{"unquoted heredoc body, bare go test without the opt-in", "cat <<EOF\n$(go test ./internal/beads/...)\nEOF", false},
		{"empty substitution in an unquoted body", "cat <<EOF\n$( )\nEOF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, matched := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// commandSubstitutionSpans cuts a body with the parentheses balanced and the
// quotes respected, and runs a substitution that never closes to the end of the
// text (gt-wrr5c, gt-6v07e). A backslash keeps the next character literal, so
// an escaped backtick is text rather than the end of a backtick body. A process
// substitution is a span where the shell performs one — a command line, not
// inside a quote or a comment (gt-jj3or).
func TestCommandSubstitutionSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", `x=$(go test .)`, []string{"go test ."}},
		{"quoted group", `x=$(grep -E '(ok|FAIL)' f)`, []string{"grep -E '(ok|FAIL)' f"}},
		{"awk field", `x=$(awk '{print $(NF)}' f)`, []string{"awk '{print $(NF)}' f"}},
		{"nested", `x=$(tail -n $(nproc))`, []string{"tail -n $(nproc)"}},
		{"nested in double quotes", `x=$(echo "$(pwd))")`, []string{`echo "$(pwd))"`}},
		{"subshell", `x=$( (cd a && ls) )`, []string{` (cd a && ls) `}},
		{"two", `a=$(one) b=$(two (x))`, []string{"one", "two (x)"}},
		{"backticks", "x=`pwd`", []string{"pwd"}},
		{"backticks with an escaped backtick", "x=`echo \\`pwd\\``", []string{"echo \\`pwd\\`"}},
		{"paren inside backticks in the body", "x=$(echo `a)b`)", []string{"echo `a)b`"}},
		{"in double quotes", `echo "$(pwd)"`, []string{"pwd"}},
		{"single quoted", `echo '$(pwd)'`, nil},
		{"single quote hiding a paren", `echo '$(' "$(pwd)" ')'`, []string{"pwd"}},
		{"escaped", `echo \$(pwd)`, nil},
		{"escaped paren in the body", `x=$(echo \) ; pwd)`, []string{`echo \) ; pwd`}},
		{"unterminated", `x=$(go test .`, []string{"go test ."}},
		{"unterminated backtick", "x=`go test .", []string{"go test ."}},
		{"unterminated quote in the body", `echo $(a "b) $(c)`, []string{`a "b) $(c)`}},
		// gt-qg832: the quote after a quoted dollar-paren's "(" is the one that
		// ends the double-quoted word, so the ")" that closes the substitution
		// around it is found instead of the body running past it.
		{"quoted dollar-paren in a substitution", `echo $(echo "$(" ; rm -rf /)`, []string{`echo "$(" ; rm -rf /`}},
		{"unterminated after a closed one", `a=$(one) b=$(two`, []string{"one", "two"}},
		{"process substitution", `cat <(go test .)`, []string{"go test ."}},
		{"output process substitution", `cat > >(tee out)`, []string{"tee out"}},
		{"nested process substitution", `cat <(cat <(go test .))`, []string{"cat <(go test .)"}},
		{"unterminated process substitution", `cat <(go test .`, []string{"go test ."}},
		{"single-quoted process substitution", `echo '<(go test .)'`, nil},
		{"double-quoted process substitution", `echo "<(go test .)"`, nil},
		{"process substitution in a comment", `echo hi # <(go test .)`, nil},
		// A heredoc body the substitution holds is data, so its quote mark
		// opens nothing and its ")" closes nothing (gt-lx8i8).
		{"heredoc body with a quote mark in a double-quoted substitution", "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"; pwd", []string{"cat <<'EOF'\nit's a note\nEOF\n"}},
		{"heredoc body with a quote mark in a substitution", "x=$(cat <<EOF\nit's a note\nEOF\n)", []string{"cat <<EOF\nit's a note\nEOF\n"}},
		{"heredoc body holding a closing paren", "x=$(cat <<'EOF'\nfoo)\nEOF\n)", []string{"cat <<'EOF'\nfoo)\nEOF\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, s := range commandSubstitutionSpans(tt.in) {
				got = append(got, tt.in[s.bodyStart:s.bodyEnd])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commandSubstitutionSpans(%q) bodies = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// An apostrophe in a "#" comment is plain text and a "\'" inside $'...' does not
// close the string, so neither hides a live "$(...)" behind it. The scan still
// reads quotes elsewhere as before: 'it' closes, a#b is no comment, and a "$(" in
// a comment is still judged (gt-2yc0u).
func TestSubstitutionSpansCommentAndANSIQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"apostrophe in a comment", "# don't\nx=$(git reset --hard)", []string{"git reset --hard"}},
		{"apostrophe in a trailing comment", "echo hi # don't\nx=$(git reset --hard)", []string{"git reset --hard"}},
		{"comment after a separator", "true;# don't\nx=$(git reset --hard)", []string{"git reset --hard"}},
		{"double quote in a comment", "# say \"hi\nx=$(git reset --hard)", []string{"git reset --hard"}},
		{"substitution in a comment", "# don't $(git reset --hard)", []string{"git reset --hard"}},
		{"escaped quote in an ANSI-C string", `echo $'it\'s' $(git reset --hard)`, []string{"git reset --hard"}},
		{"ANSI-C string hiding a substitution", `echo $'$(pwd)' $(git reset --hard)`, []string{"git reset --hard"}},
		// gt-22glo: bash deletes the backslash-newline between "$" and "'", so
		// the quote opens an ANSI-C string whose \' does not close it, and the
		// "$(git reset --hard)" inside is text.
		{"ANSI-C string opened across a backslash-newline hides a substitution", "echo $\\\n'a\\' $(git reset --hard) '", nil},
		{"unterminated ANSI-C string reads as plain quotes", `echo $'it\'s $(git reset --hard)`, []string{"git reset --hard"}},
		{"plain single quotes", `echo 'it' $(git reset --hard)`, []string{"git reset --hard"}},
		{"hash inside a word", `echo a#b $(git reset --hard)`, []string{"git reset --hard"}},
		{"hash after an escaped space", `echo \ #b 'x $(git reset --hard)'`, nil},
		{"hash inside double quotes", "echo \"# don't\" $(git reset --hard)", []string{"git reset --hard"}},
		{"hash in a parameter expansion", `echo ${#x} 'a $(git reset --hard)'`, nil},
		{"apostrophe after the comment ended", "# note\necho 'a $(git reset --hard)'", nil},
		// A "#" opens a comment after the ")" that ends a word and not after
		// the one that closes a "$(...)", "$((...))" or "<(...)" group, whose
		// "#" is a word part: the apostrophe after "(true)" hides nothing, and
		// the process substitution after "$(date)" or after the quoted paren
		// in '$(")")' still runs (gt-5wtmz).
		{"hash after a subshell is a comment", "(true)#c 'a\n$(git reset --hard)", []string{"git reset --hard"}},
		{"hash after a substitution close is a word part", "cat $(date)#<(git reset --hard)", []string{"date", "git reset --hard"}},
		{"comment to the end of the text inside a body", "x=$(# )\n$(git reset --hard)", []string{"# )\n$(git reset --hard)"}},
		{"quoted paren before the hash", `cat $(")")#<(git reset --hard)`, []string{`")"`, "git reset --hard"}},
		{"quoted paren in a longer body", `cat $(echo ")")#<(git reset --hard)`, []string{`echo ")"`, "git reset --hard"}},
		{"single-quoted paren before the hash", `cat $(')')#<(git reset --hard)`, []string{`')'`, "git reset --hard"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, s := range commandSubstitutionSpans(tt.in) {
				got = append(got, tt.in[s.bodyStart:s.bodyEnd])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commandSubstitutionSpans(%q) bodies = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	for _, command := range []string{
		"# don't\nx=$(git reset --hard)",
		`echo $'it\'s' $(git reset --hard)`,
		`echo a#b $(git reset --hard)`,
		"(true)#c 'a\n$(git reset --hard)",
		`cat $(")")#<(git reset --hard)`,
	} {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked", command)
		}
	}
	scope := "# don't forget\nout=$(make gate)"
	if reason, _ := evaluatePolecatTestScope(fakeGuardProcess(nil, ""), scope); reason == "" {
		t.Errorf("evaluatePolecatTestScope(%q) allowed, want blocked", scope)
	}
}

// heredocSubstitutionSpans cuts the substitutions of an unquoted heredoc body.
// The body is not shell text: a quote mark in it opens nothing, so an
// apostrophe in prose does not hide a live "$(" the way commandSubstitutionSpans
// reads it on a command line, and only a backslash keeps the next character
// literal (gt-6fkng).
func TestHeredocSubstitutionSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "run $(make gate)\n", []string{"make gate"}},
		{"after an apostrophe", "don't $(make gate)\n", []string{"make gate"}},
		{"between single quotes", "'$(make gate)'\n", []string{"make gate"}},
		{"inside double quotes", "\"$(make gate)\"\n", []string{"make gate"}},
		{"after an unbalanced double quote", "say \"hi $(make gate)\n", []string{"make gate"}},
		{"backticks", "run `make gate`\n", []string{"make gate"}},
		{"nested", "$(echo $(pwd))\n", []string{"echo $(pwd)"}},
		{"quoted group in the body", "$(grep -E '(ok|FAIL)' f)\n", []string{"grep -E '(ok|FAIL)' f"}},
		{"two", "$(a) and `b`\n", []string{"a", "b"}},
		{"escaped", "run \\$(make gate)\n", nil},
		{"escaped backslash", "run \\\\$(make gate)\n", []string{"make gate"}},
		{"unterminated", "run $(make gate\n", []string{"make gate\n"}},
		{"process substitution is data", "cat <(make gate)\n", nil},
		{"no substitution", "make gate\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, s := range heredocSubstitutionSpans(tt.in) {
				got = append(got, tt.in[s.bodyStart:s.bodyEnd])
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("heredocSubstitutionSpans(%q) bodies = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestTapGuardDeepReviewP3Batch pins the command-text gaps the deep review
// filed against the internal/cmd guards (gt-jj3or). Each subtest is a command
// the shell runs — or a form the guard's own direct spelling already refuses —
// that at least one guard allowed before this change. They share one test
// because they came from one review, not because they share a scanner: the
// subtests below touch the command-substitution scan, the heredoc and -c
// handoffs, the slot-run exemption and the test-scope rule.
func TestTapGuardDeepReviewP3Batch(t *testing.T) {
	t.Parallel()
	root := fakeModule(t, "gastown/polecats/dementus/gastown")

	// The shell expands a $(...) written in an unquoted heredoc body while it
	// reads the body, so the container guard judges the body's substitution as
	// the command line it is, with the container opt-in the invocation carries
	// (heredocSubstitutionScripts).
	t.Run("unquoted_heredoc_substitution", func(t *testing.T) {
		command := "GT_TEST_DOCKER=1 cat <<EOF\n$(go test ./internal/beads)\nEOF"
		reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), command, "")
		if want := "targets testcontainers-backed package(s)"; !strings.Contains(reason, want) {
			t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want %q", command, reason, want)
		}
	})

	// A process substitution runs the command inside it and hands the word the
	// result, so "cat <(gh pr create --fill)" opens the PR and "cat <(git reset
	// --hard)" resets the tree though the segment judge sees only a cat. Both
	// angle forms are spans; a quoted one is plain text the shell never runs.
	t.Run("process_substitution", func(t *testing.T) {
		if !matchesPRWorkflowCommand("cat <(gh pr create --fill)") {
			t.Errorf("matchesPRWorkflowCommand(%q) = false, want true", "cat <(gh pr create --fill)")
		}
		proc := fakeGuardProcess(nil, root)
		sess := guardSession{proc: proc}
		wantDanger := "Hard reset discards all uncommitted changes irreversibly"
		for _, command := range []string{"cat <(git reset --hard)", "cat >(git reset --hard)"} {
			if reason, _ := evaluateDangerousCommand(command, 0, sess); !strings.Contains(reason, wantDanger) {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want %q", command, reason, wantDanger)
			}
		}
		if reason, _ := evaluateDangerousCommand(`echo "<(git reset --hard)"`, 0, sess); reason != "" {
			t.Errorf("evaluateDangerousCommand(quoted process substitution) blocked (%q), want allowed", reason)
		}
		// A heredoc body the shell expands is still not a script: process
		// substitution is not part of that expansion, so its "<(...)" is data
		// until a shell is handed the body and parses it.
		if reason, _ := evaluateDangerousCommand("cat > f <<EOF\ncat <(git reset --hard)\nEOF", 0, sess); reason != "" {
			t.Errorf("evaluateDangerousCommand(process substitution in a data body) blocked (%q), want allowed", reason)
		}
		if reason, _ := evaluateDangerousCommand("bash <<EOF\ncat <(git reset --hard)\nEOF", 0, sess); !strings.Contains(reason, wantDanger) {
			t.Errorf("evaluateDangerousCommand(process substitution in a script body) reason = %q, want %q", reason, wantDanger)
		}
		// The container-suite and polecat test-scope guards read the same spans
		// through commandSubstitutionSpans, so the command inside one is judged
		// as the shell runs it. An unterminated "<(" runs to the end of the
		// text, so the words after the last one are read the same way.
		wantContainer := "targets testcontainers-backed package(s)"
		for _, command := range []string{
			`GT_TEST_DOCKER=1 cat <(go test ./internal/beads)`,
			`GT_TEST_DOCKER=1 cat >(go test ./internal/beads)`,
			`GT_TEST_DOCKER=1 cat <(go test ./internal/beads`,
		} {
			if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); !strings.Contains(reason, wantContainer) {
				t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want %q", command, reason, wantContainer)
			}
		}
		scopeBlocked := `cat <(make test)`
		scopeReason, _ := evaluatePolecatTestScope(proc, scopeBlocked)
		if want := "runs the whole suite"; !strings.Contains(scopeReason, want) {
			t.Errorf("evaluatePolecatTestScope(%q) reason = %q, want %q", scopeBlocked, scopeReason, want)
		}
		// A quoted angle form is plain text: the shell runs nothing.
		for _, command := range []string{
			`GT_TEST_DOCKER=1 echo '<(go test ./internal/beads)'`,
			`GT_TEST_DOCKER=1 echo "<(go test ./internal/beads)"`,
		} {
			if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); reason != "" {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked (%q), want allowed", command, reason)
			}
		}
	})

	// A -c payload runs in the segment's directory. When the walk cannot place
	// that directory, "./beads" resolves against nothing the guard knows, so it
	// can be internal/beads — a testcontainers package — and the payload is
	// refused rather than read against the invocation's own directory.
	t.Run("payload_unplaced_dir", func(t *testing.T) {
		command := `cd "$X" && GT_TEST_DOCKER=1 bash -c 'go test -run TestOne ./beads'`
		reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), command, "")
		if !strings.Contains(reason, "names a directory the guard cannot place") {
			t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want the unplaced-directory refusal", command, reason)
		}
	})

	// The same handoff for a script fed to a shell: a body numbered "." is the
	// directory the body runs in, which the walk could not place, so both rules
	// refuse it with their own unplaced-directory refusal.
	t.Run("heredoc_unplaced_dir", func(t *testing.T) {
		command := "cd \"$X\" && GT_TEST_DOCKER=1 bash <<EOF\ngo test .\nEOF"
		proc := fakeGuardProcess(nil, root)
		want := "names a directory the guard cannot place"
		if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); !strings.Contains(reason, want) {
			t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want %q", command, reason, want)
		}
		if reason, _ := evaluatePolecatTestScope(proc, command); !strings.Contains(reason, want) {
			t.Errorf("evaluatePolecatTestScope(%q) reason = %q, want %q", command, reason, want)
		}
	})

	// The slot-run exemption is anchored to the segment's own command, not a
	// subsequence of it: the three words after go's -args are go's argument,
	// and no slot runs. A wrapper behind leading assignments and launchers is
	// still the wrapper (idleGateSlotRunPrefix's reading).
	t.Run("slot_run_anchored", func(t *testing.T) {
		proc := fakeGuardProcess(nil, root)
		blocked := "GT_TEST_DOCKER=1 go test ./internal/beads -args gt slot run"
		reason, _ := evaluateContainerSuiteCommand(proc, blocked, "")
		if want := "targets testcontainers-backed package(s)"; !strings.Contains(reason, want) {
			t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want %q", blocked, reason, want)
		}
		allowed := []string{
			"gt slot run -- go test ./internal/beads",
			"env X=1 gt slot run -- go test ./internal/beads",
			"timeout 5 gt slot run -- go test ./internal/beads",
		}
		for _, command := range allowed {
			if reason, _ := evaluateContainerSuiteCommand(proc, command, ""); reason != "" {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked (%q), want allowed", command, reason)
			}
		}
	})

	// The same handoff for a command substitution: the directory its own cd
	// could not place is carried to its body as unplaced, so "go test ./beads"
	// there is refused rather than read as the base's own package (gt-jj3or).
	t.Run("substitution_unplaced_dir", func(t *testing.T) {
		command := `cd "$X" && GT_TEST_DOCKER=1 v=$(go test ./beads)`
		reason, _ := evaluateContainerSuiteCommand(fakeGuardProcess(nil, root), command, "")
		if want := "names a directory the guard cannot place"; !strings.Contains(reason, want) {
			t.Errorf("evaluateContainerSuiteCommand(%q) reason = %q, want %q", command, reason, want)
		}
	})

	// The backslash-escaped backtick pair is literal to the outer scan, yet the
	// shell drops those backslashes when it parses the body, so the body it runs
	// holds a live substitution around make gate (substitutionBody).
	t.Run("nested_escaped_backtick", func(t *testing.T) {
		command := "echo `echo \\`make gate\\``"
		reason, _ := evaluatePolecatTestScope(fakeGuardProcess(nil, root), command)
		if want := "runs the whole suite"; !strings.Contains(reason, want) {
			t.Errorf("evaluatePolecatTestScope(%q) reason = %q, want %q", command, reason, want)
		}
	})

	// The shell turns \$ into $ inside a backtick body too, so a "\$(...)" there
	// is a live substitution the body's own parse runs (gt-tder1).
	t.Run("escaped_dollar_in_backticks", func(t *testing.T) {
		command := "echo `echo \\$(make test)`"
		if reason, _ := evaluatePolecatTestScope(fakeGuardProcess(nil, root), command); reason == "" {
			t.Errorf("evaluatePolecatTestScope(%q) allowed, want blocked", command)
		}
	})

	// A doubled backslash pairs with itself rather than with the "$" after it,
	// so the "$(" stays behind one backslash and the body's parse runs nothing;
	// a second pair restores it. The pass reads parity, not presence, so the
	// guard must judge each of these the way the shell does (substitutionBody).
	t.Run("doubled_backslash_in_backticks", func(t *testing.T) {
		proc := fakeGuardProcess(nil, root)
		literal := "echo `echo \\\\$(make test)`"
		if reason, _ := evaluatePolecatTestScope(proc, literal); reason != "" {
			t.Errorf("evaluatePolecatTestScope(%q) blocked (%q), want allowed", literal, reason)
		}
		live := "echo `echo \\\\\\\\$(make test)`"
		reason, _ := evaluatePolecatTestScope(proc, live)
		if want := "runs the whole suite"; !strings.Contains(reason, want) {
			t.Errorf("evaluatePolecatTestScope(%q) reason = %q, want %q", live, reason, want)
		}
	})
}

// substitutionBody drops the backslash of each \\, \` and \$ pair in a backtick
// body, the three the shell unescapes there, in one left-to-right pass: "\$("
// becomes the live "$(", while "\\$(" becomes "\$(", a literal "$(" behind a
// backslash. Any other backslash stays, and a "$(...)" or process-substitution
// body is returned as written (gt-tder1).
func TestSubstitutionBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain backticks", "x=`pwd`", "pwd"},
		{"escaped nested backtick", "x=`echo \\`pwd\\``", "echo `pwd`"},
		{"escaped dollar substitution", "echo `echo \\$(git reset --hard)`", "echo $(git reset --hard)"},
		{"escaped backslash", "x=`echo \\\\ hi`", "echo \\ hi"},
		{"doubled backslash then dollar", "x=`echo \\\\$(make test)`", "echo \\$(make test)"},
		{"other escape stays", "x=`echo \\n`", "echo \\n"},
		{"dollar paren body as written", `x=$(echo \$(pwd))`, `echo \$(pwd)`},
		{"process substitution body as written", `cat <(echo \$(pwd))`, `echo \$(pwd)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spans := commandSubstitutionSpans(tt.in)
			if len(spans) != 1 {
				t.Fatalf("commandSubstitutionSpans(%q) = %d spans, want 1", tt.in, len(spans))
			}
			if got := substitutionBody(tt.in, spans[0]); got != tt.want {
				t.Errorf("substitutionBody(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	// The unescaped body is what the guards scan next, so its own scan finds the
	// command the "\$(...)" runs.
	in := "echo `echo \\$(git reset --hard)`"
	body := substitutionBody(in, commandSubstitutionSpans(in)[0])
	nested := commandSubstitutionSpans(body)
	if len(nested) != 1 || body[nested[0].bodyStart:nested[0].bodyEnd] != "git reset --hard" {
		t.Errorf("commandSubstitutionSpans(%q) = %v, want one span holding %q", body, nested, "git reset --hard")
	}
}

// A process substitution that holds several commands is the script the shell
// runs, whatever command prints it: the body is cut at the separators, and a
// quoted word that carries its own parentheses does not close it (gt-n2gt8).
func TestStdinShellPayloadsMultiCommandProcessSubstitution(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"two echoes", `bash <(echo 'echo a'; echo 'make test')`, true},
		{"printf with a quoted substitution word", `bash <(printf '%s\n' 'echo $(date)' 'make test')`, true},
		{"second command behind &&", `bash <(echo true && echo 'make test')`, true},
		{"second command behind a pipe", `bash <(echo true | echo 'make test')`, true},
		{"on stdin", `bash < <(echo 'echo a'; echo 'make test')`, true},
		{"multi-command body that never closes", `bash <(echo 'echo a'; echo 'make test'`, true},
		{"nested substitution then the script", `bash <(echo $(date); echo 'make test')`, true},
		{"quoted close paren ahead of the script", `bash <(echo ')'; echo 'make test')`, true},
		{"quoted open paren ahead of the script", `bash <(echo '('; echo 'make test')`, true},
		{"harmless commands", `bash <(echo 'echo a'; echo 'make lint')`, false},
		{"commands behind the substitution are not its body", `bash <(echo 'echo a'); echo 'make lint'`, false},
		{"script file operand takes the substitution as an argument", `bash ./run.sh <(echo a; echo 'make test')`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}

// bash -s reads its script from stdin, so a process substitution beside it is a
// positional parameter and the pipe is still the script (gt-n2gt8). A
// substitution on stdin replaces the pipe, and -s after the substitution is the
// script file's own argument.
func TestStdinShellPayloadsDashSWithProcessSubstitution(t *testing.T) {
	t.Parallel()
	goRoot := fakeModule(t, "gastown/refinery/rig")
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"pipe is the script", `echo 'make test' | bash -s <(echo true)`, true},
		{"pipe is the script, clustered flags", `echo 'make test' | bash -es <(echo true)`, true},
		{"pipe is the script, sh", `echo 'make test' | sh -s <(echo true)`, true},
		{"harmless pipe", `echo 'make lint' | bash -s <(echo 'make test')`, false},
		{"substitution on stdin replaces the pipe", `echo 'make test' | bash -s < <(echo true)`, false},
		{"substitution on stdin is the script", `bash -s < <(echo 'make test')`, true},
		{"-s after the substitution is the file's argument", `echo 'make lint' | bash <(echo 'make test') -s`, true},
		{"no -s, the substitution replaces the pipe", `echo 'make test' | bash <(echo true)`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, goRoot)
			reason, matched := evaluateContainerSuiteCommand(proc, tt.command, "")
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateContainerSuiteCommand(%q) blocked = %v (reason %q, matched %v), want %v", tt.command, got, reason, matched, tt.blocked)
			}
		})
	}
}
