package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRigName is the rig directory name inside the fake town, named after the
// rig these tests live in so the fake layout reads like the real one.
const fakeRigName = "gastown"

// makeFakeTown builds a minimal on-disk town tree at town, so the scan-root
// classifier can be exercised against a real layout rather than a string that
// merely looks like one. Returns town.
//
// The layout mirrors a live town: the town root's own directories, one rig
// with that rig's standard subdirectories (polecats/, crew/, refinery/,
// witness/, mayor/ — see rig.AgentDirs), the rig's bare repo, a rig
// directory that holds no checkouts, one polecat worktree, and one checkout
// nested inside it.
func makeFakeTown(t *testing.T, town string) string {
	t.Helper()
	for _, d := range []string{
		"mayor",
		"logs",
		".dolt-data",
		filepath.Join(fakeRigName, "polecats", "lapis", "gastown", "internal"),
		filepath.Join(fakeRigName, "crew"),
		filepath.Join(fakeRigName, "refinery", "rig"),
		filepath.Join(fakeRigName, "witness"),
		filepath.Join(fakeRigName, "mayor", "rig"),
		filepath.Join(fakeRigName, "settings"),
		filepath.Join(fakeRigName, ".repo.git"),
	} {
		if err := os.MkdirAll(filepath.Join(town, d), 0o755); err != nil {
			t.Fatalf("building fake town: %v", err)
		}
	}
	return town
}

// fakeTown builds a fake town under a fresh temp dir.
func fakeTown(t *testing.T) string {
	t.Helper()
	return makeFakeTown(t, filepath.Join(t.TempDir(), "gt"))
}

func TestTownScanHazard(t *testing.T) {
	t.Parallel()
	town := fakeTown(t)
	rig := filepath.Join(town, fakeRigName)
	other := t.TempDir()

	tests := []struct {
		name     string
		scanRoot string
		want     string
	}{
		// Blocked — the town root and everything directly under it. A rig
		// root is the town's biggest single tree, and the town's other
		// level-1 directories are either large (.dolt-data, logs) or not
		// worth a recursive scan.
		{"town root", town, "the town root"},
		{"town root with dot suffix", town + string(filepath.Separator) + ".", "the town root"},
		{"town root in another case (case-insensitive host filesystem)", strings.ToUpper(town), "the town root"},
		{"rig root in another case", strings.ToUpper(rig), "a rig root"},
		{"rig root", rig, "a rig root"},
		{"town-level mayor", filepath.Join(town, "mayor"), "a rig root"},
		{"town-level logs", filepath.Join(town, "logs"), "a rig root"},
		{"town-level dolt data", filepath.Join(town, ".dolt-data"), "a rig root"},
		{"nonexistent direct child is still a town-level path", filepath.Join(town, "gastown-notes"), "a rig root"},

		// Blocked — a rig's aggregate directories hold many checkouts, so
		// scanning one walks every worktree of that kind at once.
		{"rig polecats (every worktree)", filepath.Join(rig, "polecats"), "a rig's worktree dir"},
		{"rig crew", filepath.Join(rig, "crew"), "a rig's worktree dir"},
		{"rig refinery", filepath.Join(rig, "refinery"), "a rig's worktree dir"},
		{"rig witness", filepath.Join(rig, "witness"), "a rig's worktree dir"},
		{"rig mayor", filepath.Join(rig, "mayor"), "a rig's worktree dir"},

		// Blocked — bare repos, at any depth.
		{"rig bare repo", filepath.Join(rig, ".repo.git"), "a .repo.git bare repo"},
		{"bare repo nested under a worktree", filepath.Join(rig, "polecats", "lapis", ".repo.git"), "a .repo.git bare repo"},

		// Allowed — one repo or one worktree is a bounded subtree.
		{"polecat worktree", filepath.Join(rig, "polecats", "lapis", "gastown"), ""},
		{"worktree subdir", filepath.Join(rig, "polecats", "lapis", "gastown", "internal"), ""},
		{"rig directory with no checkouts", filepath.Join(rig, "settings"), ""},
		{"rig mayor clone", filepath.Join(rig, "mayor", "rig"), ""},
		{"rig refinery clone", filepath.Join(rig, "refinery", "rig"), ""},

		// Allowed — outside the town, and the degenerate inputs.
		{"outside the town", other, ""},
		{"parent of the town", filepath.Dir(town), ""},
		{"filesystem root", "/", ""},
		{"empty scan root", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := townScanHazard(tt.scanRoot, town); got != tt.want {
				t.Errorf("townScanHazard(%q, %q) = %q, want %q", tt.scanRoot, town, got, tt.want)
			}
		})
	}
}

// TestTownScanHazardNoTown pins the fallback: with no town context ("") the
// town rule is inert, so the guard behaves exactly as it did before gt-6e2l
// for anything outside a Gas Town workspace.
func TestTownScanHazardNoTown(t *testing.T) {
	t.Parallel()
	town := fakeTown(t)
	for _, p := range []string{town, filepath.Join(town, fakeRigName), filepath.Join(town, fakeRigName, ".repo.git")} {
		if got := townScanHazard(p, ""); got != "" {
			t.Errorf("townScanHazard(%q, \"\") = %q, want no hazard", p, got)
		}
	}
}

func TestScanRootPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	t.Chdir(work)
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dot-prefixed *file*, the shape that used to read as a scan root: the
	// live town root has a .env, so `grep -rn .env src/` run from it would be
	// classified as a scan of <town>/.env without the directory check.
	if err := os.WriteFile(filepath.Join(work, ".env"), []byte("K=V\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"absolute path", "/Users/me/gt", "/Users/me/gt"},
		{"absolute trailing slash is cleaned", "/Users/me/gt/", "/Users/me/gt"},
		{"interior dot segments", "/Users/me/gt/../gt", "/Users/me/gt"},
		{"bare tilde", "~", home},
		{"tilde path", "~/gt", filepath.Join(home, "gt")},
		{"tilde with trailing slash", "~/gt/", filepath.Join(home, "gt")},
		{"bare $HOME", "$HOME", home},
		{"$HOME path", "$HOME/gt", filepath.Join(home, "gt")},
		{"braced ${HOME} path", "${HOME}/gt", filepath.Join(home, "gt")},
		{"dot", ".", work},
		{"dot slash", "./src", filepath.Join(work, "src")},
		{"parent", "..", filepath.Dir(work)},
		{"existing bare word", "src", filepath.Join(work, "src")},
		{"nonexistent bare word is a pattern, not a path", "TODO", ""},
		{"dot-prefixed file is a pattern, not a scan root", ".env", ""},
		{"dot-prefixed directory is a scan root", ".hidden", filepath.Join(work, ".hidden")},
		{"glob token is classified without existing on disk", "*/x", filepath.Join(work, "*/x")},
		{"flag", "-name", ""},
		{"long flag", "--recursive", ""},
		{"unresolvable variable", "$GT_SOMETHING", ""},
		{"unresolvable braced variable", "${GT_SOMETHING}/gt", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanRootPath(tt.token); got != tt.want {
				t.Errorf("scanRootPath(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

// TestScanPatternPath pins how a scan tool's pattern argument resolves: a bare
// relative name is not read as the directory that shares its spelling
// (gt-yts7), while a token that spells a path resolves as any other scan root
// does, so the denylist, home-directory and town-tree rules still see it.
func TestScanPatternPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	t.Chdir(work)
	if err := os.MkdirAll(filepath.Join(work, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token string
		want  string
	}{
		{"bare name that exists is a pattern, not a path", "src", ""},
		{"bare name that does not exist", "TODO", ""},
		{"dot-prefixed name is a pattern", ".hidden", ""},
		{"dot segment", ".", work},
		{"dot slash", "./src", filepath.Join(work, "src")},
		{"relative path below cwd", "src/nested", filepath.Join(work, "src", "nested")},
		{"absolute path", "/Users/me/gt", "/Users/me/gt"},
		{"tilde path", "~/gt", filepath.Join(home, "gt")},
		{"glob", "*/x", filepath.Join(work, "*/x")},
		{"unresolvable variable", "$GT_SOMETHING", ""},
		{"flag", "-name", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scanPatternPath(tt.token); got != tt.want {
				t.Errorf("scanPatternPath(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

// TestTownScanHazardHomeOverride pins that a town reached through ~ or $HOME
// is classified the same as the town reached by its absolute path. The
// denylist's existing ~/$HOME entries name the *home* directory, so the
// unexpanded ~/gt was the obvious way around the town rule.
func TestTownScanHazardHomeOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	t.Chdir(town)

	for _, token := range []string{
		town,
		"~/gt",
		"$HOME/gt",
		filepath.Join(home, "gt", fakeRigName),
		"~/gt/" + fakeRigName + "/.repo.git",
	} {
		resolved := scanRootPath(token)
		if resolved == "" {
			t.Fatalf("scanRootPath(%q) did not resolve", token)
		}
		if got := townScanHazard(resolved, town); got == "" {
			t.Errorf("token %q (resolved %q) was not a town hazard", token, resolved)
		}
	}
}

// TestMatchesUnboundedScanTownTree is the end-to-end check of the town rule
// on the guard's real entry point: the shapes that must block — gt-6e2l's
// `grep -R ... /Users/sloan/gt` above all — and, just as important, the
// shapes that must keep working. A fix that blocks the first set by blocking
// scans generally would be a worse bug than the one being fixed.
func TestMatchesUnboundedScanTownTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	rig := filepath.Join(town, fakeRigName)
	worktree := filepath.Join(rig, "polecats", "lapis", "gastown")
	// The guard resolves relative tokens (and the town root) against cwd, so
	// run from inside a polecat worktree where a real polecat session sits.
	t.Chdir(worktree)

	other := t.TempDir()

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The incident, verbatim shape and its neighbours.
		{"grep -R over the town root", `grep -R "record-run" -n ` + town, true},
		{"grep -rn over the town root", `grep -rn TODO ` + town, true},
		{"rg over the town root", `rg TODO ` + town, true},
		{"find over the town root", `find ` + town + ` -name x`, true},
		{"du over a rig root", `du -sh ` + rig, true},
		{"ls -R over a rig root", `ls -R ` + rig, true},
		{"grep -R over a rig's polecats", `grep -R TODO ` + filepath.Join(rig, "polecats"), true},
		{"find over a rig's bare repo", `find ` + filepath.Join(rig, ".repo.git") + ` -name x`, true},
		{"town root via tilde", `find ~/gt -name x`, true},
		{"town root via $HOME", `grep -r TODO $HOME/gt`, true},
		{"town root via relative escape", `grep -r TODO ../../../..`, true},
		{"glob over the town's rig roots", `grep -R TODO ` + town + `/*`, true},

		// Allowed — bounded subtrees and unrelated paths.
		{"scan of this worktree", `grep -rn TODO .`, false},
		{"scan of a worktree subdir", `grep -rn TODO ./internal`, false},
		{"scan of one worktree by absolute path", `grep -rn TODO ` + worktree, false},
		{"scan of the rig's mayor clone", `grep -rn TODO ` + filepath.Join(rig, "mayor", "rig"), false},
		{"scan of a rig directory with no checkouts", `grep -rn TODO ` + filepath.Join(rig, "settings"), false},
		{"find in an unrelated temp dir", `find ` + other + ` -name x`, false},
		{"grep for a pattern that collides with a town directory name", `grep -rn logs ` + other, false},
		{"scan of a sibling of the town", `grep -rn TODO ` + filepath.Join(home, "elsewhere"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), town)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesUnboundedScan(%q, town) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if tt.blocked && alternative == "" {
				t.Errorf("matchesUnboundedScan(%q, town) blocked but returned no alternative text", tt.command)
			}
		})
	}
}

// TestMatchesUnboundedScanPatternNotRoot pins the gt-yts7 false positive on
// the guard's real entry point: a scan tool's search pattern is not a scan
// root, so a pattern spelled like a directory at cwd is searched for, not
// walked. cwd is the rig root, where polecats/, crew/, refinery/, witness/,
// mayor/ and the rig's bare repo are each one bare word away.
//
// The boundary cases carry equal weight. The pattern slot is read that way
// only when another argument names the path the walk starts from; a bare word
// anywhere else is still a path; the tools that take no pattern keep every
// argument a path; and an option that supplies the pattern leaves every
// positional argument a path, so a pattern-consuming flag cannot smear a root
// into the pattern slot (gt-yts7's rejection).
func TestMatchesUnboundedScanPatternNotRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	rig := filepath.Join(town, fakeRigName)
	t.Chdir(rig)

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Allowed — the reported false positive, and the same shape for a
		// pattern colliding with any other directory at the rig root.
		{"grep for a pattern colliding with polecats/", "grep -rn polecats ./settings", false},
		{"grep for a pattern colliding with crew/", "grep -rn crew ./settings", false},
		{"grep for a pattern colliding with the bare repo name", "grep -rn .repo.git ./settings", false},
		{"the reported shape", "grep -rn polecats ./docs", false},
		{"rg for a colliding pattern", "rg polecats ./settings", false},
		{"ag for a colliding pattern", "ag polecats ./settings", false},
		{"fd for a colliding pattern", "fd polecats ./settings", false},

		// Allowed — the pattern is behind the tool's own flag grammar, which
		// must not shift the slot onto the pattern's neighbour.
		{"grep -e supplies the pattern", "grep -rn -e polecats ./settings", false},
		{"a short bundle carrying -e", "grep -rne polecats ./settings", false},
		{"a context flag's value is not the pattern", "grep -rn -A 3 polecats ./settings", false},
		{"rg's context flag", "rg -A 3 polecats ./settings", false},
		{"rg's count flag with an inline value", "rg -A3 polecats ./settings", false},
		{"ag's count flag, which takes only a number", "ag -A 3 polecats ./settings", false},
		{"rg's type filter", "rg -t go polecats ./settings", false},
		{"rg's glob filter", "rg -g *.md polecats ./settings", false},
		{"rg's inline long value", "rg --iglob=*.md polecats ./settings", false},
		{"rg's command-valued option", "rg --pre cat polecats ./settings", false},
		{"fd's extension filter", "fd -e go polecats ./settings", false},
		{"fd's path option names the path", "fd --search-path ./settings polecats", false},
		{"a dash-led pattern escaped with --", "grep -rn -- --recursive ./settings", false},
		{"a pattern above a bounded path", "grep -rn polecats ./mayor/rig", false},

		// Blocked — the pattern slot is the only place a name is read that
		// way. A bare word after the pattern, a pattern that spells a path,
		// and an invocation naming no path at all all stay roots.
		{"a bare word after the pattern is a path", "grep -rn TODO polecats ./settings", true},
		{"a bare word with no path argument", "grep -rn TODO polecats", true},
		{"a bare word under a non-pattern tool", "find polecats -name x", true},
		{"du over an aggregate directory", "du -sh polecats", true},
		{"ls -R over an aggregate directory", "ls -R polecats", true},
		{"a path spelling at the pattern slot", "grep -rn polecats .", true},
		{"a path below the aggregate directory", "grep -rn TODO ./polecats", true},
		{"--files leaves the tool no pattern", "rg --files polecats ./settings", true},

		// Blocked — an option's argument is judged like any other argument,
		// so a bare word there stays a path. That is the direction a wrong
		// grammar errs in, and it keeps a value-reading from sparing a root:
		// `fd --exclude polecats` excludes a directory, but the guard cannot
		// tell that from a path it was about to walk.
		{"an option's argument that collides with a directory is still a path", "fd --exclude polecats ./settings", true},
		{"a glob filter's argument that collides", "rg -g polecats ./settings", true},
		{"a path option's bare-name value", "fd --search-path polecats ./settings", true},

		// Blocked — a flag that supplies the pattern makes every positional
		// argument a path, wherever the flag sits (the gt-yts7 rejection:
		// these must not slip through as the pattern).
		{"-e before a relative path", "rg -e TODO polecats ./settings", true},
		{"-e before an absolute root", "rg -e TODO " + rig, true},
		{"-e before the town root", "rg -e TODO " + town, true},
		{"-e after a path", "rg polecats -e TODO ./settings", true},
		{"-f supplies the pattern from a file", "rg -f pats polecats ./settings", true},

		// Blocked — an option no grammar covers takes no value, so its
		// argument is the pattern and the pattern it displaces is a path.
		{"an unrecognized option re-blocks the pattern it displaces", "rg --no-such-option TODO polecats ./settings", true},

		// Blocked — the spelling rules still judge the pattern slot, which is
		// what keeps a misread root reachable.
		{"a pattern spelled as the filesystem root", "rg / ./settings", true},
		{"a pattern spelled as the town root", "rg " + town + " ./settings", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), town)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesUnboundedScan(%q, town) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if tt.blocked && alternative == "" {
				t.Errorf("matchesUnboundedScan(%q, town) blocked but returned no alternative text", tt.command)
			}
		})
	}
}

// TestMatchesUnboundedScanPatternNotRootFromTownRoot is the same rule one
// level up, where the names a pattern can collide with are the town's own
// children: every rig directory, and the town's logs/ and .dolt-data/.
func TestMatchesUnboundedScanPatternNotRootFromTownRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	rig := filepath.Join(town, fakeRigName)
	bounded := filepath.Join(rig, "settings")
	t.Chdir(town)

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"grep for the rig name", "grep -rn " + fakeRigName + " " + bounded, false},
		{"grep for the town's log directory name", "grep -rn logs " + bounded, false},
		{"grep for the dolt data directory name", "grep -rn .dolt-data " + bounded, false},
		{"rg for the rig name", "rg " + fakeRigName + " " + bounded, false},
		{"fd for the rig name", "fd " + fakeRigName + " " + bounded, false},

		{"a rig directory is a root wherever it sits", "grep -rn TODO " + fakeRigName + " " + bounded, true},
		{"a rig directory with no path argument", "grep -rn TODO " + fakeRigName, true},
		{"a rig directory under a pattern flag", "rg -e TODO " + fakeRigName, true},
		{"du over a rig directory", "du -sh " + fakeRigName, true},
		{"ls -R over a rig directory", "ls -R " + fakeRigName, true},
		{"--files leaves the tool no pattern", "rg --files " + fakeRigName + " " + bounded, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), town)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesUnboundedScan(%q, town) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if tt.blocked && alternative == "" {
				t.Errorf("matchesUnboundedScan(%q, town) blocked but returned no alternative text", tt.command)
			}
		})
	}
}

// TestNestedTownScanPayloadIsBlocked covers the shapes only the recursive
// evaluator can see: a town-root scan hidden inside bash -c, eval, or a
// command substitution. matchesUnboundedScan judges one token list and never
// recurses, so these belong against evaluateDangerousCommand — the function
// that unrolls shell wrappers and threads the resolved town root down to each
// payload, so a wrapper cannot be used to escape the town rule (gt-6e2l).
func TestNestedTownScanPayloadIsBlocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	worktree := filepath.Join(town, fakeRigName, "polecats", "lapis", "gastown")
	t.Chdir(worktree)

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"bash -c payload", `bash -c "grep -R TODO ` + town + `"`, true},
		{"sh -c payload", `sh -c "find ` + town + ` -name x"`, true},
		{"eval payload", `eval "grep -rn TODO ` + town + `"`, true},
		{"command substitution", "echo $(grep -r TODO " + town + ")", true},
		{"backtick substitution", "echo `grep -r TODO " + town + "`", true},

		// Allowed — the same wrappers around a bounded root.
		{"wrapped scan of one worktree", `bash -c "grep -rn TODO ."`, false},
		{"wrapped scan outside the town", `bash -c "grep -rn TODO ` + filepath.Join(home, "elsewhere") + `"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, town)
			if got := reason != ""; got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q, town) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// TestRunTapGuardDangerousBlocksTownScan drives the guard the way Claude Code
// does — hook-input JSON on stdin, cwd inside a town — so the whole path is
// covered at once: read the command, resolve the town root from cwd,
// classify the scan root, block with exit 2. The unit tests above pass a town
// root in explicitly; this is the test that fails if resolving the town from
// a real session's cwd breaks, which would leave a correctly-classifying
// guard blocking nothing in production (gt-6e2l is exactly that shape of
// gap: correct rules, wrong tree).
func TestRunTapGuardDangerousBlocksTownScan(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Pin cwd resolution: these two are the fallback path, not what is under
	// test here, and the environment may legitimately have them set.
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("GT_ROOT", "")

	town := makeFakeTown(t, filepath.Join(home, "gt"))
	townJSON := filepath.Join(town, "mayor", "town.json")
	if err := os.WriteFile(townJSON, []byte(`{"type":"town","version":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(town, fakeRigName, "polecats", "lapis", "gastown"))

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"grep -R over the town root (gt-6e2l)", `grep -R "record-run" -n ` + town, true},
		{"find over a rig root", `find ` + filepath.Join(town, fakeRigName) + ` -name x`, true},
		{"grep -R over a rig's polecats", `grep -R TODO ` + filepath.Join(town, fakeRigName, "polecats"), true},
		{"town root reached through a shell wrapper", `bash -c "rg TODO ` + town + `"`, true},
		{"scan of this one worktree stays allowed", `grep -rn TODO .`, false},
		{"scan outside the town stays allowed", `grep -rn TODO ` + filepath.Join(home, "elsewhere"), false},
		{"scan of the literal home directory is blocked", `grep -rn TODO ` + home, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := json.Marshal(map[string]any{
				"hook_event_name": "PreToolUse",
				"tool_name":       "Bash",
				"tool_input":      map[string]string{"command": tt.command},
			})
			if err != nil {
				t.Fatal(err)
			}

			var guardErr error
			stderr := captureStderr(t, func() {
				withStdin(t, string(input), func() {
					guardErr = runTapGuardDangerous(tapGuardDangerousCmd, nil)
				})
			})

			code, isSilentExit := IsSilentExit(guardErr)
			if (guardErr != nil) != tt.blocked {
				t.Fatalf("guard err=%v (silent exit %d), want blocked=%v (stderr: %s)", guardErr, code, tt.blocked, stderr)
			}
			if tt.blocked {
				if !isSilentExit || code != 2 {
					t.Errorf("blocked with err=%v, want a silent exit 2 (stderr: %s)", guardErr, stderr)
				}
				if !strings.Contains(stderr, "DANGEROUS COMMAND BLOCKED") {
					t.Errorf("blocked %q without printing the block banner; stderr: %s", tt.command, stderr)
				}
			} else if strings.Contains(stderr, "DANGEROUS COMMAND BLOCKED") {
				t.Errorf("allowed %q but printed a block banner; stderr: %s", tt.command, stderr)
			}
		})
	}
}

// TestScanRootPathWithoutHome pins the failure mode an unset HOME would
// otherwise create: "~/gt" must resolve to nothing, not silently become
// "<cwd>/gt", which could point into an unrelated tree and turn a blocked
// scan into an allowed one (or the reverse).
func TestScanRootPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Chdir(t.TempDir())

	for _, token := range []string{"~", "~/gt", "$HOME", "$HOME/gt", "${HOME}/gt"} {
		if got := scanRootPath(token); got != "" {
			t.Errorf("scanRootPath(%q) with no HOME = %q, want \"\"", token, got)
		}
	}
}
