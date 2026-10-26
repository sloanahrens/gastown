package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptFileTestTown is a hermetic Gas Town layout for the script-files guard:
//
//	<root>/gt/<rig>/polecats/<name>/<repo>    a polecat's worktree
//	<root>/gt/<rig>/polecats/<other>/<repo>   a sibling polecat's worktree
//	<root>/gt/<rig>/crew/<member>             a crew member's worktree
//	<root>/scratch                            scratch outside every worktree
//
// The polecat worktree seeds "build.sh" (a script the guard must let the
// session edit), "release.txt" (a non-script operand an install command copies)
// and "bin/" (an install -t directory).
type scriptFileTestTown struct {
	root     string
	rigRoot  string
	worktree string
	sibling  string
	crew     string
	scratch  string
	repo     string
}

func newScriptFileTestTown(t *testing.T) scriptFileTestTown {
	t.Helper()
	root := t.TempDir()
	rig, name, other, member := "gastown", "ace", "coral", "alice"
	rigRoot := filepath.Join(root, "gt", rig)
	worktree := filepath.Join(rigRoot, "polecats", name, rig)
	town := scriptFileTestTown{
		root:     root,
		rigRoot:  rigRoot,
		worktree: worktree,
		sibling:  filepath.Join(rigRoot, "polecats", other, rig),
		crew:     filepath.Join(rigRoot, "crew", member),
		scratch:  filepath.Join(root, "scratch"),
		repo:     rig,
	}
	for _, dir := range []string{
		worktree,
		town.sibling,
		town.crew,
		town.scratch,
		filepath.Join(worktree, "bin"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	// A .git entry marks each checkout the way git does, which is how a crew
	// session's worktree is found (a polecat's comes from GT_POLECAT_PATH).
	for _, dir := range []string{worktree, town.crew} {
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
			t.Fatalf("marking %s as a checkout: %v", dir, err)
		}
	}
	// The seeded scripts carry no shebang: the guard judges a path by its
	// existence and suffix, and a shebang literal in a test file is itself a
	// no-exec-files violation (internal/testpolicy).
	for _, file := range []struct{ path, content string }{
		{filepath.Join(worktree, "build.sh"), "go build ./...\n"},
		{filepath.Join(worktree, "release.txt"), "notes\n"},
		{filepath.Join(town.sibling, "build.sh"), "go build ./...\n"},
	} {
		if err := os.WriteFile(file.path, []byte(file.content), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", file.path, err)
		}
	}
	return town
}

// polecatProc is a polecat session in town's worktree: the payload cwd names
// the worktree and GT_POLECAT_PATH names the same tree, as the session manager
// sets them.
func (town scriptFileTestTown) polecatProc() guardProcess {
	return fakeGuardProcess(map[string]string{
		"GT_ROLE":         "gastown/polecats/ace",
		"GT_RIG":          "gastown",
		"GT_POLECAT":      "ace",
		"GT_POLECAT_PATH": town.worktree,
	}, "")
}

// crewProc is a crew session in town's crew checkout. Crew carry no worktree
// variable: the guard finds the checkout from cwd.
func (town scriptFileTestTown) crewProc() guardProcess {
	return fakeGuardProcess(map[string]string{
		"GT_ROLE": "gastown/crew/alice",
		"GT_RIG":  "gastown",
		"GT_CREW": "alice",
	}, "")
}

// run invokes the guard as the PreToolUse hook does: a payload on stdin, the
// guard's error as the verdict.
func (town scriptFileTestTown) run(t *testing.T, proc guardProcess, cwd, tool, toolInput string, stderr io.Writer) error {
	t.Helper()
	payload := fmt.Sprintf(`{"tool_name":%q,"cwd":%q,"tool_input":%s}`, tool, cwd, toolInput)
	return tapGuardScriptFiles(strings.NewReader(payload), stderr, proc)
}

// assertVerdict checks a guard result against the table's expectation, and
// that a denial carries exit code 2 (the Claude Code "block" signal).
func assertVerdict(t *testing.T, got error, wantBlocked bool, label string) {
	t.Helper()
	if !wantBlocked {
		if got != nil {
			t.Errorf("%s: allowed expected, got error %v", label, got)
		}
		return
	}
	code, ok := IsSilentExit(got)
	if !ok || code != 2 {
		t.Errorf("%s: expected a silent exit 2 denial, got %v", label, got)
	}
}

func TestScriptFileGuardFileTools(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.polecatProc()

	tests := []struct {
		name    string
		tool    string
		target  string
		blocked bool
	}{
		{"new sh", "Write", filepath.Join(town.worktree, "deploy.sh"), true},
		{"new bash", "Write", filepath.Join(town.worktree, "deploy.bash"), true},
		{"new zsh", "Write", filepath.Join(town.worktree, "deploy.zsh"), true},
		{"new perl", "Write", filepath.Join(town.worktree, "deploy.pl"), true},
		{"new ruby", "Write", filepath.Join(town.worktree, "deploy.rb"), true},
		{"new sh uppercase suffix", "Write", filepath.Join(town.worktree, "deploy.SH"), true},
		{"new sh in a subdirectory", "Write", filepath.Join(town.worktree, "scripts", "deploy.sh"), true},
		{"relative new sh", "Write", "deploy.sh", true},
		{"edit creates a new sh", "Edit", filepath.Join(town.worktree, "deploy.sh"), true},
		{"multiedit creates a new sh", "MultiEdit", filepath.Join(town.worktree, "deploy.sh"), true},
		{"editing an existing script", "Edit", filepath.Join(town.worktree, "build.sh"), false},
		{"multiedit an existing script", "MultiEdit", filepath.Join(town.worktree, "build.sh"), false},
		{"rewriting an existing script", "Write", filepath.Join(town.worktree, "build.sh"), false},
		{"a non-script file", "Write", filepath.Join(town.worktree, "notes.txt"), false},
		{"a new python file", "Write", filepath.Join(town.worktree, "tool.py"), false},
		{"a script outside the worktree", "Write", filepath.Join(town.scratch, "scratch.sh"), false},
		{"a script in a sibling worktree", "Write", filepath.Join(town.sibling, "deploy.sh"), false},
		{"a new sh named through an unknown variable", "Write", "$UNKNOWN_DIR/deploy.sh", true},
		{"a notebook", "NotebookEdit", filepath.Join(town.worktree, "notes.ipynb"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := town.run(t, proc, town.worktree, tt.tool, fileInput(tt.target), io.Discard)
			assertVerdict(t, err, tt.blocked, tt.tool+" "+tt.target)
		})
	}
}

// TestScriptFileGuardDenialMessage pins the redirect: a denial without the Go
// instruction leaves the agent to invent another script.
func TestScriptFileGuardDenialMessage(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := town.run(t, town.polecatProc(), town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "deploy.sh")), &stderr)
	assertVerdict(t, err, true, "new deploy.sh")
	if !strings.Contains(stderr.String(), "NO NEW SCRIPTS") {
		t.Errorf("denial message missing the NO NEW SCRIPTS header, got:\n%s", stderr.String())
	}
	assertPolicyIn(t, stderr.String(), "denial message")
}

// TestScriptFileGuardPythonAdvisory pins the one allowed script-shaped file:
// Python is allowed but not silent, so the reason reaches the bead.
func TestScriptFileGuardPythonAdvisory(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := town.run(t, town.polecatProc(), town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "tool.py")), &stderr)
	assertVerdict(t, err, false, "new tool.py")
	if !strings.Contains(stderr.String(), "record in the bead") {
		t.Errorf("new Python file produced no advisory asking for the reason, got:\n%s", stderr.String())
	}
}

func TestScriptFileGuardBash(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.polecatProc()

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"touch", "touch deploy.sh", true},
		{"touch an existing script", "touch build.sh", false},
		{"touch a non-script", "touch notes.txt", false},
		{"touch past a flag value", "touch -r release.txt new.sh", true},
		{"touch whose flag value is a script", "touch -r missing.sh release.txt", false},
		{"tee", "tee gen.zsh", true},
		{"tee appending to an existing script", "tee -a build.sh", false},
		{"cp names a new script", "cp release.txt deploy.sh", true},
		{"cp onto an existing script", "cp release.txt build.sh", false},
		{"cp into a target directory", "cp -t bin deploy.sh", true},
		{"cp several sources into a directory", "cp --target-directory=bin release.txt deploy.sh", true},
		{"mv renames to a script", "mv release.txt deploy.sh", true},
		{"mv renames an existing script", "mv build.sh archive.sh", true},
		{"mv into a target directory", "mv -t bin build.sh", true},
		{"mv into a target directory under an existing name", "mv -t bin release.txt", false},
		{"ln creates a link named like a script", "ln -s release.txt deploy.sh", true},
		{"ln onto an existing link name", "ln -s release.txt build.sh", false},
		{"redirect to a non-script", "echo hi > notes.txt", false},
		{"redirect to a script", "printf x > setup.rb", true},
		{"glued redirect to a script", "echo hi>deploy.sh", true},
		{"clobber redirect to a script", "echo hi >| deploy.sh", true},
		{"descriptor redirect names no file", "echo hi 2>&1 >/dev/null", false},
		{"redirect from a heredoc", "cat <<'EOF' > gen.pl\nprint 1;\nEOF", true},
		{"heredoc redirect to a non-script", "cat <<'EOF' > notes.txt\nhi\nEOF", false},
		{"append to a new script", "echo hi >> setup.bash", true},
		{"install", "install -m 755 release.txt deploy.rb", true},
		{"install outside the worktree", "install -m 755 release.txt " + filepath.Join(town.scratch, "gt"), false},
		{"install into a target directory", "install -t bin deploy.sh", true},
		{"running an existing script", "bash build.sh", false},
		{"reading an existing script", "cat build.sh", false},
		{"removing a script", "rm -f deploy.sh", false},
		{"touch behind a launcher", "sudo touch deploy.sh", true},
		{"touch in a nested shell", "bash -c 'touch deploy.sh'", true},
		{"touch in an eval", "eval 'touch deploy.sh'", true},
		{"touch in a heredoc fed to a shell", "bash <<'EOF'\ntouch deploy.sh\nEOF", true},
		{"touch in a heredoc read in scratch", "cd " + town.scratch + " && bash <<'EOF'\ntouch deploy.sh\nEOF", false},
		{"touch in a subshell directory", "cd bin && touch deploy.sh", true},
		{"touch in scratch after a cd", "cd " + town.scratch + " && touch deploy.sh", false},
		{"redirect in scratch after a cd", "cd " + town.scratch + " && echo hi > notes.sh", false},
		{"relative redirect after a cd", "cd bin && echo hi > deploy.sh", true},
		{"relative redirect after a cd with a semicolon", "cd bin; echo hi > deploy.sh", true},
		{"relative redirect after a pushd", "pushd bin && echo hi > deploy.sh", true},
		{"relative redirect after a cd in scratch", "cd " + town.scratch + " && echo hi > deploy.sh", false},
		{"quoted greater-than is data", `echo "a>b"`, false},
		{"absolute redirect to a script", "echo hi > " + filepath.Join(town.worktree, "abs.sh"), true},
		{"a path the guard cannot resolve", "touch $UNKNOWN_DIR/x.sh", true},
		{"a non-script path the guard cannot resolve", "touch $UNKNOWN_DIR/notes.txt", false},
		{"a script after an unplaceable cd", "cd $UNKNOWN_DIR && touch x.sh", true},
		{"a runtime suffix with no suffix literal", "touch build.$EXT", false},
		{"a runtime suffix beside a suffix literal", "touch build.$EXT; echo .sh", true},
		{"touch -t takes a time, not a directory", "touch -t 202601010000 deploy.sh", true},
		{"touch -t on a non-script", "touch -t 202601010000 notes.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := town.run(t, proc, town.worktree, "Bash", commandInput(tt.command), io.Discard)
			assertVerdict(t, err, tt.blocked, tt.command)
		})
	}
}

// TestScriptFileGuardFailsClosedOnUnreadablePayload pins the gt-b43ra
// fail-closed rule: a payload the guard cannot read denies, and the denial
// carries the same redirect as a refused script, so the model is never left
// with a silent pass it can exploit by accident.
func TestScriptFileGuardFailsClosedOnUnreadablePayload(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := tapGuardScriptFiles(failingReader{}, &stderr, town.polecatProc())
	assertVerdict(t, err, true, "unreadable payload")
	assertPolicyIn(t, stderr.String(), "unreadable payload")
}

// TestScriptFileGuardFailsClosedOnUnparseablePayload pins the same rule for a
// payload that is not the hook JSON.
func TestScriptFileGuardFailsClosedOnUnparseablePayload(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := tapGuardScriptFiles(strings.NewReader("not a hook payload"), &stderr, town.polecatProc())
	assertVerdict(t, err, true, "unparseable payload")
	assertPolicyIn(t, stderr.String(), "unparseable payload")
}

// TestScriptFileGuardFailsClosedWhenWorkerTreeIsUnknown pins the scope half of
// the fail-closed rule: a worker session whose own worktree cannot be named is
// refused, because "I cannot see where you are" is not evidence of no script.
func TestScriptFileGuardFailsClosedWhenWorkerTreeIsUnknown(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/ace",
		"GT_POLECAT": "ace",
	}, "")
	var stderr strings.Builder
	// The payload cwd is outside any worker worktree and the proc names no
	// GT_POLECAT_PATH, so the guard's own tree cannot be resolved.
	err := town.run(t, proc, town.scratch, "Write", fileInput("deploy.sh"), &stderr)
	assertVerdict(t, err, true, "unresolvable worker tree")
	assertPolicyIn(t, stderr.String(), "unresolvable worker tree")
}

// assertPolicyIn checks a denial carries every line of the guarded redirect.
// The printer indents each line, so the lines are checked one at a time.
func assertPolicyIn(t *testing.T, stderr, label string) {
	t.Helper()
	for _, line := range strings.Split(scriptFilePolicy, "\n") {
		if !strings.Contains(stderr, line) {
			t.Errorf("%s: denial does not carry %q from scriptFilePolicy, got:\n%s", label, line, stderr)
		}
	}
}

// TestScriptFileGuardNonWorkerSessionAllowed pins the scope: a session that is
// neither a polecat nor crew is never judged, even inside a worker's tree.
// A session with no identifying variable is judged as a worker: the guard is
// installed only in worker and crew settings, so an unidentified session fails
// closed rather than allowed (gt-b43ra).
func TestScriptFileGuardNoIdentitySessionIsJudged(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := fakeGuardProcess(map[string]string{}, "")
	err := town.run(t, proc, town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "deploy.sh")), io.Discard)
	assertVerdict(t, err, true, "unidentified Write of a new script")
}

// TestScriptFileGuardCrewSession pins the crew half of the registration: a
// crew member's checkout is found from cwd, with no worktree variable.
func TestScriptFileGuardCrewSession(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.crewProc()
	err := town.run(t, proc, town.crew, "Write",
		fileInput(filepath.Join(town.crew, "deploy.sh")), io.Discard)
	assertVerdict(t, err, true, "crew Write of a new script")

	// A polecat worktree is not the crew member's tree, so it is not judged.
	err = town.run(t, proc, town.crew, "Write",
		fileInput(filepath.Join(town.worktree, "deploy.sh")), io.Discard)
	assertVerdict(t, err, false, "crew Write into a polecat worktree")
}

// TestScriptFileGuardNoImplicitAllowlist pins by behaviour the gt-b43ra
// constraint that the allowlist starts empty: a new script is refused whatever
// plausible repo name it carries, so nothing is exempt without an explicit
// entry that a reviewer can see.
func TestScriptFileGuardNoImplicitAllowlist(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.polecatProc()
	for _, name := range []string{
		"scripts/release.sh",
		"build.sh.bak.sh",
		"gen/assemble.bash",
		"hooks/pre-commit.zsh",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := town.run(t, proc, town.worktree, "Write",
				fileInput(filepath.Join(town.worktree, name)), io.Discard)
			assertVerdict(t, err, true, "new script named "+name)
		})
	}
}

// TestScriptFileGuardMonitorAndCrewBash pins the two Bash paths the table above
// does not cover: a Monitor command is judged like a Bash one, and a crew
// member's Bash is judged in the crew checkout (gt-b43ra).
func TestScriptFileGuardMonitorAndCrewBash(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	cases := []struct {
		name    string
		proc    guardProcess
		cwd     string
		tool    string
		command string
		blocked bool
	}{
		{"monitor redirect to a script", town.polecatProc(), town.worktree, "Monitor", "echo hi > watch.sh", true},
		{"monitor read of a script", town.polecatProc(), town.worktree, "Monitor", "cat build.sh", false},
		{"crew redirect to a script", town.crewProc(), town.crew, "Bash", "echo hi > deploy.sh", true},
		{"crew touch of a non-script", town.crewProc(), town.crew, "Bash", "touch notes.txt", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := town.run(t, tt.proc, tt.cwd, tt.tool, commandInput(tt.command), io.Discard)
			assertVerdict(t, err, tt.blocked, tt.command)
		})
	}
}
