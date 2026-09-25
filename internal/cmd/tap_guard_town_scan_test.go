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

// TestMatchesUnboundedScanPatternNotRoot pins gt-yts7 from both sides: a
// search pattern is not a scan root — except when the invocation has no root
// argument anywhere, in which case it is one — and detecting the pattern must
// not skip the root that follows it.
//
// The false positive it fixes: scanRootPath reads a relative token as a path
// whenever the directory it names exists, so `grep -rn polecats ./elsewhere`
// run from a rig root — which really does hold a polecats/ directory — was
// blocked as a scan of that directory.
//
// The bypasses the exemption could open, and which the blocked cases here
// pin: fd reads a lone positional argument that names an existing path as
// its search root rather than its pattern (`fd /`, `fd $HOME`), fd's
// path-taking flags and rg --files carry paths instead of a pattern, a
// value-taking flag's argument is part of the flag (so `grep -A 3 polecats
// /` does not hand the pattern slot to the root), and a pattern escaped
// with "--" must not hand the pattern slot to the root argument behind it.
func TestMatchesUnboundedScanPatternNotRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	town := makeFakeTown(t, filepath.Join(home, "gt"))
	rig := filepath.Join(town, fakeRigName)
	worktree := filepath.Join(rig, "polecats", "lapis", "gastown")
	other := t.TempDir()

	tests := []struct {
		name    string
		cwd     string
		command string
		blocked bool
	}{
		// The pattern names a directory that exists at cwd, and the root
		// argument behind it is bounded — the gt-yts7 false positive.
		{"grep pattern collides with a rig agent dir", rig, `grep -rn polecats ` + other, false},
		{"rg pattern collides with a rig agent dir", rig, `rg polecats ` + other, false},
		{"ag pattern collides with a rig agent dir", rig, `ag polecats ` + other, false},
		{"fd pattern collides with a rig agent dir", rig, `fd polecats ` + other, false},
		{"grep pattern collides with the rig name", town, `grep -rn ` + fakeRigName + ` ` + other, false},
		// fd alone in a cwd with no such entry: the pattern is not a root.
		{"fd pattern with no path argument", other, `fd polecats`, false},
		// fd alone in a cwd that does hold such an entry: it is one.
		{"fd pattern with no path argument over a rig agent dir", rig, `fd polecats`, true},

		// fd's pattern is optional: an existing lone path is its search root,
		// so these must still reach the root checks.
		{"fd rooted at /", worktree, `fd /`, true},
		{"fd rooted at ~", worktree, `fd ~`, true},
		{"fd rooted at the home directory", worktree, `fd $HOME`, true},
		{"fd rooted at /Users", worktree, `fd /Users`, true},
		{"fd rooted at the town tree", worktree, `fd --hidden ` + town, true},
		{"fd rooted at a rig's agent dir", rig, `fd polecats`, true},

		// fd's path-taking flags supply the paths to walk, so their value is
		// the root and the pattern argument after it is still the pattern.
		{"fd --search-path over /", worktree, `fd --search-path / foo`, true},
		{"fd -C over the town tree", worktree, `fd -C ` + town + ` foo`, true},
		{"fd --search-path over a rig agent dir", rig, `fd --search-path polecats foo`, true},
		{"fd --search-path before a bounded pattern", worktree, `fd --search-path ./internal foo`, false},

		// rg --files searches paths without a pattern.
		{"rg --files over /", worktree, `rg --files /`, true},
		{"rg --files over the town tree", worktree, `rg --files ` + town, true},

		// A pattern escaped with "--" is positional, so the root behind it is
		// still the root.
		{"grep dash-led pattern before /", worktree, `grep -rn -- --recursive /`, true},
		{"grep dash-led pattern before the town tree", worktree, `grep -rn -- --recursive ` + town, true},
		{"grep dash-led pattern before a bounded root", worktree, `grep -rn -- --recursive ./internal`, false},
		// "--" settles what the slot holds, so a path spelled there is the
		// pattern and this searches ./internal rather than the town.
		{"path spelled as the pattern after --", worktree, `grep -rn -- ` + town + ` ./internal`, false},

		// The root argument *after* the pattern is what the guard is for.
		{"grep root after a pattern", rig, `grep -rn TODO polecats`, true},
		{"rg root after a pattern", rig, `rg TODO polecats`, true},
		{"grep root after a dash-led pattern", rig, `grep -rn -- TODO polecats`, true},

		// A value-taking flag's argument is part of the flag, not the first
		// positional — reading it as a positional shifts what the guard
		// examines (gt-yts7 rejection): the phantom value would swallow the
		// pattern slot when the value is dash-led, and re-flag the real
		// pattern when the value is not.
		{"grep value-flag then a bounded pattern", worktree, `grep -rn -A 3 polecats ./internal`, false},
		{"grep value-flag then /", worktree, `grep -rn -A 3 polecats /`, true},
		{"grep value-flag over a rig agent dir", rig, `grep -rn -A 3 polecats /`, true},
		{"rg dash-led value then a bounded root", worktree, `rg -rn -e --foo ./internal`, false},
		{"rg dash-led value then /", worktree, `rg -e --foo /`, true},
		{"rg -f value then a bounded root", worktree, `rg -rn -f patterns ./internal`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.cwd)
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
