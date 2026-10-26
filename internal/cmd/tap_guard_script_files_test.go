package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

// scriptFileTestTown is a hermetic Gas Town layout for the script-files guard:
//
//	<root>/gt/<rig>/polecats/<name>/<repo>    a polecat's worktree
//	<root>/gt/<rig>/polecats/<other>/<repo>   a sibling polecat's worktree
//	<root>/gt/<rig>/crew/<member>             a crew member's worktree
//	<root>/scratch                            scratch outside every worktree
//
// The polecat worktree seeds "build.sh", a script the guard must let the
// session edit.
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
	// A dangling symlink: its target is missing, so writing it creates one.
	if err := os.Symlink(filepath.Join(worktree, "missing-target"), filepath.Join(worktree, "ghost.sh")); err != nil {
		t.Fatalf("seeding a dangling symlink: %v", err)
	}
	// A dangling link whose target is a script, and a link to an existing file.
	if err := os.Symlink(filepath.Join(worktree, "x.sh"), filepath.Join(worktree, "alias.txt")); err != nil {
		t.Fatalf("seeding a dangling link to a script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "readme.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatalf("seeding readme.txt: %v", err)
	}
	if err := os.Symlink(filepath.Join(worktree, "readme.txt"), filepath.Join(worktree, "readme-link")); err != nil {
		t.Fatalf("seeding a link to readme.txt: %v", err)
	}
	// The seeded scripts carry no shebang: the guard judges a path by its
	// existence and suffix, and a shebang literal in a test file is itself a
	// no-exec-files violation (internal/testpolicy).
	for _, file := range []struct{ path, content string }{
		{filepath.Join(worktree, "build.sh"), "go build ./...\n"},
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
	return town.bounded(fakeGuardProcess(map[string]string{
		"GT_ROLE":         "gastown/polecats/ace",
		"GT_RIG":          "gastown",
		"GT_POLECAT":      "ace",
		"GT_POLECAT_PATH": town.worktree,
	}, ""))
}

// bounded stops the checkout walk at the fake root, so a test never reads the
// real filesystem above its temporary directory (gt-b43ra).
func (town scriptFileTestTown) bounded(proc guardProcess) guardProcess {
	proc.checkoutBound = town.root
	return proc
}

// crewProc is a crew session in town's crew checkout. Crew carry no worktree
// variable: the guard finds the checkout from cwd.
func (town scriptFileTestTown) crewProc() guardProcess {
	return town.bounded(fakeGuardProcess(map[string]string{
		"GT_ROLE": "gastown/crew/alice",
		"GT_RIG":  "gastown",
		"GT_CREW": "alice",
	}, ""))
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
		{"writing a dangling symlink named as a script", "Write", filepath.Join(town.worktree, "ghost.sh"), true},
		{"writing a dangling symlink to a script", "Write", filepath.Join(town.worktree, "alias.txt"), true},
		{"writing a symlink to an existing non-script", "Write", filepath.Join(town.worktree, "readme-link"), false},
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

// An unreadable payload denies when it names a denied script suffix, with the
// same redirect as a refused script; an unreadable payload that names none is
// allowed, because no script is named in it (gt-b43ra).
func TestScriptFileGuardUnreadablePayload(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := tapGuardScriptFiles(failingReader{}, &stderr, town.polecatProc())
	assertVerdict(t, err, false, "unreadable payload naming no script")

	stderr.Reset()
	err = tapGuardScriptFiles(strings.NewReader(`{"tool_input":{"file_path":"deploy.sh"`), &stderr, town.polecatProc())
	assertVerdict(t, err, true, "unparseable payload naming a script")
	assertPolicyIn(t, stderr.String(), "unparseable payload naming a script")
}

// An unparseable payload denies on a script suffix it names and is allowed
// otherwise (gt-b43ra).
func TestScriptFileGuardUnparseablePayload(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	var stderr strings.Builder
	err := tapGuardScriptFiles(strings.NewReader("not a hook payload"), &stderr, town.polecatProc())
	assertVerdict(t, err, false, "unparseable payload naming no script")

	// The suffix match ignores case: an upper-case script name is a script.
	stderr.Reset()
	err = tapGuardScriptFiles(strings.NewReader(`{"tool_input":{"file_path":"DEPLOY.SH"`), &stderr, town.polecatProc())
	assertVerdict(t, err, true, "unparseable payload naming an upper-case script")
}

// A worker session whose own worktree cannot be placed denies a file-writing
// call whose target names a denied script suffix, and allows every other call:
// the guard cannot see where the session stands, so it denies only what it can
// name (gt-b43ra).
func TestScriptFileGuardUnresolvedWorktreeDeniesOnlyScriptTargets(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.bounded(fakeGuardProcess(map[string]string{
		"GT_ROLE":    "gastown/polecats/ace",
		"GT_POLECAT": "ace",
	}, ""))
	// The payload cwd is outside any worker worktree and the proc names no
	// GT_POLECAT_PATH, so the guard's own tree cannot be resolved.
	var stderr strings.Builder
	err := town.run(t, proc, town.scratch, "Write", fileInput("deploy.sh"), &stderr)
	assertVerdict(t, err, true, "unresolvable worker tree, script target")
	assertPolicyIn(t, stderr.String(), "unresolvable worker tree, script target")

	stderr.Reset()
	err = town.run(t, proc, town.scratch, "Write", fileInput("notes.txt"), &stderr)
	assertVerdict(t, err, false, "unresolvable worker tree, non-script target")
	if !strings.Contains(stderr.String(), "was not judged for new scripts") {
		t.Errorf("unresolved worktree write printed no warning: %q", stderr.String())
	}
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

// TestScriptFileGuardNoIdentitySessionIsJudged pins the no-identity rule: a
// session with no identifying variable is judged as a worker, because the guard
// is installed only in worker and crew settings, so an unidentified session
// fails closed rather than allowed (gt-b43ra).
func TestScriptFileGuardNoIdentitySessionIsJudged(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.bounded(fakeGuardProcess(map[string]string{}, ""))
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

// TestScriptFileGuardNoImplicitAllowlist pins by behavior that no name exempts a
// new script: a new script is refused whatever plausible repo name it carries
// (gt-b43ra).
// TestScriptFileGuardIsRegistered pins the cobra registration and the hook
// config registration: removing either fails here (gt-b43ra).
func TestScriptFileGuardIsRegistered(t *testing.T) {
	t.Parallel()
	c, _, err := rootCmd.Find([]string{"tap", "guard", "script-files"})
	if err != nil || c == nil || c.Name() != "script-files" {
		t.Fatalf("gt tap guard script-files is not registered: %v", err)
	}
	for role, cfg := range hooks.DefaultOverrides() {
		if role != "polecats" && role != "crew" {
			continue
		}
		listed := false
		for _, entry := range cfg.PreToolUse {
			for _, h := range entry.Hooks {
				if strings.Contains(h.Command, "tap guard script-files") {
					listed = true
				}
			}
		}
		if !listed {
			t.Errorf("%s hook config does not list the script-files guard", role)
		}
	}
}

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

// The guard judges file tools only. Bash-created scripts are to be caught at
// commit by the planned repo guard gt-lpm8b, which has not landed, so a Bash
// command is not judged here (gt-b43ra).
func TestScriptFileGuardBashIsNotJudged(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	err := town.run(t, town.polecatProc(), town.worktree, "Bash",
		commandInput("touch deploy.sh"), io.Discard)
	assertVerdict(t, err, false, "Bash touch of a new script")
}

// An unrecognized GT_ROLE does not exempt the session: it is judged as a
// worker, so a new script in its worktree is denied (gt-b43ra).
func TestScriptFileGuardUnrecognizedRoleIsJudged(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.bounded(fakeGuardProcess(map[string]string{
		"GT_ROLE": "gastown/witness/ace",
	}, ""))
	err := town.run(t, proc, town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "deploy.sh")), io.Discard)
	assertVerdict(t, err, true, "unrecognized GT_ROLE Write of a new script")
}

// A symlink chain longer than the guard follows is judged by the names on it:
// a script name on the way denies, and a chain of plain names is allowed. The
// chains live in the test's own temporary worktree (gt-b43ra).
func TestScriptFileGuardLongSymlinkChain(t *testing.T) {
	t.Parallel()
	town := newScriptFileTestTown(t)
	proc := town.polecatProc()

	// hops builds a chain of n links from first to a missing final name, with
	// each link's name taken from names (or a name derived from first).
	hops := func(first string, n int, names map[int]string) {
		t.Helper()
		name := func(i int) string {
			if v, ok := names[i]; ok {
				return v
			}
			return fmt.Sprintf("%s-link%d.txt", strings.TrimSuffix(first, ".txt"), i)
		}
		for i := 0; i < n; i++ {
			from := filepath.Join(town.worktree, name(i))
			if i == 0 {
				from = filepath.Join(town.worktree, first)
			}
			to := filepath.Join(town.worktree, name(i+1))
			if i+1 == n {
				to = filepath.Join(town.worktree, "missing-final.txt")
			}
			if err := os.Symlink(to, from); err != nil {
				t.Fatalf("linking %s: %v", from, err)
			}
		}
	}

	hops("plain-head.txt", 40, nil)
	err := town.run(t, proc, town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "plain-head.txt")), io.Discard)
	assertVerdict(t, err, false, "a long chain of plain names")

	hops("script-head.txt", 40, map[int]string{3: "hidden.sh"})
	err = town.run(t, proc, town.worktree, "Write",
		fileInput(filepath.Join(town.worktree, "script-head.txt")), io.Discard)
	assertVerdict(t, err, true, "a long chain with a script name on the way")
}
