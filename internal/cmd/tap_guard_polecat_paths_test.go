package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// polecatTestTown is a hermetic Gas Town layout for the polecat-paths guard:
//
//	<root>/gt/<rig>/polecats/<name>/<repo>   this polecat's worktree
//	<root>/gt/<rig>/polecats/<other>/<repo>  a sibling polecat's worktree
//	<root>/gt/<rig>/.repo.git                the rig's bare repo
//	<root>/gt/<rig>/crew/alice               another agent's workspace
//	<root>/gt/mayor, deacon, settings        town state a polecat must not touch
//	<root>/gt/.claude-town/projects          the session scratchpad
//
// <root>/gt/.claude-town is also $CLAUDE_CONFIG_DIR, as it is in production,
// so the config-dir cases below exercise the tree the guard really sees.
//
// Every path the guard sees (the payload cwd, GT_POLECAT_PATH, HOME) is under
// the test's own temp root, so the block cases below hold on a clean machine
// and never touch the operator's live tree — the previous attempt's tests
// hardcoded /Users/sloan/gt and only "passed" by accident of that box's layout.
type polecatTestTown struct {
	root     string
	town     string
	rig      string
	rigRoot  string
	name     string
	other    string
	worktree string
	sibling  string
	repoGit  string
}

func newPolecatTestTown(t *testing.T) polecatTestTown {
	t.Helper()
	root := t.TempDir()
	rig, name, other := "gastown", "ruby", "coral"
	town := filepath.Join(root, "gt")
	rigRoot := filepath.Join(town, rig)
	worktree := filepath.Join(rigRoot, "polecats", name, rig)
	sibling := filepath.Join(rigRoot, "polecats", other, rig)

	for _, dir := range []string{
		worktree,
		sibling,
		filepath.Join(rigRoot, ".repo.git"),
		filepath.Join(rigRoot, "crew", "alice"),
		filepath.Join(town, "mayor"),
		filepath.Join(town, "deacon"),
		filepath.Join(town, "settings"),
		filepath.Join(town, ".claude-town", "projects"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	// A file in the sibling worktree, so the cases below cover paths that exist
	// (Edit's usual shape) as well as paths that do not exist yet (Write's).
	err := os.WriteFile(filepath.Join(sibling, "internal.go"), []byte("package x\n"), 0o644)
	if err != nil {
		t.Fatalf("seeding the sibling worktree: %v", err)
	}

	t.Setenv("HOME", root) // ~ expands inside the test root, not the operator's home
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatalf("creating the temp root: %v", err)
	}
	t.Setenv("TMPDIR", filepath.Join(root, "tmp")) // $TMPDIR and /tmp-adjacent rules stay hermetic
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("GT_ROOT", town)
	t.Setenv("GT_RIG", rig)
	t.Setenv("GT_POLECAT", name)
	t.Setenv("GT_POLECAT_PATH", worktree)
	// $CLAUDE_CONFIG_DIR is the town's own config tree, exactly as production
	// sets it — inside the town, which is also what makes the Bash leg judge it
	// (that leg only polices targets inside the town).
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(town, ".claude-town"))

	return polecatTestTown{
		root:     root,
		town:     town,
		rig:      rig,
		rigRoot:  rigRoot,
		name:     name,
		other:    other,
		worktree: worktree,
		sibling:  sibling,
		repoGit:  filepath.Join(rigRoot, ".repo.git"),
	}
}

// run invokes the guard exactly as the PreToolUse hook does: a Claude Code
// payload on stdin, the guard's exit status as the verdict.
func (p polecatTestTown) run(t *testing.T, tool, toolInput string) error {
	t.Helper()
	payload := fmt.Sprintf(`{"tool_name":%q,"cwd":%q,"tool_input":%s}`, tool, p.worktree, toolInput)
	var err error
	withStdin(t, payload, func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	return err
}

func fileInput(path string) string {
	return fmt.Sprintf(`{"file_path":%q}`, path)
}

func notebookInput(path string) string {
	return fmt.Sprintf(`{"notebook_path":%q}`, path)
}

func commandInput(command string) string {
	return fmt.Sprintf(`{"command":%q}`, command)
}

// TestRunTapGuardPolecatPaths_BlocksLiveHookPayload is the live-block leg: the
// guard must actually exit non-nil (exit 2) for the incident's shape —
// an Edit whose file_path is inside a sibling polecat's worktree — driven
// through the real stdin payload the hook sends, not by calling internals.
func TestRunTapGuardPolecatPaths_BlocksLiveHookPayload(t *testing.T) {
	p := newPolecatTestTown(t)
	err := p.run(t, "Edit", fileInput(filepath.Join(p.sibling, "internal.go")))
	if err == nil {
		t.Fatalf("expected an Edit inside %s/polecats/%s/ to be blocked, got nil error", p.rig, p.other)
	}
}

// TestRunTapGuardPolecatPaths_BlocksSharedBinDir is the live-block leg for
// gt-tnts5: obsidian, a local-coder polecat, overwrote the production bd
// binary at $HOME/.local/bin/bd with a Bash echo, causing a town-wide bd
// outage. $HOME/.local/bin sits outside the town tree, so it must be blocked
// on its own rule rather than the town-membership check that protects
// everything else — driven through the real stdin payload, not internals.
func TestRunTapGuardPolecatPaths_BlocksSharedBinDir(t *testing.T) {
	p := newPolecatTestTown(t)
	target := filepath.Join(p.root, ".local", "bin", "bd")
	err := p.run(t, "Bash", commandInput("echo 'echo STUB-RAN' > "+target))
	if err == nil {
		t.Fatalf("expected a write to %s (the shared bd install path) to be blocked, got nil error", target)
	}
}

// TestPolecatPathGuardFileTargets covers the Edit/Write/MultiEdit/NotebookEdit
// leg: only the polecat's own worktree, temp directories and the session
// scratchpad are writable.
func TestPolecatPathGuardFileTargets(t *testing.T) {
	p := newPolecatTestTown(t)
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")

	cases := []struct {
		name      string
		tool      string
		input     string
		wantBlock bool
	}{
		{"own worktree, existing file", "Edit", fileInput(filepath.Join(p.worktree, "internal", "cmd", "tap.go")), false},
		{"own worktree, new file", "Write", fileInput(filepath.Join(p.worktree, "internal", "cmd", "new.go")), false},
		{"own worktree, relative path", "Write", fileInput("internal/cmd/relative.go"), false},
		{"own worktree, dot-dot that stays inside", "Write", fileInput(p.worktree + "/internal/../x.go"), false},
		{"sibling worktree, existing file", "Edit", fileInput(filepath.Join(p.sibling, "internal.go")), true},
		{"sibling worktree, new file", "Write", fileInput(filepath.Join(p.sibling, "brand", "new.go")), true},
		{"sibling worktree via tilde", "Write", fileInput("~/gt/" + p.rig + "/polecats/" + p.other + "/" + p.rig + "/x.go"), true},
		{"sibling worktree via relative escape", "Write", fileInput("../../../" + p.other + "/" + p.rig + "/x.go"), true},
		// The session's own config dir is writable in exactly the subdirectories
		// Claude Code keeps its own state in — plan mode's plans/, transcription
		// and memory's projects/, todos/ — never as a whole tree (gt-ovo1).
		{"CLAUDE_CONFIG_DIR/plans", "Write", fileInput(filepath.Join(configDir, "plans", "x.md")), false},
		{"CLAUDE_CONFIG_DIR/todos", "Write", fileInput(filepath.Join(configDir, "todos", "x.md")), false},
		{"CLAUDE_CONFIG_DIR/projects", "Write", fileInput(filepath.Join(configDir, "projects", "x.md")), false},
		{"CLAUDE_CONFIG_DIR session transcripts", "Write", fileInput(filepath.Join(configDir, "projects", "-Users-sloan-gt", "sess-id", "memory", "note.md")), false},
		// ...and the rest of it is live harness config: deny (gt-ovo1).
		{"CLAUDE_CONFIG_DIR/settings.json", "Write", fileInput(filepath.Join(configDir, "settings.json")), true},
		{"CLAUDE_CONFIG_DIR/settings.json, Edit", "Edit", fileInput(filepath.Join(configDir, "settings.json")), true},
		{"CLAUDE_CONFIG_DIR/.claude.json", "Write", fileInput(filepath.Join(configDir, ".claude.json")), true},
		{"CLAUDE_CONFIG_DIR/session-env", "Write", fileInput(filepath.Join(configDir, "session-env", "sess-id", "env.json")), true},
		{"CLAUDE_CONFIG_DIR/shell-snapshots", "Write", fileInput(filepath.Join(configDir, "shell-snapshots", "snap.sh")), true},
		{"CLAUDE_CONFIG_DIR/history.jsonl", "Write", fileInput(filepath.Join(configDir, "history.jsonl")), true},
		{"CLAUDE_CONFIG_DIR/plugins", "Write", fileInput(filepath.Join(configDir, "plugins", "installed.json")), true},
		{"own rig root", "Write", fileInput(filepath.Join(p.rigRoot, "rig.json")), true},
		{"town mayor dir", "Write", fileInput(filepath.Join(p.town, "mayor", "proposal.md")), true},
		{"town deacon dir", "Edit", fileInput(filepath.Join(p.town, "deacon", "state.json")), true},
		{"town settings dir", "Write", fileInput(filepath.Join(p.town, "settings", "x.json")), true},
		{"own polecat dir but outside worktree", "Write", fileInput(filepath.Join(p.rigRoot, "polecats", p.name, ".claude", "settings.json")), true},
		{"another agent's workspace", "Write", fileInput(filepath.Join(p.rigRoot, "crew", "alice", "x.go")), true},
		{"shared install dir, outside the town tree", "Write", fileInput(filepath.Join(p.root, ".local", "bin", "bd")), true},
		{"shared install dir via tilde", "Write", fileInput("~/.local/bin/bd"), true},
		{"tmp", "Write", fileInput("/tmp/polecat-paths-probe/x.go"), false},
		{"session scratchpad", "Write", fileInput(filepath.Join(p.town, ".claude-town", "projects", "p", "notes.md")), false},
		{"notebook in sibling worktree", "NotebookEdit", notebookInput(filepath.Join(p.sibling, "nb.ipynb")), true},
		{"notebook in own worktree", "NotebookEdit", notebookInput(filepath.Join(p.worktree, "nb.ipynb")), false},
		{"absolute sibling path with a trailing slash", "Write", fileInput(filepath.Join(p.sibling) + "/"), true},
		{"prefix lookalike directory", "Write", fileInput(p.worktree + "-evil/x.go"), true},
		// Fail closed: the guard must not guess when a path cannot be resolved.
		{"unknown variable", "Write", fileInput("$POLEKAT_PATHS_UNSET/x.go"), true},
		{"command substitution", "Write", fileInput("$(mktemp -d)/x.go"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.run(t, tc.tool, tc.input)
			if tc.wantBlock && err == nil {
				t.Errorf("%s: expected BLOCK, got allow", tc.name)
			}
			if !tc.wantBlock && err != nil {
				t.Errorf("%s: expected allow, got block: %v", tc.name, err)
			}
		})
	}
}

// TestPolecatPathGuardFollowsSymlinks pins the EvalSymlinks leg: a symlink
// created inside the worktree that points at a sibling's worktree must not be
// a way in.
func TestPolecatPathGuardFollowsSymlinks(t *testing.T) {
	p := newPolecatTestTown(t)
	link := filepath.Join(p.worktree, "escape")
	if err := os.Symlink(p.sibling, link); err != nil {
		t.Fatalf("creating symlink: %v", err)
	}
	if err := p.run(t, "Write", fileInput(filepath.Join(link, "internal.go"))); err == nil {
		t.Error("expected a write through a symlink into a sibling worktree to be blocked")
	}
	if err := p.run(t, "Write", fileInput(filepath.Join(link, "new.go"))); err == nil {
		t.Error("expected a new file written through that symlink to be blocked (fail closed)")
	}
}

// TestClaudeConfigScratchRoots pins the gt-ovo1 rule for the config dir: those
// roots are the config dir's three state subdirectories, and never the config
// dir itself, which is where the live harness config lives (settings.json,
// .claude.json, session-env/).
//
// The bogus values matter. "/" would allowlist the filesystem root's own
// plans/, projects/ and todos/; $HOME would do the same to the operator's home
// tree, next to ~/.ssh. A mis-set environment must yield no roots at all rather
// than a wide one.
func TestClaudeConfigScratchRoots(t *testing.T) {
	const home = "/town/home"
	cases := []struct {
		name      string
		configDir string
		want      []string
	}{
		{"unset", "", nil},
		{"relative", "relative-config", nil},
		{"filesystem root", "/", nil},
		{"home directory", home, nil},
		{"home directory with a trailing slash", home + "/", nil},
		{"ancestor of the home directory", "/town", nil},
		{"town config dir", "/town/.claude-town", []string{
			"/town/.claude-town/plans",
			"/town/.claude-town/projects",
			"/town/.claude-town/todos",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeConfigScratchRoots(tc.configDir, home)
			if len(got) != len(tc.want) {
				t.Fatalf("claudeConfigScratchRoots(%q) = %v, want %v", tc.configDir, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("claudeConfigScratchRoots(%q) = %v, want %v", tc.configDir, got, tc.want)
				}
			}
		})
	}
}

// TestPolecatPathGuardUnexpectedConfigDirFailsClosed drives the same rule
// through the guard: with $HOME as the config dir, a write to $HOME/plans/ —
// which the bare config dir used to allowlist along with everything beside it —
// must be denied, while the session's ordinary scratch space stays writable.
func TestPolecatPathGuardUnexpectedConfigDirFailsClosed(t *testing.T) {
	p := newPolecatTestTown(t)
	t.Setenv("CLAUDE_CONFIG_DIR", p.root) // the test's $HOME, a config dir shaped wrong
	err := p.run(t, "Write", fileInput(filepath.Join(p.root, "plans", "x.md")))
	if err == nil {
		t.Errorf("expected a write to $HOME/plans/ to be blocked when CLAUDE_CONFIG_DIR=$HOME, got allow")
	}
	// Failing closed on the config dir must not wedge the session: the worktree
	// and the town's own scratch dir are allowed independently of it.
	for _, target := range []string{
		filepath.Join(p.worktree, "internal", "cmd", "x.go"),
		filepath.Join(p.town, ".claude-town", "projects", "p", "notes.md"),
	} {
		if err := p.run(t, "Write", fileInput(target)); err != nil {
			t.Errorf("expected %s to stay writable, got block: %v", target, err)
		}
	}
}

// TestPolecatPathGuardNoOpOutsidePolecatContext is the false-positive leg: the
// guard is registered on the polecats settings, but it must stay silent for any
// session that is not a polecat's (and for the same call once the polecat env
// is gone).
func TestPolecatPathGuardNoOpOutsidePolecatContext(t *testing.T) {
	p := newPolecatTestTown(t)
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_POLECAT_PATH", "")
	err := p.run(t, "Edit", fileInput(filepath.Join(p.sibling, "internal.go")))
	if err != nil {
		t.Errorf("expected no-op outside a polecat session, got block: %v", err)
	}
}

// TestPolecatPathGuardBash covers the Bash leg in one table: writes aimed at
// another agent's tree are blocked, ordinary polecat work is not.
func TestPolecatPathGuardBash(t *testing.T) {
	p := newPolecatTestTown(t)
	sib := filepath.Join(p.sibling, "x.go")
	town := p.town
	mayor := filepath.Join(town, "mayor", "f")
	deacon := filepath.Join(town, "deacon", "f")
	settings := filepath.Join(town, "settings", "f")
	crew := filepath.Join(p.rigRoot, "crew", "alice", "x.go")
	own := filepath.Join(p.worktree, "x.go")

	cases := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{"cp into sibling worktree", "cp a.go " + sib, true},
		{"cp into a sibling that does not exist yet", "cp a.go " + filepath.Join(p.sibling, "new", "x.go"), true},
		{"mv into sibling worktree", "mv a.go " + sib, true},
		{"rm inside sibling worktree", "rm -rf " + filepath.Join(p.sibling, "internal.go"), true},
		{"mkdir in sibling worktree", "mkdir -p " + filepath.Join(p.sibling, "new"), true},
		{"tee into sibling worktree", "echo hi | tee " + sib, true},
		{"touch in town mayor dir", "touch " + mayor, true},
		{"rm -rf town settings dir", "rm -rf " + filepath.Join(town, "settings"), true},
		{"chmod in another agent workspace", "chmod 600 " + crew, true},
		{"cp into own rig root", "cp a.go " + filepath.Join(p.rigRoot, "rig.json"), true},
		{"cp to a sibling via tilde", "cp a.go ~/gt/" + p.rig + "/polecats/" + p.other + "/" + p.rig + "/x.go", true},
		{"cp to a sibling via $HOME", "cp a.go $HOME/gt/" + p.rig + "/polecats/" + p.other + "/" + p.rig + "/x.go", true},
		{"cp to a sibling via a relative escape", "cp a.go ../../../" + p.other + "/" + p.rig + "/x.go", true},
		{"cp to a sibling via $GT_POLECAT_PATH sibling", "cp a.go $GT_TOWN_ROOT/" + p.rig + "/polecats/" + p.other + "/" + p.rig + "/x.go", true},
		{"cp with --target-directory into a sibling", "cp --target-directory=" + p.sibling + " a.go", true},
		{"dd of= into a sibling", "dd if=/dev/zero of=" + sib, true},
		// gt-tnts5: the shared gt/bd install dir sits outside the town tree, so
		// it needs its own always-on rule rather than the town-membership check.
		{"echo stub over the shared bd binary", "echo 'echo STUB-RAN' > " + filepath.Join(p.root, ".local", "bin", "bd"), true},
		{"cp over the shared bd binary", "cp backup-bd " + filepath.Join(p.root, ".local", "bin", "bd"), true},
		{"mkdir the shared install dir", "mkdir -p " + filepath.Join(p.root, ".local", "bin"), true},
		{"shared install dir via tilde", "cp a.go ~/.local/bin/bd", true},
		{"shared install dir via $HOME", "cp a.go $HOME/.local/bin/bd", true},
		{"read from the shared install dir is allowed", "cat " + filepath.Join(p.root, ".local", "bin", "bd"), false},
		// Interpreters and downloaders are write-capable: the previous
		// attempt's guard whitelisted them as read-only.
		{"python3 -c opening a sibling file for write", `python3 -c "open('` + sib + `','w').write('x')"`, true},
		{"python3 script argument in a sibling", "python3 " + filepath.Join(p.sibling, "script.py"), true},
		{"node -e writing into a sibling", `node -e "require('fs').writeFileSync('` + sib + `','x')"`, true},
		{"sh -c writing into a sibling", `sh -c "cp a.go ` + sib + `"`, true},
		{"perl -e writing into a sibling", `perl -e "open(F,'>','` + sib + `')"`, true},
		{"curl -o into a sibling", "curl -o " + sib + " https://example.com/x", true},
		{"curl into a sibling by URL path", "curl -o " + filepath.Join(p.town, "mayor", "x") + " https://example.com/x", true},
		{"wget -O into a sibling", "wget -O " + sib + " https://example.com/x", true},
		// Redirections are write targets whatever the command word is.
		{"echo into a sibling worktree", "echo hi > " + sib, true},
		{"append into a sibling worktree", "echo hi >> " + sib, true},
		{"redirect glued to the target", "echo hi>" + sib, true},
		{"redirect into the town mayor dir", "git hook > " + mayor, true},
		{"redirect into town deacon", "echo x > " + deacon, true},
		{"redirect into town settings", "echo x > " + settings, true},
		// cd decides where later relative paths land.
		{"cd into a sibling worktree", "cd " + p.sibling + " && ls", true},
		{"cd to the town root", "cd " + town + " && ls", true},
		// git -C reaches another tree without naming a path anywhere else.
		{"git -C into a sibling worktree", "git -C " + p.sibling + " checkout -- .", true},
		{"git -C into the town mayor dir", "git -C " + filepath.Join(town, "mayor") + " status", true},
		// Command substitutions run as real commands.
		{"writing inside a command substitution", `echo "$(cp a.go ` + sib + `)"`, true},
		{"writing inside backticks", "echo `cp a.go " + sib + "`", true},
		// Fail closed when a checked target cannot be resolved.
		{"unresolvable variable in a write", "cp a.go $POLECAT_PATHS_UNSET/x.go", true},
		{"unresolvable variable as a redirect target", "echo hi > $POLECAT_PATHS_UNSET/x.go", true},

		// --- allowed ---
		{"read-only command in a sibling worktree", "grep -rn TODO " + p.sibling, false},
		{"read-only command in a town dir", "ls -la " + filepath.Join(town, "mayor"), false},
		{"cat a sibling's file", "cat " + filepath.Join(p.sibling, "internal.go"), false},
		{"grep a sibling through a path pattern", "grep -rn 'func main' " + filepath.Join(p.sibling, "*.go"), false},
		{"build in the worktree", "make build", false},
		{"scoped tests", "GOFLAGS=-p=6 go test ./internal/cmd/...", false},
		{"cd own worktree then build", "cd " + p.worktree + " && go build ./...", false},
		{"write inside own worktree", "cp a.go " + own, false},
		{"rm a glob inside own worktree", "rm -rf " + filepath.Join(p.worktree, "tmp") + "/*", false},
		{"mkdir inside own worktree", "mkdir -p " + filepath.Join(p.worktree, "internal", "new"), false},
		{"write to /tmp", "cp a.go /tmp/polecat-paths-probe/x.go", false},
		{"write to $TMPDIR", "cp a.go $TMPDIR/polecat-paths-probe/x.go", false},
		{"write to $CLAUDE_CONFIG_DIR/plans", "cp a.go $CLAUDE_CONFIG_DIR/plans/x.md", false},
		{"write to $CLAUDE_CONFIG_DIR/settings.json", "cp a.go $CLAUDE_CONFIG_DIR/settings.json", true},
		{"redirect into $CLAUDE_CONFIG_DIR/.claude.json", "echo x > $CLAUDE_CONFIG_DIR/.claude.json", true},
		{"write to /dev/null", "make build > /dev/null 2>&1", false},
		{"file descriptor duplication", "make build 2>&1", false},
		{"own rig's .repo.git", "git -C " + p.repoGit + " worktree list", false},
		{"write inside own rig's .repo.git", "cp a.go " + p.repoGit + "/x", false},
		{"own polecat dir", "mkdir -p " + filepath.Join(p.rigRoot, "polecats", p.name, "scratch"), false},
		{"gt command", "gt hook", false},
		{"gt mail with a body mentioning a sibling", `gt mail send gastown/witness -s "HELP" -m "I edited ` + sib + `"`, false},
		{"bd command", "bd show gt-hmaf", false},
		{"git commit message mentioning a sibling path", `git commit -m "fix(x): touches nothing in ` + sib + `"`, false},
		{"git add and commit in the worktree", "git add internal/cmd/x.go && git commit -m 'feat: x (gt-hmaf)'", false},
		{"curl to stdout", "curl -s https://example.com/x", false},
		{"curl -o in /tmp", "curl -o /tmp/polecat-paths-probe/x https://example.com/x", false},
		{"sed reading a sibling", "sed -n '1,10p' " + filepath.Join(p.sibling, "internal.go"), false},
		{"python with no paths", `python3 -c "print(1)"`, false},
		{"heredoc body into own worktree", "cat > " + own + " <<'EOF'\nsee " + sib + " for context\nEOF", false},
		{"heredoc body mentioning the mayor dir", "cat > " + own + " <<'EOF'\n" + mayor + "\nEOF", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := p.run(t, "Bash", commandInput(tc.command))
			if tc.wantBlock && err == nil {
				t.Errorf("command %q: expected BLOCK, got allow", tc.command)
			}
			if !tc.wantBlock && err != nil {
				t.Errorf("command %q: expected allow, got block: %v", tc.command, err)
			}
		})
	}
}

// TestPolecatPathGuardMonitorSameAsBash pins gt-vx2mm: the Monitor tool
// carries the same tool_input.command shape as Bash (a shell command in
// tool_input.command run as a background/streaming watch), and this guard's
// switch on tool_name must treat it identically — a polecat should not be
// able to reach a sibling's worktree just by routing the same command
// through Monitor instead of Bash.
func TestPolecatPathGuardMonitorSameAsBash(t *testing.T) {
	p := newPolecatTestTown(t)
	blocked := "rm -rf " + filepath.Join(p.sibling, "internal.go")
	if err := p.run(t, "Monitor", commandInput(blocked)); err == nil {
		t.Errorf("Monitor command %q: expected BLOCK (same as Bash), got allow", blocked)
	}

	allowed := "cd " + p.worktree + " && go build ./..."
	if err := p.run(t, "Monitor", commandInput(allowed)); err != nil {
		t.Errorf("Monitor command %q: expected allow, got block: %v", allowed, err)
	}
}

// TestPolecatPathGuardBashHeredocFedInterpreter pins the one shape where a
// heredoc body really is code: a script handed to an interpreter on stdin
// cannot be inspected, so it is denied rather than waved through.
func TestPolecatPathGuardBashHeredocFedInterpreter(t *testing.T) {
	p := newPolecatTestTown(t)
	command := "python3 - <<'EOF'\nopen('/tmp/x','w').write('hi')\nEOF"
	if err := p.run(t, "Bash", commandInput(command)); err == nil {
		t.Error("expected a heredoc-fed interpreter to be blocked (its body cannot be inspected)")
	}
}

// TestPolecatPathGuardBashUnresolvableReadIsAllowed pins that fail-closed
// applies to writes only: reading through a path the guard cannot resolve is
// not a hazard it needs to block.
func TestPolecatPathGuardBashUnresolvableReadIsAllowed(t *testing.T) {
	p := newPolecatTestTown(t)
	if err := p.run(t, "Bash", commandInput("grep -rn TODO $POLECAT_PATHS_UNSET/dir")); err != nil {
		t.Errorf("expected an unresolvable read target to be allowed, got block: %v", err)
	}
}

func TestSplitPolecatLayout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path     string
		ok       bool
		townRoot string
		rig      string
		name     string
		worktree string
	}{
		{path: "/town/rig/polecats/ruby/gastown", ok: true, townRoot: "/town", rig: "rig", name: "ruby", worktree: "/town/rig/polecats/ruby/gastown"},
		{path: "/town/rig/polecats/ruby/gastown/internal/cmd", ok: true, townRoot: "/town", rig: "rig", name: "ruby", worktree: "/town/rig/polecats/ruby/gastown"},
		{path: "/town/rig/polecats/ruby", ok: true, townRoot: "/town", rig: "rig", name: "ruby", worktree: "/town/rig/polecats/ruby"},
		{path: "/town/rig/crew/alice", ok: false},
		{path: "/town/rig/polecats", ok: false},
		{path: "relative/polecats/ruby/repo", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			layout, ok := splitPolecatLayout(tc.path)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if layout.townRoot != tc.townRoot || layout.rig != tc.rig || layout.name != tc.name || layout.worktree != tc.worktree {
				t.Errorf("layout = %+v, want town=%s rig=%s name=%s worktree=%s", layout, tc.townRoot, tc.rig, tc.name, tc.worktree)
			}
		})
	}
}

func TestIsWithinPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		target string
		root   string
		want   bool
	}{
		{"/worktree", "/worktree", true},
		{"/worktree/a/b", "/worktree", true},
		{"/worktree/a", "/worktree/", true},
		{"/worktree-evil/a", "/worktree", false},
		{"/worktreex", "/worktree", false},
		{"/other", "/worktree", false},
		{"", "/worktree", false},
		{"/worktree", "", false},
		// macOS folds case; a differently-cased twin must not slip through.
		{"/Users/Sloan/gt/x", "/users/sloan/gt", true},
	}
	for _, tc := range cases {
		if got := isWithinPath(tc.target, tc.root); got != tc.want {
			t.Errorf("isWithinPath(%q, %q) = %v, want %v", tc.target, tc.root, got, tc.want)
		}
	}
}

func TestCanonicalizeToolPath(t *testing.T) {
	root := t.TempDir()
	// t.TempDir() on macOS is /var/folders/..., whose /private symlink the
	// canonicaliser resolves; compare against the resolved spelling.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolving the temp root: %v", err)
	}
	existing := filepath.Join(root, "worktree")
	if err := os.MkdirAll(filepath.Join(existing, "internal"), 0o755); err != nil {
		t.Fatalf("creating tree: %v", err)
	}
	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		t.Fatalf("resolving the test tree: %v", err)
	}
	t.Setenv("HOME", resolvedRoot)
	t.Setenv("POLEKAT_TEST_DIR", resolvedExisting)

	t.Run("new file resolves through its parent", func(t *testing.T) {
		got, ok := canonicalizeToolPath(filepath.Join(existing, "internal", "new.go"), "")
		if !ok {
			t.Fatal("expected a not-yet-created file to resolve through its existing parent")
		}
		if want := filepath.Join(resolvedExisting, "internal", "new.go"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("relative resolves against cwd", func(t *testing.T) {
		got, ok := canonicalizeToolPath("internal/new.go", existing)
		if !ok {
			t.Fatal("expected a relative path to resolve")
		}
		if want := filepath.Join(resolvedExisting, "internal", "new.go"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("tilde and env vars expand", func(t *testing.T) {
		for _, raw := range []string{"~/gt/x", "$HOME/gt/x", "${HOME}/gt/x", "$POLEKAT_TEST_DIR/x"} {
			got, ok := canonicalizeToolPath(raw, "")
			if !ok {
				t.Fatalf("%s: expected to resolve", raw)
			}
			if !strings.HasPrefix(got, resolvedRoot) {
				t.Errorf("%s resolved to %s, want a path under %s", raw, got, resolvedRoot)
			}
		}
	})
	t.Run("unresolvable fails closed", func(t *testing.T) {
		for _, raw := range []string{"", "$POLEKAT_TEST_UNSET/x", "$(mktemp -d)/x", "x", `dir/"` + "`" + `cmd` + "`"} {
			if got, ok := canonicalizeToolPath(raw, ""); ok {
				t.Errorf("%q resolved to %q, want a fail-closed failure", raw, got)
			}
		}
	})
}

func TestBashPathCandidates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		word string
		want []string
	}{
		{"plain", nil},
		{"-rf", nil},
		{"-i", nil},
		{"https://example.com/x", nil},
		{"/tmp/x", []string{"/tmp/x"}},
		{"~/gt/x", []string{"~/gt/x"}},
		{"../../x", []string{"../../x"}},
		{"$HOME/gt/x", []string{"$HOME/gt/x"}},
		{"--output=/tmp/x", []string{"/tmp/x"}},
		{"of=/tmp/x", []string{"of=/tmp/x", "/tmp/x"}}, // dd's of=VALUE names its value
		{`open('/town/mayor/x','w')`, []string{"/town/mayor/x"}},
		{`f(1/2)`, nil},
		{`print("http://x/y")`, nil},
	}
	for _, tc := range cases {
		got := bashPathCandidates(tc.word)
		if len(got) != len(tc.want) {
			t.Errorf("bashPathCandidates(%q) = %q, want %q", tc.word, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("bashPathCandidates(%q) = %q, want %q", tc.word, got, tc.want)
				break
			}
		}
	}
}

func TestRedirectTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		command string
		want    []string
	}{
		{"echo hi > /tmp/x", []string{"/tmp/x"}},
		{"echo hi>>/tmp/x", []string{"/tmp/x"}},
		{"make 2>/dev/null", []string{"/dev/null"}},
		{"make 2>&1", nil},
		{"make >&2", nil},
		{"echo x >", nil},
		{"echo '> /not/a/redirect'", nil},
		{"cat > '/tmp/with space/x'", []string{"/tmp/with space/x"}},
		{"echo a > /tmp/a > /tmp/b", []string{"/tmp/a", "/tmp/b"}},
		{"cat < /etc/hosts", nil},
	}
	for _, tc := range cases {
		got := redirectTargets(tc.command)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("redirectTargets(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestSegmentCommandWord(t *testing.T) {
	t.Parallel()
	word, args := segmentCommandWord([]string{"GOFLAGS=-p=6", "env", "FOO=bar", "make", "build"})
	if word != "make" {
		t.Errorf("command word = %q, want make", word)
	}
	if strings.Join(args, " ") != "build" {
		t.Errorf("args = %q, want [build]", args)
	}
	if word, _ := segmentCommandWord([]string{"FOO=bar"}); word != "" {
		t.Errorf("an assignment-only segment should have no command word, got %q", word)
	}
}
