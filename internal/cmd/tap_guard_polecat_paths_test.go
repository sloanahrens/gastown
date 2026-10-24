package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
// The session environment is env, read through proc(), so no test touches the
// process environment and every one can run in parallel.
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
	hostTmp  string // stands in for the host's /tmp (see hostTempScratchDirs)
	env      map[string]string
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

	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatalf("creating the temp root: %v", err)
	}
	// The host's /tmp and /var/tmp are scratch space, but on Linux t.TempDir()
	// is itself under /tmp, which made this whole fixture — its $HOME and its
	// town — scratch space there and not on macOS. The fixture's host temp dir
	// is its own directory instead, so the rules are judged the same on both.
	hostTmp := filepath.Join(root, "hosttmp")
	if err := os.MkdirAll(hostTmp, 0o755); err != nil {
		t.Fatalf("creating the host temp dir: %v", err)
	}
	env := map[string]string{
		"HOME":            root,                       // ~ expands inside the test root, not the operator's home
		"TMPDIR":          filepath.Join(root, "tmp"), // $TMPDIR and /tmp-adjacent rules stay hermetic
		"GT_TOWN_ROOT":    town,
		"GT_ROOT":         town,
		"GT_RIG":          rig,
		"GT_POLECAT":      name,
		"GT_POLECAT_PATH": worktree,
		// $CLAUDE_CONFIG_DIR is the town's own config tree, exactly as
		// production sets it — inside the town, which is also what makes the
		// Bash leg judge it (that leg only polices targets inside the town).
		"CLAUDE_CONFIG_DIR": filepath.Join(town, ".claude-town"),
	}

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
		hostTmp:  hostTmp,
		env:      env,
	}
}

// withEnv returns the town with its session environment changed by kv, a
// list of key, value pairs; an empty value unsets the key.
func (p polecatTestTown) withEnv(kv ...string) polecatTestTown {
	env := make(map[string]string, len(p.env)+len(kv)/2)
	for k, v := range p.env {
		env[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(env, kv[i])
			continue
		}
		env[kv[i]] = kv[i+1]
	}
	p.env = env
	return p
}

// proc is the guard's view of the session: the town's environment, no
// working directory, and the fixture's own host temp dir.
func (p polecatTestTown) proc() guardProcess {
	proc := fakeGuardProcess(p.env, "")
	proc.hostTemp = []string{p.hostTmp}
	return proc
}

// run invokes the guard exactly as the PreToolUse hook does: a Claude Code
// payload on stdin, the guard's exit status as the verdict.
func (p polecatTestTown) run(t *testing.T, tool, toolInput string) error {
	t.Helper()
	payload := fmt.Sprintf(`{"tool_name":%q,"cwd":%q,"tool_input":%s}`, tool, p.worktree, toolInput)
	return tapGuardPolecatPaths(strings.NewReader(payload), io.Discard, p.proc())
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
	t.Parallel()
	p := newPolecatTestTown(t)
	err := p.run(t, "Edit", fileInput(filepath.Join(p.sibling, "internal.go")))
	if err == nil {
		t.Fatalf("expected an Edit inside %s/polecats/%s/ to be blocked, got nil error", p.rig, p.other)
	}
}

// TestRunTapGuardPolecatPaths_TownUnderTempDirStillGuarded pins the guard for a
// town that lives inside a temp directory. The temp dirs are scratch space a
// polecat may always write to, but a scratch root that contains the town must
// not exempt the town: it made every sibling worktree, mayor/ and settings/
// writable. On Linux t.TempDir() is under /tmp, which is how every
// polecat-paths test failed on the CI runner while passing on macOS, whose
// TMPDIR is /var/folders (gt-22hdp.39).
func TestRunTapGuardPolecatPaths_TownUnderTempDirStillGuarded(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	p = p.withEnv("TMPDIR", p.root) // the town's parent is the session's temp dir

	for _, target := range []string{
		filepath.Join(p.sibling, "internal.go"),
		filepath.Join(p.town, "mayor", "state.json"),
		filepath.Join(p.town, "settings", "config.json"),
	} {
		if err := p.run(t, "Edit", fileInput(target)); err == nil {
			t.Errorf("Edit of %s allowed: a temp dir that contains the town exempted the whole town", target)
		}
	}
	if err := p.run(t, "Bash", commandInput("cp x "+filepath.Join(p.sibling, "y.go"))); err == nil {
		t.Errorf("cp into the sibling worktree allowed: a temp dir that contains the town exempted the whole town")
	}
	// The temp dir outside the town is still scratch space.
	if err := p.run(t, "Edit", fileInput(filepath.Join(p.root, "tmp", "scratch.txt"))); err != nil {
		t.Errorf("Edit in the temp dir outside the town blocked: %v", err)
	}
	// And the worktree is still the polecat's own.
	if err := p.run(t, "Edit", fileInput(filepath.Join(p.worktree, "main.go"))); err != nil {
		t.Errorf("Edit in the own worktree blocked: %v", err)
	}
}

// TestRunTapGuardPolecatPaths_BlocksSharedBinDir is the live-block leg for
// gt-tnts5: obsidian, a local-coder polecat, overwrote the production bd
// binary at $HOME/.local/bin/bd with a Bash echo, causing a town-wide bd
// outage. $HOME/.local/bin sits outside the town tree, so it must be blocked
// on its own rule rather than the town-membership check that protects
// everything else — driven through the real stdin payload, not internals.
func TestRunTapGuardPolecatPaths_BlocksSharedBinDir(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := newPolecatTestTown(t)
	configDir := p.env["CLAUDE_CONFIG_DIR"]

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
		{"tmp", "Write", fileInput(filepath.Join(p.hostTmp, "polecat-paths-probe", "x.go")), false},
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

// TestPolecatPathGuardFollowsSymlinks pins the symlink leg: a symlink
// created inside the worktree that points at a sibling's worktree must not be
// a way in.
func TestPolecatPathGuardFollowsSymlinks(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	p := newPolecatTestTown(t)
	p = p.withEnv("CLAUDE_CONFIG_DIR", p.root) // the test's $HOME, a config dir shaped wrong
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
	t.Parallel()
	p := newPolecatTestTown(t)
	p = p.withEnv("GT_POLECAT", "", "GT_POLECAT_PATH", "")
	err := p.run(t, "Edit", fileInput(filepath.Join(p.sibling, "internal.go")))
	if err != nil {
		t.Errorf("expected no-op outside a polecat session, got block: %v", err)
	}
}

// TestPolecatPathGuardBash covers the Bash leg in one table: writes aimed at
// another agent's tree are blocked, ordinary polecat work is not.
func TestPolecatPathGuardBash(t *testing.T) {
	t.Parallel()
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
		// A cd moves the directory every later word resolves from: ../coral is a
		// path inside the polecat's own directory from the worktree, and the
		// sibling polecat's directory from one level up (gt-g2zd6).
		{"cd up then rm a sibling polecat dir", "cd .. && rm -rf ../" + p.other, true},
		{"cd up twice then rm a sibling polecat dir", "cd .. && cd .. && rm -rf " + p.other, true},
		{"cd up with a semicolon", "cd ..; rm -rf ../" + p.other, true},
		{"cd -P up then rm a sibling polecat dir", "cd -P .. && rm -rf ../" + p.other, true},
		{"cd -- up then rm a sibling polecat dir", "cd -- .. && rm -rf ../" + p.other, true},
		{"pushd up then rm a sibling polecat dir", "pushd .. && rm -rf ../" + p.other, true},
		{"cd up then redirect into a sibling polecat dir", "cd .. && echo x > ../" + p.other + "/f", true},
		{"cd up then mv into a sibling polecat dir", "cd .. && mv a ../" + p.other + "/a", true},
		{"cd up in a subshell", "(cd .. && rm -rf ../" + p.other + ")", true},
		{"cd up in a brace group", "{ cd ..; rm -rf ../" + p.other + "; }", true},
		{"cd up inside bash -c", "bash -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"cd up inside sh -lc", "sh -lc 'cd .. && cd .. && rm -rf " + p.other + "'", true},
		{"cd up inside bash -o pipefail -c", "bash -o pipefail -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"cd up then a substitution that removes a sibling", "cd .. && echo $(rm -rf ../" + p.other + ")", true},
		{"cd home then rm a sibling polecat dir by a relative path", "cd ~ && rm -rf gt/" + p.rig + "/polecats/" + p.other, true},
		{"bare cd then rm a sibling polecat dir by a relative path", "cd && rm -rf gt/" + p.rig + "/polecats/" + p.other, true},
		{"cd up then git -C a sibling polecat dir", "cd .. && git -C ../" + p.other + " checkout -- .", true},
		{"cd up then cd into a sibling polecat dir", "cd .. && cd ../" + p.other, true},
		// $OLDPWD is set by an earlier command unless a cd of this line set it.
		{"cd - after a cd up is judged from the set", "cd .. && cd - && rm -rf ../" + p.other, true},
		{"cd - first returns to a directory the guard never saw", "cd - && ls", true},
		{"cd - first then rm", "cd - && rm -rf build", true},
		{"cd - after OLDPWD is assigned", "cd internal && OLDPWD=" + p.sibling + " cd - && rm -rf x", true},
		{"cd - inside bash -c with no cd before it", "bash -c 'cd - && ls'", true},
		// A shell behind a wrapper is walked like a bare one.
		{"timeout wrapping bash -c cd up then rm a sibling", "timeout 5 bash -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"env -i wrapping bash -c cd up then rm a sibling", "env -i bash -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"nice -n wrapping sh -c cd up then rm a sibling", "nice -n 5 sh -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"sudo -u wrapping bash -c cd up then rm a sibling", "sudo -u root bash -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"env assignment then timeout wrapping bash -c", "A=1 timeout 5 /bin/bash -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"xargs sh -c cd up then rm a sibling", "ls | xargs -I{} sh -c 'cd .. && rm -rf ../" + p.other + "'", true},
		{"find -exec sh -c cd up then rm a sibling", `find . -name x -exec sh -c 'cd .. && rm -rf ../` + p.other + `' \;`, true},
		{"cd with an unresolvable destination then rm", "cd $POLECAT_PATHS_UNSET && rm -rf build", true},
		// git -C reaches another tree without naming a path anywhere else.
		{"git -C into a sibling worktree", "git -C " + p.sibling + " checkout -- .", true},
		{"git -C into the town mayor dir", "git -C " + filepath.Join(town, "mayor") + " status", true},
		// Command substitutions run as real commands.
		{"writing inside a command substitution", `echo "$(cp a.go ` + sib + `)"`, true},
		{"writing inside backticks", "echo `cp a.go " + sib + "`", true},
		// A body holding a parenthesis is cut whole, not at the first ")"
		// (gt-d1mlb).
		{"writing after a quoted paren in a substitution", `echo "$(grep -E '(a|b)' f; cp a.go ` + sib + `)"`, true},
		{"writing after a nested substitution", `echo "$(echo $(date); cp a.go ` + sib + `)"`, true},
		{"writing inside a nested substitution", `echo "$(echo $(cp a.go ` + sib + `))"`, true},
		// sed edits in place when i sits anywhere in a short-option cluster,
		// not only when the argument starts with -i (gt-x9gya).
		{"sed -i on a sibling", "sed -i 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed -ri on a sibling", "sed -ri 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed -ni on a sibling", "sed -ni 's/a/b/p' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed -Ei on a sibling", "sed -Ei 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed --in-place on a sibling", "sed --in-place=.bak 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		// GNU sed accepts an unambiguous prefix of a long option.
		{"sed --in-plac on a sibling", "sed --in-plac 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed --in on a sibling", "sed --in 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed --i=.bak on a sibling", "sed --i=.bak 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		{"sed --quiet reads a sibling", "sed --quiet 's/a/b/p' " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed --in-placex is not in-place", "sed --in-placex 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), false},
		// A cluster ends at -e, -f or -l: what follows is that option's value,
		// not more flags, so the i in it does not turn editing on.
		{"sed -es/i/x/ reads a sibling", "sed -es/i/x/ " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed -nes/i/x/p reads a sibling", "sed -nes/i/x/p " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed -fi.sed reads a sibling", "sed -fi.sed " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed -l5i reads a sibling", "sed -l5i " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed -ie edits a sibling (i before e)", "sed -ie 's/a/b/' " + filepath.Join(p.sibling, "internal.go"), true},
		// Fail closed when a checked target cannot be resolved.
		{"unresolvable variable in a write", "cp a.go $POLECAT_PATHS_UNSET/x.go", true},
		{"unresolvable variable as a redirect target", "echo hi > $POLECAT_PATHS_UNSET/x.go", true},
		// A control keyword in front of the command must not hide it: the
		// segment `do rm ...` named no write-capable command at all.
		{"rm behind a for-loop's do", "for f in a b; do rm -rf " + filepath.Join(p.sibling, "internal.go") + "; done", true},
		{"cp behind a then", "if true; then cp a.go " + sib + "; fi", true},

		// --- allowed ---
		{"read-only command in a sibling worktree", "grep -rn TODO " + p.sibling, false},
		{"read-only command in a town dir", "ls -la " + filepath.Join(town, "mayor"), false},
		{"cat a sibling's file", "cat " + filepath.Join(p.sibling, "internal.go"), false},
		{"grep a sibling through a path pattern", "grep -rn 'func main' " + filepath.Join(p.sibling, "*.go"), false},
		{"build in the worktree", "make build", false},
		{"scoped tests", "GOFLAGS=-p=6 go test ./internal/cmd/...", false},
		{"cd own worktree then build", "cd " + p.worktree + " && go build ./...", false},
		{"cd a worktree subdir then rm build", "cd internal && rm -rf build", false},
		{"cd an absolute worktree subdir then rm build", "cd " + filepath.Join(p.worktree, "internal") + " && rm -rf build", false},
		{"cd a worktree subdir then test", "cd internal/cmd && go test ./...", false},
		{"cd a worktree subdir then rm beside it", "cd internal/cmd && rm -rf ../build", false},
		{"cd up to the polecat dir then ls", "cd .. && ls", false},
		{"cd up to the polecat dir then rm inside it", "cd .. && rm -rf scratch", false},
		{"cd -P a worktree subdir then rm build", "cd -P internal && rm -rf build", false},
		{"cd to the host temp dir then rm", "cd " + filepath.Join(p.hostTmp, "polecat-paths-probe") + " && rm -rf build", false},
		{"cd out of the town then rm", "cd " + p.root + " && rm -rf elsewhere", false},
		{"bash -c cd a worktree subdir then rm build", "bash -c 'cd internal && rm -rf build'", false},
		{"bash script file with a later -c", "bash run.sh -c 'cd .. && ls'", false},
		{"cd - after a cd of this line then rm", "cd internal && cd - && rm -rf build", false},
		{"timeout wrapping bash -c cd a worktree subdir then rm build", "timeout 5 bash -c 'cd internal && rm -rf build'", false},
		{"env -i wrapping bash -c cd a worktree subdir then rm build", "env -i bash -c 'cd internal && rm -rf build'", false},
		{"xargs sh -c cd a worktree subdir then rm build", "ls | xargs sh -c 'cd internal && rm -rf build'", false},
		{"find -name bash is not a shell invocation", "find . -name bash -print", false},
		{"timeout wrapping a script file with a later -c", "timeout 5 bash run.sh -c 'cd .. && ls'", false},
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
		{"sed -n s///p reading a sibling", "sed -n 's/a/b/p' " + filepath.Join(p.sibling, "internal.go"), false},
		{"sed -ri on an own-worktree file", "sed -ri 's/a/b/' " + own, false},
		{"python with no paths", `python3 -c "print(1)"`, false},
		// gt-uqp17: a perl payload's $1, $. and $_ are not path variables.
		{"perl -pi -e with a capture group reference", `perl -pi -e 's/(foo)/bar$1/g' main.go`, false},
		{"perl -pi -e with the line counter", `perl -pi -e 's/foo/bar/ if $. > 2' main.go`, false},
		{"perl -ne printing the topic variable", `perl -ne 'print $_ if /x/' main.go`, false},
		{"perl -e payload leading a path with an unresolved variable", `perl -e 'unlink "$NO_SUCH_VAR_UQP17/x"'`, true},
		{"perl -e payload with $1 and an unresolved variable path", `perl -e 's/(a)/$1/; unlink "$NO_SUCH_VAR_UQP17/x"'`, true},
		{"perl -e payload naming a sibling path then $1", `perl -e 's/(a)/` + sib + `$1/'`, true},
		{"perl -e payload naming a sibling path then $.", `perl -e 'print "` + sib + `$."'`, true},
		{"perl -e payload naming a sibling path then a trailing $", `perl -e 'unlink "` + sib + `$"'`, true},
		{"perl -e payload naming the mayor dir then $_", `perl -e 'unlink "` + mayor + `$_"'`, true},
		{"perl -e payload with a command substitution path", `perl -e 'unlink "$(echo x)"'`, true},
		{"perl -e payload deleting under $GT_TOWN_ROOT", `perl -e 'unlink "$GT_TOWN_ROOT/` + p.rig + `/polecats/` + p.other + `/x"'`, true},
		{"heredoc body into own worktree", "cat > " + own + " <<'EOF'\nsee " + sib + " for context\nEOF", false},
		{"heredoc body mentioning the mayor dir", "cat > " + own + " <<'EOF'\n" + mayor + "\nEOF", false},
		{"redirect after a heredoc whose body holds an apostrophe", "cat <<'EOF' > " + own + "\nit's x\nEOF\necho y > " + filepath.Join(p.sibling, "internal.go"), true},
		{"redirect after a heredoc body with no apostrophe", "cat <<'EOF' > " + own + "\nx\nEOF\necho y > " + filepath.Join(p.sibling, "internal.go"), true},
		{"commit body quoting a redirect into town settings", "git commit -F - <<'EOF'\nfix: runs a > " + settings + "\nEOF", false},
		{"redirect on the heredoc opener line into a sibling", "cat <<'EOF' > " + sib + "\nx\nEOF", true},
		{"redirect on the heredoc opener line into own worktree", "cat <<'EOF' > " + own + "\nx\nEOF", false},
		// Redirections are read off the stripped text, so a "<<" the stripper
		// wrongly took for a heredoc would hide the real redirect standing where
		// its body would have been. None of these is a heredoc to the shell, and
		// a sibling redirect follows each.
		{"redirect after a shift in arithmetic expansion", "echo $((1 <<EOF))\necho y > " + sib + "\nEOF", true},
		{"redirect after a here-string", "cat <<<EOF\necho y > " + sib + "\nEOF", true},
		{"redirect after a quoted double angle", "echo '<<EOF'\necho y > " + sib + "\nEOF", true},
		{"redirect after an expansion word holding a double angle", "echo ${v%<<EOF}\necho y > " + sib + "\nEOF", true},
		{"redirect after an unterminated heredoc", "cat <<EOF\necho y > " + sib, true},
		{"redirect after a commented double angle", "echo hi # cat <<EOF\necho y > " + sib + "\nEOF", true},
		{"redirect after an escaped double angle", "echo \\<<EOF\necho y > " + sib + "\nEOF", true},
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	p := newPolecatTestTown(t)
	if err := p.run(t, "Bash", commandInput("grep -rn TODO $POLECAT_PATHS_UNSET/dir")); err != nil {
		t.Errorf("expected an unresolvable read target to be allowed, got block: %v", err)
	}
}

// TestPolecatPathGuardBashVariableIndirection is the gt-tt8sg leg: the
// guard expands a target's variable from its own environment, and a command
// line that assigns the name itself has already replaced that value for the
// shell that will run it. The session here carries `path`, exactly as the
// incident's zsh session did — a PATH-shaped value that is not a town path, so
// the guard read `rm "$path"` as an ordinary delete outside the town and let
// it through. The fixtures are the three shapes the bead names, plus the FIX
// probes: the incident command verbatim, `rm $(which bd)`, and
// `mv -f "$T" ~/.local/bin/bd`.
func TestPolecatPathGuardBashVariableIndirection(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t).withEnv("path", "/usr/local/bin:/usr/bin:/bin")
	bin := filepath.Join(p.root, ".local", "bin", "bd")

	cases := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{
			"the incident: rm a path resolved by command -v inside a for loop",
			"export PATH=\"/tmp/gt-uninst-probe/bin:$PATH\"\n" +
				"for bin in gt bd; do path=$(command -v \"$bin\") || continue; chflags nouchg \"$path\"; rm \"$path\"; done",
			true,
		},
		{"the incident without the loop", `path=$(command -v bd); rm "$path"`, true},
		{"chflags on a path resolved by command -v", `path=$(command -v bd); chflags nouchg "$path"`, true},
		{"rm through which directly", "rm $(which bd)", true},
		{"rm through command -v directly", "rm $(command -v bd)", true},
		{"rm through type directly", "rm $(type -p bd)", true},
		{"mv a variable assigned earlier in the line", `T=$(mktemp); mv -f "$T" ` + bin, true},
		{"cp onto a variable-assigned destination", `out=$(mktemp -d); cp a.go "$out"`, true},
		{"rm a for-loop variable that shadows the environment", `for path in a b; do rm "$path"; done`, true},
		{"rm a variable set by read", `read p; rm "$p"`, true},
		{"redirect onto a variable-assigned target", `T=out; echo hi > "$T"`, true},
		{"rm a braced variable assigned in the line", `path=$(command -v bd); rm "${path}"`, true},
		// An interpreter's payload is code, and code writes: the first cut of
		// this rule judged the payload as a read, so this exact shape still
		// reached the production bd.
		{"python3 payload writing a variable resolved by command -v", `path=$(command -v bd); python3 -c "open('$path','w').write('stub')"`, true},
		{"node payload writing a variable set by which", "p=`which bd`; node -e \"require('fs').writeFileSync('$p','x')\"", true},
		{"sh payload removing a for-loop variable", `for path in a b; do sh -c "rm \"$path\""; done`, true},

		// Indirection is the trigger, not variables: a name the guard really can
		// look up in the session environment still resolves as it always did.
		{"rm under $TMPDIR", "rm -rf $TMPDIR/polecat-paths-probe", false},
		{"cp to $TMPDIR", "cp a.go $TMPDIR/polecat-paths-probe/x.go", false},
		{"read through an assigned variable", `p=$(command -v bd); cat "$p"`, false},
		{"read through which", "grep -rn STUB $(which bd)", false},
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

// TestPolecatPathGuardBashGroupingConstructs is the other half of the gt-tt8sg
// review: a construct that OPENS a segment — a subshell, a brace group, a
// `case` body — reported its own punctuation as the command word, so the rm
// inside it was never looked at. The fixtures are the shapes the review named,
// the glued spellings a tokenizer leaves together, and the reads and
// own-worktree writes that must stay allowed.
func TestPolecatPathGuardBashGroupingConstructs(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	sib := filepath.Join(p.sibling, "internal.go")

	cases := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{"subshell around an rm", `( rm -rf ` + sib + ` )`, true},
		{"brace group around an rm", `{ rm -rf ` + sib + `; }`, true},
		{"subshell glued to its command", `(rm -rf ` + sib + `)`, true},
		{"brace group glued to its command", `{rm -rf ` + sib + `; }`, true},
		{"if-then body", `if [ -e ` + sib + ` ]; then rm -rf ` + sib + `; fi`, true},
		{"case body", `case x in a) rm -rf ` + sib + `;; esac`, true},
		{"case body, second branch", `case x in a) echo hi ;; b) cp a.go ` + sib + ` ;; esac`, true},
		{"subshell around a variable write", `( T=x; echo hi > "$T" )`, true},
		{"subshell behind time", `time ( rm -rf ` + sib + ` )`, true},
		{"brace group behind a negation", `! { rm -rf ` + sib + `; }`, true},
		{"subshell behind env", `env ( rm -rf ` + sib + ` )`, true},
		{"rm behind exec", `exec rm -rf ` + sib, true},
		{"rm behind builtin", `builtin rm -rf ` + sib, true},

		{"subshell around a read", `( cat ` + sib + ` )`, false},
		{"case body reading", `case x in a) cat ` + sib + ` ;; esac`, false},
		{"case body echoing", `case x in a) echo hi ;; esac`, false},
		{"subshell writing to /tmp", `( echo hi > $TMPDIR/x )`, false},
		{"subshell writing to the worktree", `( cp a.go ` + filepath.Join(p.worktree, "b.go") + ` )`, false},
		// A stray ")" in a quoted token closes the assignment's substitution
		// early; the real ")" then reads as the command word, and the rm behind
		// it must still be judged (gt-lhn14).
		{"stray paren in a quoted token, rm behind it", `y=$(echo ")" ) rm -rf ` + sib, true},
		{"stray paren in a quoted token, cp behind it", `y=$(echo ")" ) cp a.go ` + sib, true},
		{"stray paren in a quoted token, harmless command behind it", `y=$(echo ")" ) ls ` + sib, false},
		{"stray paren in a quoted token, echo behind it", `y=$(echo ")" ) echo hi`, false},
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

// TestPolecatPathGuardReadingsStartFromEntry: the readings of a segment are
// alternatives, so a cd in one must not move where the next resolves from.
// The ")" reading here is `cd sub`; the payload reading after it still stands
// where the segment starts, where a `cd -` has no directory this line made,
// so that payload is refused (gt-lhn14).
func TestPolecatPathGuardReadingsStartFromEntry(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	for _, tc := range []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{"payload with cd - after a reading that cd'd", `X=$(echo ")" ) cd sub ) bash -c "cd -; echo hi"`, true},
		{"payload echoing after a reading that cd'd", `X=$(echo ")" ) cd sub ) bash -c "echo hi"`, false},
	} {
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

// TestPolecatPathGuardCdCarriedFromNonfirstReading: a stray ")" or a quoted
// "$(" makes reading 0 of a segment miss the cd that a later reading finds,
// and in bash that cd runs in the current shell either way. The next segment
// is judged from the directories of every reading (gt-8lv66).
func TestPolecatPathGuardCdCarriedFromNonfirstReading(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	for _, tc := range []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{"cd-carried-from-nonfirst-reading, stray paren", `y=$(echo ")" ) cd .. && rm -rf ../` + p.other, true},
		{"cd-carried-from-nonfirst-reading, quoted dollar-paren", `X='$(a' cd .. && rm -rf ../` + p.other, true},
		{"cd-carried-from-nonfirst-reading, harmless rm", `y=$(echo ")" ) cd .. && rm -rf scratch`, false},
	} {
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

// TestPolecatPathGuardRefusesTooManyReadings drives the reading cap through
// the guard: past the cap the whole segment is refused, even when no reading
// writes anything (gt-lhn14).
func TestPolecatPathGuardRefusesTooManyReadings(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	var parts []string
	for i := 0; i < 6; i++ {
		parts = append(parts, fmt.Sprintf("V%d=$(echo", i), `")"`, ")")
	}
	command := strings.Join(append(parts, "echo", "hi"), " ")
	var stderr strings.Builder
	payload := fmt.Sprintf(`{"tool_name":%q,"cwd":%q,"tool_input":%s}`, "Bash", p.worktree, commandInput(command))
	if err := tapGuardPolecatPaths(strings.NewReader(payload), &stderr, p.proc()); err == nil {
		t.Fatalf("command %q allowed: its readings outnumber the guard's cap", command)
	}
	if !strings.Contains(stderr.String(), "too many readings") {
		t.Errorf("command %q: stderr %q, want the too-many-readings refusal", command, stderr.String())
	}
}

// TestScanCommandVars pins what the assignment scanner sees, since a name it
// misses is a name the guard resolves from a stale environment, and a name it
// invents would refuse a legitimate literal target.
func TestScanCommandVars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		command  string
		assigned []string
		lookup   []string
	}{
		{name: "plain assignment", command: "p=/tmp/x; rm a", assigned: []string{"p"}},
		{name: "command -v assignment", command: `p=$(command -v bd); rm a`, assigned: []string{"p"}, lookup: []string{"p"}},
		{name: "which assignment in backticks", command: "p=`which bd`; rm a", assigned: []string{"p"}, lookup: []string{"p"}},
		{name: "for variable", command: `for f in a b; do rm "$f"; done`, assigned: []string{"f"}},
		{name: "read variable", command: "read -r p; rm a", assigned: []string{"p"}},
		{name: "read with a flag that takes a value", command: `read -p 'prompt: ' ans; rm "$ans"`, assigned: []string{"ans"}},
		{name: "printf -v setter", command: `printf -v p '%s' /tmp/x; rm "$p"`, assigned: []string{"p"}},
		{name: "read behind sudo", command: "sudo read p", assigned: []string{"p"}},
		// The setters are read at command position only: a word that merely
		// reads as one names nothing, and treating it as a setter would deny a
		// target the line really does spell out.
		{name: "for in argument position", command: "echo for x", assigned: nil},
		{name: "read in argument position", command: "grep -n read p", assigned: nil},
		// A NAME= value that is merely data must not read as an assignment.
		{name: "assignment inside a commit message", command: `git commit -m "fix: a=b"`, assigned: nil},
		{name: "no assignments at all", command: "cp a.go b.go", assigned: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := scanCommandVars(tc.command)
			for _, name := range tc.assigned {
				if !vars.assigned[name] {
					t.Errorf("scanCommandVars(%q): %s not seen as assigned", tc.command, name)
				}
			}
			for _, name := range tc.lookup {
				if !vars.lookup[name] {
					t.Errorf("scanCommandVars(%q): %s not seen as a command lookup", tc.command, name)
				}
			}
			if len(vars.assigned) != len(tc.assigned) {
				t.Errorf("scanCommandVars(%q).assigned = %v, want %v", tc.command, vars.assigned, tc.assigned)
			}
			// A name read as a command lookup is the stricter of the two and
			// must be pinned too: an extra entry here denies a legitimate
			// literal target through a name the line never set that way.
			if len(vars.lookup) != len(tc.lookup) {
				t.Errorf("scanCommandVars(%q).lookup = %v, want %v", tc.command, vars.lookup, tc.lookup)
			}
		})
	}
}

// TestShellVarNames pins the name extraction: braced and bare forms both count,
// and `$1` / `$?` / `$$` are not names.
func TestShellVarNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		word string
		want []string
	}{
		{"$path", []string{"path"}},
		{"$path/x", []string{"path"}},
		{"${path}/x", []string{"path"}},
		{"$HOME/.local/bin/bd", []string{"HOME"}},
		{"$a$b", []string{"a", "b"}},
		{"/tmp/literal", nil},
		{`"$path"`, []string{"path"}},
		{"$1", nil},
		{"$?", nil},
		{"$", nil},
		{"${}", nil},
	}
	for _, tc := range cases {
		got := shellVarNames(tc.word)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("shellVarNames(%q) = %v, want %v", tc.word, got, tc.want)
		}
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
	t.Parallel()
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
	proc := fakeGuardProcess(map[string]string{
		"HOME":             resolvedRoot,
		"POLEKAT_TEST_DIR": resolvedExisting,
	}, "")

	t.Run("new file resolves through its parent", func(t *testing.T) {
		got, ok := canonicalizeToolPath(proc, filepath.Join(existing, "internal", "new.go"), "")
		if !ok {
			t.Fatal("expected a not-yet-created file to resolve through its existing parent")
		}
		if want := filepath.Join(resolvedExisting, "internal", "new.go"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("relative resolves against cwd", func(t *testing.T) {
		got, ok := canonicalizeToolPath(proc, "internal/new.go", existing)
		if !ok {
			t.Fatal("expected a relative path to resolve")
		}
		if want := filepath.Join(resolvedExisting, "internal", "new.go"); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})
	t.Run("tilde and env vars expand", func(t *testing.T) {
		for _, raw := range []string{"~/gt/x", "$HOME/gt/x", "${HOME}/gt/x", "$POLEKAT_TEST_DIR/x"} {
			got, ok := canonicalizeToolPath(proc, raw, "")
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
			if got, ok := canonicalizeToolPath(proc, raw, ""); ok {
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
		{`s/(foo)/bar$1/g`, []string{"/bar"}},
		{`s/foo/bar/ if $. > 2`, []string{"s/foo/bar/ if "}},
		{`unlink "/town/mayor/x$1"`, []string{"/town/mayor/x"}},
		{`print "/town/mayor/x$."`, []string{"/town/mayor/x"}},
		{`unlink "/town/mayor/x$"`, []string{"/town/mayor/x"}},
		{`unlink "$(echo x)"`, []string{"$"}},
		{`unlink "/a$1/$HOME/x"`, []string{"/a$1/$HOME/x"}},
		{`print $_ if /x/`, []string{"print ", "/x/"}},
		{`unlink $path`, []string{"$path"}},
		{`unlink "$HOME/x"`, []string{"$HOME/x"}},
		{`s/(a)/$1/; unlink "$X/y"`, []string{"/", "$X/y"}},
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

// TestRedirectTargetsAmpersandRedirect: bash's ">&" duplicates a file
// descriptor only when the word is a descriptor number or "-" (2>&1, >&2,
// >&-); any other word is a file, so "echo x >& sibling.go" writes the
// sibling worktree and the guard must treat it as a write.
func TestRedirectTargetsAmpersandRedirect(t *testing.T) {
	t.Parallel()

	const siblingFile = "/gt/gastown/polecats/coral/gastown/internal.go"
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"echo x >& " + siblingFile, []string{siblingFile}},
		{"echo x >&" + siblingFile, []string{siblingFile}},
		// A variable-led word is never a descriptor: its expansion cannot be
		// known here, so the guard reads it as a file target.
		{"echo x >& $out", []string{"$out"}},
		{"echo x >& ${out}", []string{"${out}"}},
		{"echo x >& \"$out/internal.go\"", []string{"$out/internal.go"}},
		// Quotes are removed before the descriptor test, so a quoted path is a
		// file path with its spaces intact and a quoted number is still a fd.
		{"cat x >& '/tmp/with space/x'", []string{"/tmp/with space/x"}},
		{"make 2>&\"1\"", nil},
		{"make 2>&1", nil},
		{"make >&2", nil},
		{"make >& 2", nil},
		{"make >&-", nil},
	} {
		if got := redirectTargets(tc.command); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("redirectTargets(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}

	p := newPolecatTestTown(t)
	if err := p.run(t, "Bash", commandInput("echo x >& "+filepath.Join(p.sibling, "internal.go"))); err == nil {
		t.Errorf("redirect >& into the sibling worktree allowed: it is a write, not fd duplication")
	}
	if err := p.run(t, "Bash", commandInput("echo x >& '"+filepath.Join(p.sibling, "with space.go")+"'")); err == nil {
		t.Errorf("quoted redirect >& into the sibling worktree allowed")
	}
	for _, allowed := range []string{"make 2>&1", "make >&2"} {
		if err := p.run(t, "Bash", commandInput(allowed)); err != nil {
			t.Errorf("%q refused: %v", allowed, err)
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
	// A leading subshell paren and an assignment whose value holds a
	// substitution (split across tokens by shlex) both lead to the real command.
	for _, tt := range []struct {
		name    string
		segment []string
		want    string
	}{
		{"subshell paren", []string{"(", "bash", "<<EOF"}, "bash"},
		{"assignment with a substitution", []string{"y=$(echo", "a)", "bash", "-s"}, "bash"},
		{"assignment with a nested substitution", []string{"y=$(echo", "$(date))", "bash"}, "bash"},
		{"subshell paren with an env launcher", []string{"(", "env", "X=1", "bash"}, "bash"},
		{"lone subshell paren", []string{"("}, ""},
		{"substitution closed in its own token", []string{"y=$(echo)", "bash"}, "bash"},
		{"substitution spread over three tokens", []string{"y=$(echo", "$(date)", "a)", "bash"}, "bash"},
		{"substitution closed by a token with two parens", []string{"y=$(echo", "a))", "bash"}, "bash"},
		{"plain assignment before a substitution assignment", []string{"A=1", "y=$(echo", "a)", "bash"}, "bash"},
		{"subshell paren after a substitution assignment", []string{"y=$(echo", "a)", "(", "bash"}, "bash"},
		// A backtick substitution closes on a later token, as "$(" does, so the
		// command after it is the word that follows (gt-bdct5).
		{"backtick substitution closed in a later token", []string{"y=`echo", "a`", "apt-get"}, "apt-get"},
		{"backtick substitution closed in its own token", []string{"y=`echo`", "bash"}, "bash"},
		{"backtick substitution that never closes", []string{"y=`echo", "a", "bash"}, "echo"},
		// shlex drops the quotes around a ")" inside a substitution, so the
		// stray ")" tokens are not the command (gt-bdct5).
		{"quoted paren closes the substitution early", []string{"y=$(echo", ")", ")", "apt-get"}, "apt-get"},
		// A launcher's own flags are not the command word: the command is the
		// first word after them, and a value flag takes its value (gt-48buu).
		{"env with a flag before the command", []string{"env", "-i", "bash"}, "bash"},
		{"nice with a value flag before the command", []string{"nice", "-n", "5", "bash"}, "bash"},
		{"time with a flag before the command", []string{"time", "-p", "bash"}, "bash"},
		{"sudo with a value flag before the command", []string{"sudo", "-u", "alice", "bash"}, "bash"},
		{"launcher flags then an env assignment", []string{"env", "-i", "X=1", "bash"}, "bash"},
		{"launcher flags with nothing after them", []string{"env", "-i"}, ""},
		// A substitution that never closes runs the text after its "$(" first.
		{"substitution that never closes in the segment", []string{"y=$(echo", "a", "bash"}, "echo"},
		{"substitution that never closes around a launcher", []string{"y=$(sudo", "rm", "-rf", "/"}, "sudo"},
		// A "$(" with nothing after it is a substitution whose command is the
		// next word, or a literal "$(" whose command is the next word too.
		{"unclosed substitution with nothing after its paren", []string{"X=$(", "bash", "-c"}, "bash"},
		{"literal dollar-paren in a value, command after it", []string{"X=$(", "rm", "-rf", "/"}, "rm"},
	} {
		if word, _ := segmentCommandWord(tt.segment); word != tt.want {
			t.Errorf("%s: command word = %q, want %q", tt.name, word, tt.want)
		}
	}
}

func TestSubstitutionStep(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		token      string
		depth      int
		ticked     bool
		wantDepth  int
		wantTicked bool
	}{
		{"opens a dollar-paren substitution", "$(echo", 0, false, 1, false},
		{"closes a dollar-paren substitution", "a)", 1, false, 0, false},
		{"opens a backtick substitution", "`echo", 0, false, 1, true},
		{"closes a backtick substitution", "a`", 1, true, 0, false},
		{"backtick opened and closed in one token", "`echo`", 0, false, 0, false},
		{"plain token changes nothing", "apt-get", 2, false, 2, false},
	} {
		depth, ticked := substitutionStep(tt.token, tt.depth, tt.ticked)
		if depth != tt.wantDepth || ticked != tt.wantTicked {
			t.Errorf("%s: substitutionStep(%q, %d, %v) = (%d, %v), want (%d, %v)",
				tt.name, tt.token, tt.depth, tt.ticked, depth, ticked, tt.wantDepth, tt.wantTicked)
		}
	}
}

func TestJoinSubstitutionSeparators(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		tokens []string
		want   string
	}{
		{"separator inside a substitution is removed", []string{"y=$(echo", "a", ";", "echo", "b)", "apt-get"}, "y=$(echo a echo b) apt-get"},
		{"separator after a substitution closes is kept", []string{"y=$(echo", "a)", ";", "rm"}, "y=$(echo a) ; rm"},
		{"separator inside a backtick substitution is removed", []string{"y=`echo", "a;", "echo", "b`", "apt-get"}, "y=`echo a; echo b` apt-get"},
	} {
		if got := strings.Join(joinSubstitutionSeparators(tt.tokens), " "); got != tt.want {
			t.Errorf("%s: joined = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestSplitShellSegmentsJudgesSharedSegmentOnce(t *testing.T) {
	t.Parallel()
	// No substitution holds a separator, so both readings agree and each
	// segment appears once.
	if got := splitShellSegments([]string{"make", "build", "&&", "make", "test"}); len(got) != 2 {
		t.Errorf("two commands split into %d segments, want 2: %q", len(got), got)
	}
	// The ";" inside the substitution makes the joined reading differ. Its
	// segment is added beside the split ones, and the shared "apt-get" segment
	// is not repeated.
	segments := splitShellSegments([]string{"y=$(echo", "a", ";", "echo", "b)", "apt-get", "install", "foo"})
	if len(segments) != 3 {
		t.Fatalf("substitution with a separator split into %d segments, want 3: %q", len(segments), segments)
	}
	found := false
	for _, segment := range segments {
		if word, _ := segmentCommandWord(segment); word == "apt-get" {
			found = true
		}
	}
	if !found {
		t.Errorf("no segment names apt-get as its command: %q", segments)
	}
}

// TestQuotedDollarParenHidesNoCommand: shlex drops the quotes around a literal
// "$(", so X='$(echo' rm arrives as X=$(echo rm, which reads as an assignment
// whose substitution never closes. The shell runs rm there, so the guard must
// still judge rm, as well as the substitution's own text.
func TestQuotedDollarParenHidesNoCommand(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	for _, command := range []string{
		"X='$(echo' rm -rf " + filepath.Join(p.sibling, "internal.go"),
		"X='$(echo' bash <<'EOF'\nrm -rf " + p.sibling + "\nEOF",
		"A=1 X='$(echo' rm -rf " + filepath.Join(p.sibling, "internal.go"),
	} {
		if err := p.run(t, "Bash", commandInput(command)); err == nil {
			t.Errorf("%q allowed: the quoted dollar-paren is a plain value, so the command after it runs", command)
		}
	}
}

// TestSegmentReadings: an assignment that opens a substitution it never closes
// is read both ways, the substitution's text first and the literal reading
// (the assignment dropped) after it. A stray ")" in a quoted token closes the
// substitution early, so the reading that drops the assignment through the
// real close is read too (gt-lhn14). Segments without one are unchanged.
func TestSegmentReadings(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		segment []string
		want    [][]string
	}{
		{"no substitution", []string{"make", "build"}, [][]string{{"make", "build"}}},
		{"closed substitution", []string{"y=$(echo)", "bash"}, [][]string{{"y=$(echo)", "bash"}}},
		{"unclosed substitution", []string{"X=$(echo", "bash"}, [][]string{{"X=$(echo", "bash"}, {"bash"}}},
		{"unclosed substitution after a plain assignment", []string{"A=1", "X=$(echo", "rm"}, [][]string{{"A=1", "X=$(echo", "rm"}, {"A=1", "rm"}}},
		{"stray paren closes early", []string{"y=$(echo", `")"`, ")", "rm"}, [][]string{{"y=$(echo", `")"`, ")", "rm"}, {"rm"}}},
		{"stray paren in an unclosed substitution", []string{"X=$(echo", `"$("`, ")", "rm"}, [][]string{{"X=$(echo", `"$("`, ")", "rm"}, {"rm"}, {`"$("`, ")", "rm"}}},
	} {
		got, ok := segmentReadings(tt.segment)
		if !ok || fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("%s: segmentReadings(%q) = %q (ok %v), want %q", tt.name, tt.segment, got, ok, tt.want)
		}
	}
}

// TestEnvAssignmentCloses pins the ")" tokens an assignment's substitution may
// close at, other than the close envAssignmentWidth reads (gt-lhn14).
func TestEnvAssignmentCloses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		segment []string
		want    []int
	}{
		{"plain assignment", []string{"FOO=1", "printf", "a)"}, nil},
		{"closed substitution", []string{"y=$(echo", "a)", "bash"}, nil},
		{"stray paren closes early", []string{"y=$(echo", `")"`, ")", "rm"}, []int{3}},
		{"never closes, paren later", []string{"y=$(echo", `"$("`, ")", "rm"}, []int{3}},
	} {
		if got := envAssignmentCloses(tt.segment); !slices.Equal(got, tt.want) {
			t.Errorf("%s: envAssignmentCloses(%q) = %v, want %v", tt.name, tt.segment, got, tt.want)
		}
	}
}

// TestSegmentReadingsRefusesTooMany: each leading assignment with a stray
// paren adds readings and they multiply, so a segment with enough of them is
// refused rather than walked part way (gt-lhn14).
func TestSegmentReadingsRefusesTooMany(t *testing.T) {
	t.Parallel()
	var segment []string
	for i := 0; i < 8; i++ {
		segment = append(segment, fmt.Sprintf("V%d=$(echo", i), `")"`, ")")
	}
	segment = append(segment, "rm", "-rf", "x")
	if _, ok := segmentReadings(segment); ok {
		t.Errorf("segmentReadings of %d stray-paren assignments: ok, want the segment refused", 8)
	}
}

func TestCdDestination(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		args    []string
		want    string
		found   bool
	}{
		{"plain", "cd", []string{"src"}, "src", true},
		{"no operand", "cd", nil, "", false},
		{"logical flag", "cd", []string{"-L", "src"}, "src", true},
		{"physical flag", "cd", []string{"-P", "src"}, "src", true},
		{"end of options", "cd", []string{"--", "-odd"}, "-odd", true},
		{"end of options with nothing after", "cd", []string{"--"}, "", false},
		{"previous directory", "cd", []string{"-"}, "-", true},
		{"pushd directory", "pushd", []string{"src"}, "src", true},
		{"pushd rotation", "pushd", []string{"+1"}, "", false},
		{"pushd -n flag", "pushd", []string{"-n", "src"}, "src", true},
		{"cd operand that starts with a plus", "cd", []string{"+x"}, "+x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := cdDestination(tc.command, tc.args)
			if got != tc.want || found != tc.found {
				t.Errorf("cdDestination(%q, %q) = (%q, %v), want (%q, %v)", tc.command, tc.args, got, found, tc.want, tc.found)
			}
		})
	}
}

func TestShellPayload(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		args    []string
		want    string
		found   bool
	}{
		{"bash -c", "bash", []string{"-c", "cd .. && ls"}, "cd .. && ls", true},
		{"sh -lc cluster", "sh", []string{"-lc", "ls"}, "ls", true},
		{"option with a value first", "bash", []string{"-o", "pipefail", "-c", "ls"}, "ls", true},
		{"set option off first", "bash", []string{"+o", "history", "-c", "ls"}, "ls", true},
		{"-c without a payload", "bash", []string{"-c"}, "", false},
		{"script file owns a later -c", "bash", []string{"run.sh", "-c", "ls"}, "", false},
		{"no -c", "bash", []string{"-x", "run.sh"}, "", false},
		{"long option is not a cluster", "bash", []string{"--norc", "ls"}, "", false},
		{"not a shell", "python3", []string{"-c", "print(1)"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := shellPayload(tc.command, tc.args)
			if got != tc.want || found != tc.found {
				t.Errorf("shellPayload(%q, %q) = (%q, %v), want (%q, %v)", tc.command, tc.args, got, found, tc.want, tc.found)
			}
		})
	}
}

// TestPolecatPathGuardBashCdChainIsBounded pins that a long chain of relative
// cds, each of which doubles the directories followCd tracks, is refused
// rather than followed without limit (gt-g2zd6).
func TestPolecatPathGuardBashCdChainIsBounded(t *testing.T) {
	t.Parallel()
	p := newPolecatTestTown(t)
	var steps []string
	for i := 0; i < 12; i++ {
		steps = append(steps, fmt.Sprintf("cd d%d", i))
	}
	chain := strings.Join(steps, " && ")
	if err := p.run(t, "Bash", commandInput(chain)); err == nil {
		t.Errorf("command %q: expected BLOCK for an untrackable cd chain, got allow", chain)
	}
	short := "cd d0 && cd d1 && cd d2 && ls"
	if err := p.run(t, "Bash", commandInput(short)); err != nil {
		t.Errorf("command %q: expected allow, got block: %v", short, err)
	}
}

// TestPolecatPathGuardBashCdSearchesCdpath pins that a bare relative cd operand
// is also judged under every $CDPATH entry, where the shell looks it up before
// the current directory.
func TestPolecatPathGuardBashCdSearchesCdpath(t *testing.T) {
	t.Parallel()
	// @POLECATS@ stands for <rig>/polecats in the fixture's town.
	const polecats = "@POLECATS@"
	cases := []struct {
		name      string
		cdpath    string
		command   string
		wantBlock bool
	}{
		{"absolute entry reaches a sibling polecat dir", polecats, "cd coral", true},
		{"relative entry reaches a sibling polecat dir", "../..", "cd coral", true},
		{"relative entry that stays in the polecat's own dir", "..", "cd coral", false},
		{"second entry reaches a sibling polecat dir", ".:" + polecats, "cd coral", true},
		{"entry with a trailing slash", polecats + "/", "cd coral", true},
		{"entry reaches a sibling, cd then rm", polecats, "cd coral/gastown && rm -rf internal.go", true},
		{"dot-slash operand skips the search", polecats, "cd ./internal && rm -rf build", false},
		{"dot-dot operand skips the search", polecats, "cd .. && ls", false},
		{"tilde operand skips the search", polecats, "cd ~ && ls", false},
		{"entry that holds no sibling", ".:", "cd internal && rm -rf build", false},
		{"empty CDPATH", "", "cd internal && rm -rf build", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPolecatTestTown(t)
			cdpath := strings.ReplaceAll(tc.cdpath, polecats, filepath.Join(p.rigRoot, "polecats"))
			p = p.withEnv("CDPATH", cdpath)
			err := p.run(t, "Bash", commandInput(tc.command))
			if tc.wantBlock && err == nil {
				t.Errorf("CDPATH=%q command %q: expected BLOCK, got allow", cdpath, tc.command)
			}
			if !tc.wantBlock && err != nil {
				t.Errorf("CDPATH=%q command %q: expected allow, got block: %v", cdpath, tc.command, err)
			}
		})
	}

	t.Run("CDPATH assigned on the line is refused for a searched operand", func(t *testing.T) {
		t.Parallel()
		p := newPolecatTestTown(t)
		for _, command := range []string{"CDPATH=../.. cd coral", "export CDPATH=../..; cd coral", "CDPATH=../..; cd coral && ls"} {
			if err := p.run(t, "Bash", commandInput(command)); err == nil {
				t.Errorf("command %q: expected BLOCK, got allow", command)
			}
		}
		if err := p.run(t, "Bash", commandInput("CDPATH=.. cd ./internal && ls")); err != nil {
			t.Errorf("a ./ operand is never searched, got block: %v", err)
		}
	})
}

func TestWrappedShell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		cmdline   []string
		wantShell string
		wantArgs  []string
		found     bool
	}{
		{"timeout", []string{"timeout", "5", "bash", "-c", "ls"}, "bash", []string{"-c", "ls"}, true},
		{"env with an option", []string{"env", "-i", "sh", "-c", "ls"}, "sh", []string{"-c", "ls"}, true},
		{"nice with an option", []string{"nice", "-n", "5", "zsh", "-c", "ls"}, "zsh", []string{"-c", "ls"}, true},
		{"shell by path", []string{"sudo", "/bin/bash", "-c", "ls"}, "bash", []string{"-c", "ls"}, true},
		{"leading assignment", []string{"A=1", "timeout", "5", "bash", "-c", "ls"}, "bash", []string{"-c", "ls"}, true},
		{"xargs", []string{"xargs", "-I{}", "sh", "-c", "ls {}"}, "sh", []string{"-c", "ls {}"}, true},
		{"find -exec", []string{"find", ".", "-exec", "sh", "-c", "ls", ";"}, "sh", []string{"-c", "ls", ";"}, true},
		{"find -execdir", []string{"find", ".", "-execdir", "bash", "-c", "ls", ";"}, "bash", []string{"-c", "ls", ";"}, true},
		{"find without -exec", []string{"find", ".", "-name", "bash"}, "", nil, false},
		{"wrapper without a shell", []string{"timeout", "5", "make", "build"}, "", nil, false},
		{"not a wrapper", []string{"echo", "bash", "-c", "ls"}, "", nil, false},
		{"only assignments", []string{"A=1"}, "", nil, false},
		{"empty", nil, "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell, args, found := wrappedShell(tc.cmdline)
			if shell != tc.wantShell || !slices.Equal(args, tc.wantArgs) || found != tc.found {
				t.Errorf("wrappedShell(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.cmdline, shell, args, found, tc.wantShell, tc.wantArgs, tc.found)
			}
		})
	}
}
