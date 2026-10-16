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
		// -w (print directory) and -i (ignore errors) take no value, unlike
		// the uppercase -W (what-if file) and -I (include dir): the -C behind
		// a bare -w or -i is still the run directory (gt-i6jw6).
		{"make -w -C into the Go tree", npmRoot, "make -w -C " + goRoot + " test", true},
		{"make -i -C into the Go tree", npmRoot, "make -i -C " + goRoot + " test", true},
		{"make -w -C out of the Go tree", goRoot, "make -w -C " + npmRoot + " test", false},
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

// The whole-suite goals other than test and build read the same run directory:
// a bare -i before the -C must not hide it (gt-i6jw6).
func TestMakeBareIgnoreErrorsFlagKeepsTheDirectory(t *testing.T) {
	t.Parallel()
	npmRoot := nonGoTree(t)
	goRoot := fakeModule(t, "gastown/refinery/rig")
	for _, command := range []string{
		"make -i -C " + goRoot + " gate",
		"make -w -C " + goRoot + " gate",
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
			name:        "shell-fed body may itself carry a heredoc",
			command:     "bash <<'OUTER'\nbash <<'INNER'\nmake test\nINNER\nOUTER\n",
			wantRefused: true, wantScopeOut: true,
		},
		{
			name:        "shell-fed container run is refused",
			command:     "bash <<'EOF'\nGT_TEST_DOCKER=1 go test ./internal/beads/...\nEOF\n",
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
			// The fallback for a cd the walk cannot place: the body keeps the
			// directory the invocation already had. An unplaceable directory
			// is not evidence that the body runs in a guarded tree, the same
			// reading scanWalkRoot gives an unknown walk root (gt-3e6wa).
			name:         "a cd the guard cannot place leaves the body where it was",
			cwd:          nonGo,
			command:      "cd \"$BUILD_DIR\" && bash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
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
// an earlier cd the walk cannot place leaves the body where it was.
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
			// The unknown-root fallback, reached through the preceding text: an
			// unplaceable directory is not evidence the body runs in a Go tree.
			name:         "an earlier cd the guard cannot place leaves the body where it was",
			cwd:          nonGo,
			command:      "cd \"$BUILD_DIR\"\nbash <<'EOF'\nmake test\nEOF\n",
			wantRefused:  false,
			wantScopeOut: false,
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
		{"backticks, make test", "`make test`", true},
		{"eval, container package", `GT_TEST_DOCKER=1 eval "go test ./internal/beads/..."`, true},
		{"eval, make test", `eval "make test"`, true},
		{"opt-in earlier on the line", `export GT_TEST_DOCKER=1; x=$(go test ./internal/beads/...)`, true},
		{"substitution after a cd into a container package", `cd internal/beads && GT_TEST_DOCKER=1 x=$(go test .)`, true},
		{"slot-wrapped substitution", `gt slot run --role gastown/flint -- echo $(make test)`, true},

		{"slot run inside the substitution", `x=$(gt slot run --role gastown/flint -- make test)`, false},
		{"slot run inside eval", `eval "gt slot run --role gastown/flint -- make test"`, false},
		{"no opt-in, bare go test", `x=$(go test ./internal/beads/...)`, false},
		{"pwd", `echo $(pwd)`, false},
		{"single-quoted substitution", `echo '$(make test)'`, false},
		{"escaped substitution", `echo \$(make test)`, false},
		{"eval of echo", `eval "echo hi"`, false},
		{"eval as an argument", `grep eval notes.txt`, false},
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
