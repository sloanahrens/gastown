package cmd

import (
	"io"
	"os"
	"path/filepath"
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
		// "--" ends make's option processing, so what follows is a target it
		// never reads as a flag: the tree stays the one it started in.
		{"a -C after the end of options", npmRoot, "make -- -C " + goRoot + " test", false},
		{"an alternate makefile", npmRoot, "make -f other.mk test", true},
		{"an attached alternate makefile", npmRoot, "make -fother.mk test", true},
		{"a long-form makefile", npmRoot, "make --makefile=other.mk test", true},
		{"a makefile from stdin", npmRoot, "make - test", true},
		{"a -C from an unknown cwd", "", "make -C . test", true},
		{"no cwd at all", "", "make test", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertBothGuardsJudge(t, tt.command, tt.cwd, tt.blocked)
		})
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
