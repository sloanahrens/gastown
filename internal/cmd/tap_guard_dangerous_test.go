package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

func TestExtractCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid hook input", `{"tool_name":"Bash","tool_input":{"command":"rm -rf /tmp/foo"}}`, "rm -rf /tmp/foo"},
		{"empty input", "", ""},
		{"invalid json", "not json", ""},
		{"no command field", `{"tool_name":"Write","tool_input":{"file_path":"/tmp/foo"}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCommand([]byte(tt.input))
			if got != tt.want {
				t.Errorf("extractCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMatchesAllFragments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		command   string
		fragments []string
		want      bool
	}{
		{"git reset hard", "git reset --hard", []string{"git", "reset", "--hard"}, true},
		{"git reset soft", "git reset --soft", []string{"git", "reset", "--hard"}, false},
		{"drop table", "drop table users", []string{"drop", "table"}, true},
		{"drop database", "drop database mydb", []string{"drop", "database"}, true},
		{"truncate table", "truncate table logs", []string{"truncate", "table"}, true},
		{"git clean -f", "git clean -f", []string{"git", "clean", "-f"}, true},
		{"git clean -n", "git clean -n", []string{"git", "clean", "-f"}, false},
		{"no match", "echo hello", []string{"rm", "-rf"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesAllFragments(lowerTokens(tt.command), tt.fragments)
			if got != tt.want {
				t.Errorf("matchesAllFragments(%q, %v) = %v, want %v", tt.command, tt.fragments, got, tt.want)
			}
		})
	}
}

func TestMatchesDangerousRmRf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block
		{"rm -rf /", "rm -rf /", true},
		{"rm -rf /*", "rm -rf /*", true},
		{"rm -rf / with sudo", "sudo rm -rf /", true},

		// Should allow (normal cleanup commands)
		{"rm -rf ./build/", "rm -rf ./build/", false},
		{"rm -rf node_modules/", "rm -rf node_modules/", false},
		{"rm -rf /tmp/test-output/", "rm -rf /tmp/test-output/", false},
		{"rm -rf relative dir", "rm -rf build", false},
		{"rm single file", "rm foo.txt", false},
		{"rm -r no force", "rm -r /", false},
		{"no rm at all", "echo hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesDangerousRmRf(lowerTokens(tt.command)) != ""
			if got != tt.blocked {
				t.Errorf("matchesDangerousRmRf(%q) blocked=%v, want %v", tt.command, got, tt.blocked)
			}
		})
	}
}

func TestMatchesDangerousGitPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block
		{"git push --force", "git push --force origin main", true},
		{"git push -f", "git push -f origin main", true},
		{"git push --force bare", "git push --force", true},

		// Should allow (safe variants)
		{"force-with-lease", "git push --force-with-lease origin main", false},
		{"force-if-includes", "git push --force-if-includes origin main", false},
		{"normal push", "git push origin main", false},
		{"no push", "git status", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesDangerousGitPush(lowerTokens(tt.command)) != ""
			if got != tt.blocked {
				t.Errorf("matchesDangerousGitPush(%q) blocked=%v, want %v", tt.command, got, tt.blocked)
			}
		})
	}
}

// TestMatchesDangerousGitReset pins the gt-63sz guard: resetting onto a
// remote-tracking ref is the squash-onto-a-fresh-base habit that silently
// encodes a revert of everything merged since the checkout was cut. Every mode
// is blocked (the implicit --mixed of a bare `git reset origin/main` included);
// every reset to a local ref or a pathspec still works, since that is the
// ordinary squash and unstage vocabulary.
func TestMatchesDangerousGitReset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — any reset whose target is a remote-tracking ref.
		{"reset --soft origin/main", "git reset --soft origin/main", true},
		{"reset --mixed origin/main", "git reset --mixed origin/main", true},
		{"reset --hard origin/main", "git reset --hard origin/main", true},
		{"bare reset origin/main (implicit --mixed)", "git reset origin/main", true},
		{"reset onto upstream/main", "git reset --soft upstream/main", true},
		{"reset onto long-form remote ref", "git reset --soft refs/remotes/origin/main", true},
		{"glued to a shell operator", "git add -A; git reset --soft origin/main", true},

		// Allowed — local targets, pathspecs, and quoted prose.
		{"soft to a local commit", "git reset --soft HEAD~1", false},
		{"mixed to a local branch", "git reset --mixed feature/x", false},
		{"reset a pathspec named like a ref", "git reset -- origin/main", false},
		{"unstage a file", "git reset HEAD file.go", false},
		{"no reset at all", "git status", false},
		{"ref name inside quoted prose", `gt mail send x -s "note" -m "never git reset --soft origin/main"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := matchesDangerousGitReset(lowerTokens(tt.command))
			if blocked := got != ""; blocked != tt.blocked {
				t.Errorf("matchesDangerousGitReset(%q) blocked=%v, want %v", tt.command, blocked, tt.blocked)
			}
		})
	}
}

// TestMatchesGitClean pins the gt-775d fix: the block fires on a git clean
// invocation carrying a force flag, matched as an invocation, and never on the
// words merely appearing somewhere in a compound line.
func TestMatchesGitClean(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — a real force clean.
		{"git clean -f", "git clean -f", true},
		{"git clean -fd", "git clean -fd", true},
		{"git clean -fdx", "git clean -fdx", true},
		{"git clean -df", "git clean -df", true},
		{"git clean -xdf", "git clean -xdf", true},
		{"force flag before a dry-run flag", "git clean -f -n", true},
		{"git clean -n -f", "git clean -n -f", true},
		{"glued to a shell operator", "cd /tmp && git clean -fdx", true},
		{"behind a leading env assignment", "GIT_DIR=.git git clean -fd", true},
		{"behind git's -C option", "git -C /tmp/repo clean -fd", true},
		{"absolute path to git", "/usr/bin/git clean -fd", true},
		{"invoked through a launcher", "time git clean -fd", true},
		{"handed to xargs", "find . -name '*.tmp' -print | xargs git clean -f", true},

		// Allowed — git clean without a force flag.
		{"dry run", "git clean -n", false},
		{"dry run of a directory clean", "git clean -nd", false},
		{"ignored files only", "git clean -X", false},
		{"bare git clean", "git clean", false},

		// Allowed — the words are present but no git clean runs.
		{"clean spelled in an echo argument", `echo "clean"`, false},
		{"git clean named inside a quoted echo argument", `echo "git clean -f"`, false},
		{"git clean named as an unquoted echo argument", "echo git clean -f", false},
		{"git clean named in an unquoted gt mail argument", "gt mail send x -s note -m never run git clean -f", false},
		{"git clean named inside a quoted grep pattern", `grep -rn "git clean -f" docs/`, false},
		{"force flag from a probe on another command", `test -f .git/MERGE_HEAD && echo "clean"`, false},
		{"git subcommand other than clean", "git status -f", false},
		{"force flag owned by a later command in the same segment", "git clean -n; git push -f origin main", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesGitClean(shellTokenize(tt.command))
			if blocked := got != ""; blocked != tt.blocked {
				t.Errorf("matchesGitClean(%q) blocked=%v (%q), want %v", tt.command, blocked, got, tt.blocked)
			}
		})
	}
}

// TestGitCleanMentionOnACompoundLineIsAllowed pins gt-775d end to end: the
// three refinery merge commands the guard rejected are ordinary git work, and
// each must pass the whole dangerous-command evaluation. Their fail-closed
// cost was a wasted round trip on the merge hot path, four times in one night.
func TestGitCleanMentionOnACompoundLineIsAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{
			"merge with a conflict probe",
			`git merge --no-ff -m "Merge origin/main into main" origin/main 2>&1|tail -12; test -f .git/MERGE_HEAD && echo "CONFLICT" || echo "clean"`,
		},
		{
			"merge preceded by a branch and log read",
			`git rev-parse --abbrev-ref HEAD; git log --oneline -1; echo "=== merge origin/main in ==="; git merge --no-ff -m "Merge origin/main into main" origin/main 2>&1|tail -25; echo "=== conflict? ==="; test -f .git/MERGE_HEAD && echo "CONFLICT STATE" || echo "clean"`,
		},
		{
			"rehearsal merge in a scratch branch",
			`git ls-remote origin refs/heads/polecat/garnet/gt-2wqt+mu7h1he7 2>&1; echo "declared: 3fd09db..."; echo "=== rehearse ==="; git branch -D temp 2>/dev/null; git checkout -b temp origin/polecat/garnet/gt-2wqt+mu7h1he7 2>&1|tail -2; git merge --no-ff --no-edit origin/main 2>&1|tail -8; test -f .git/MERGE_HEAD && echo "CONFLICT" || echo "clean"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (%q), want allowed", tt.command, reason)
			}
		})
	}
}

func TestMatchesSudo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block
		{"sudo dnf install", "sudo dnf install -y postgresql-contrib", true},
		{"sudo rm", "sudo rm -rf /var/log/syslog", true},
		{"sudo bare", "sudo su", true},
		{"sudo in pipeline", "echo foo | sudo tee /etc/config", true},

		// Should allow
		{"no sudo", "echo hello", false},
		{"sudo inside quoted text", "echo 'do not use sudo'", false}, // gt-mkrj: quoted text is opaque, not a real argv word
		{"pseudocode", "cat pseudocode.txt", false},
		{"sudo substring", "echo pseudocode", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesSudo(lowerTokens(tt.command)) != ""
			if got != tt.blocked {
				t.Errorf("matchesSudo(%q) blocked=%v, want %v", tt.command, got, tt.blocked)
			}
		})
	}
}

func TestMatchesPackageInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block
		{"apt install", "apt install -y curl", true},
		{"apt-get install", "apt-get install -y build-essential", true},
		{"dnf install", "dnf install -y postgresql-contrib", true},
		{"yum install", "yum install -y gcc", true},
		{"brew install", "brew install node", true},
		{"gem install", "gem install bundler", true},
		{"pip install --system", "pip install --system requests", true},
		{"pip3 install --system", "pip3 install --system flask", true},
		{"npm install -g", "npm install -g typescript", true},
		{"npm install --global", "npm install --global eslint", true},

		// Should allow
		{"pip install (venv ok)", "pip install requests", false},
		{"npm install (local ok)", "npm install express", false},
		{"npm install --save-dev", "npm install --save-dev jest", false},
		{"go install", "go install ./...", false},
		{"cargo install", "cargo install ripgrep", false},
		{"normal command", "ls -la", false},
		// gt-mkrj: "apt" must not fire as a substring of an ordinary word.
		{"capture substring", "tmux capture-pane -p", false},
		{"adapt substring", "echo adapt this script", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesPackageInstall(lowerTokens(tt.command)) != ""
			if got != tt.blocked {
				t.Errorf("matchesPackageInstall(%q) blocked=%v, want %v", tt.command, got, tt.blocked)
			}
		})
	}
}

// TestMatchesPacmanInstall pins pacman's case-sensitive -S (install) vs -s
// (search modifier) distinction: a generic case-insensitive bundled-flag
// match blocked "pacman -Ss foo" and "pacman -Qs foo" (both read-only
// searches, since a lowercase 's' anywhere in the cluster negates the
// capital 'S' sync/install meaning) as if they were installs
// (finding 7, gt-wisp-db27).
func TestMatchesPacmanInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"pacman -S install", "pacman -S git", true},
		{"pacman -Sy install", "pacman -Sy git", true},
		{"pacman -Syu upgrade", "pacman -Syu", true},
		{"pacman -Ss search (not install)", "pacman -Ss git", false},
		{"pacman -Qs query search (not install)", "pacman -Qs git", false},
		{"pacman -Q query (not install)", "pacman -Q", false},
		{"no pacman", "echo -S hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			got := matchesPacmanInstall(tokens, lowerTokens(tt.command))
			if got != tt.blocked {
				t.Errorf("matchesPacmanInstall(%q) blocked=%v, want %v", tt.command, got, tt.blocked)
			}
		})
	}
}

// TestNestedShellCommands pins the fix for wrappers that shlex tokenization
// otherwise hides from every fragment-based check: a quoted payload to
// bash -c/sh -c/eval collapses to a single token, so exact-token fragment
// matching never fires on it unless evaluateDangerousCommand recurses into
// the nested command text (finding 4, gt-wisp-db27).
func TestNestedShellCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"bash -c wrapped git reset --hard", `bash -c "git reset --hard"`, true},
		{"bash -c wrapped reset onto origin/main", `bash -c "git reset --soft origin/main"`, true},
		{"sh -c wrapped sudo", `sh -c "sudo rm -rf /"`, true},
		{"eval wrapped git clean -fd", `eval "git clean -fd"`, true},
		{"bash -c safe command", `bash -c "echo hello"`, false},
		{"nested but safe", `sh -c "ls -la"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// TestQuotedSQLStaysOpaque pins mayor scope for gt-5ihs attempt 2: "SQL DDL
// inside quotes is not a shell hazard; do not flag it." A quoted SQL
// payload (dolt sql -q "...", psql -c "...", mysql -e '...') must NOT be
// scanned for DDL fragments — every real false positive this guard has hit
// (bead ids, mail bodies, a package-manager name inside an ordinary word)
// came from treating quoted prose as command text, and SQL strings are the
// same category of risk, not a shell hazard like bash -c/eval.
func TestQuotedSQLStaysOpaque(t *testing.T) {
	t.Parallel()
	tests := []string{
		`dolt sql -q "DROP TABLE issues"`,
		`psql -c "TRUNCATE TABLE users"`,
		`mysql -e "DROP DATABASE prod"`,
		`dolt sql -q "SELECT * FROM issues"`,
	}
	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(command, 0, "")
			if reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — quoted SQL must stay opaque", command, reason)
			}
		})
	}
}

// TestCommandSubstitutionRecursion pins mayor scope for gt-5ihs attempt 2:
// recurse into $(...) and `...` command substitution, since these really do
// execute as shell (unlike a quoted SQL string or mail body).
func TestCommandSubstitutionRecursion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"dollar-paren wraps git reset --hard", `echo $(git reset --hard)`, true},
		{"backtick wraps sudo", "echo `sudo rm -rf /`", true},
		{"dollar-paren safe", `echo $(ls -la)`, false},
		{"backtick safe", "echo `date`", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

func TestMatchesUnboundedScan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block — the gt-nqcy incident and its siblings.
		{"bfs / with -S flag before root", "bfs -S dfs / -name regex.h -path *unicode*", true},
		{"find /", "find / -name x", true},
		{"find /*", "find /* -name x", true},
		{"find $HOME", "find $HOME -iname foo", true},
		{"find ~", "find ~ -name foo", true},
		{"find /Users", "find /Users -name x", true},
		{"find /System", "find /System -name x", true},
		{"find /Library", "find /Library -name x", true},
		{"find /opt exact", "find /opt -name x", true},
		{"fd rooted at root", "fd regex.h /", true},
		{"rg rooted at root", "rg TODO /", true},
		{"du rooted at root", "du -sh /", true},
		{"grep -r rooted at root", "grep -r TODO /", true},
		{"grep --recursive rooted at root", "grep --recursive TODO /", true},
		{"ls -R rooted at root", "ls -R /", true},
		{"ls -laR rooted at root (bundled flags)", "ls -laR /", true},
		{"find via absolute path to binary", "/usr/bin/find / -name x", true},
		{"grep -rn bundled flags rooted at root", "grep -rn TODO /", true},
		{"grep -nr bundled flags, r not first", "grep -nr TODO /", true},
		{"grep -Rn bundled flags, uppercase R", "grep -Rn TODO /", true},
		{"find ~/ trailing slash", "find ~/ -name foo", true},
		{"find /Users/ trailing slash", "find /Users/ -name x", true},
		{"find $HOME/ trailing slash", "find $HOME/ -iname foo", true},

		// Should allow — bounded or non-recursive.
		{"find /opt/homebrew", "find /opt/homebrew -name x", false},
		{"find .", "find . -name x", false},
		{"find relative", "find src -name x", false},
		{"find /tmp", "find /tmp -name x", false},
		{"find another user's home", "find /Users/someone-else -name x", false},
		{"grep without -r", "grep TODO file.go", false},
		{"ls -r reverse sort, not recursive", "ls -r /", false},
		{"ls plain", "ls /", false},
		{"du without root arg", "du -sh .", false},
		{"no scan tool at all", "echo hello", false},

		// A scan's flags and root argument come only from its own segment: a
		// recursive flag or a root path belonging to a LATER command on the
		// line must not be read as this command's (gt-n8ir — "grep -c x; jq
		// -r .a; echo //" was blocked as "grep rooted at //", combining jq's
		// -r with echo's // to incriminate a grep that is not recursive).
		{"-r from a later jq, // from a later echo", "grep -c x; jq -r .a; echo //", false},
		{"same line, order reversed", "jq -r .a; grep -c x; echo //", false},
		{"ls -R from a later command", "grep -c x; ls -R; echo //", false},
		{"// after a bounded grep", "grep -rn TODO ./src; echo //", false},
		{"the scan's own segment still blocks", "find / -name x; echo //", true},
		{"a later segment's scan still blocks", "echo //; find / -name x", true},
		{"a later pipeline stage blocks on its own scan", "grep -c x | ls -R /", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesUnboundedScan(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if tt.blocked && alternative == "" {
				t.Errorf("matchesUnboundedScan(%q) blocked but returned no alternative text", tt.command)
			}
		})
	}
}

// TestScanPatternSlot pins the per-tool grammar that locates a scan tool's
// pattern argument: the value of -e/-f, else the first positional argument,
// with a value-taking option's separate argument counting as neither
// (gt-yts7).
func TestScanPatternSlot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tool    string
		args    []string
		pattern int
		hasRoot bool
	}{
		{"grep pattern is the first positional", "grep", []string{"-rn", "TODO", "./docs"}, 1, true},
		{"grep pattern with no path argument", "grep", []string{"-rn", "polecats"}, 1, false},
		{"grep context flag's value is not the pattern", "grep", []string{"-rn", "-A", "3", "polecats", "./docs"}, 3, true},
		{"grep -A with an inline value", "grep", []string{"-rnA3", "polecats", "./docs"}, 1, true},
		{"grep -e supplies the pattern", "grep", []string{"-rn", "-e", "TODO", "./docs"}, 2, true},
		{"grep -e after a path leaves the path positional", "grep", []string{"-rn", "polecats", "-e", "TODO", "./docs"}, 3, true},
		{"grep bundle carrying -e", "grep", []string{"-rne", "polecats", "./docs"}, 1, true},
		{"a bare -- ends option parsing", "grep", []string{"-rn", "--", "--recursive", "./docs"}, 2, true},
		{"rg --files takes no pattern", "rg", []string{"--files", "polecats", "./docs"}, -1, true},
		{"rg type filter's value is not the pattern", "rg", []string{"-t", "go", "polecats", "./docs"}, 2, true},
		{"rg inline long value", "rg", []string{"--iglob=*.md", "polecats", "./docs"}, 1, true},
		{"ag count flag keeps a non-number for the pattern", "ag", []string{"-A", "polecats", "./docs"}, 1, true},
		{"ag count flag takes a number", "ag", []string{"-A", "3", "polecats", "./docs"}, 2, true},
		{"fd pattern then path", "fd", []string{"polecats", "./docs"}, 0, true},
		{"fd extension filter's value is not the pattern", "fd", []string{"-e", "go", "polecats", "./docs"}, 2, true},
		{"fd search-path names the path", "fd", []string{"--search-path", "./docs", "polecats"}, 2, true},
		{"fd pattern with no path argument", "fd", []string{"polecats"}, 0, false},
		{"no arguments", "rg", nil, -1, false},
		{"flags only", "grep", []string{"-rn"}, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gram, ok := scanGrammars[tt.tool]
			if !ok {
				t.Fatalf("no grammar for %q", tt.tool)
			}
			pattern, hasRoot := scanPatternSlot(gram, tt.args)
			if pattern != tt.pattern || hasRoot != tt.hasRoot {
				t.Errorf("scanPatternSlot(%s, %q) = (%d, %v), want (%d, %v)",
					tt.tool, tt.args, pattern, hasRoot, tt.pattern, tt.hasRoot)
			}
		})
	}
}

// lowerTokens is the test-only equivalent of runTapGuardDangerous's
// shellTokenize-then-lowercase step.
func lowerTokens(command string) []string {
	tokens := shellTokenize(command)
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = strings.ToLower(t)
	}
	return out
}

// TestMatchesPolecatMainPush pins the polecat main-push refusal (gt-ibt8):
// every refspec shape that sends work to main/master is blocked in a polecat
// session, the DELETE and --all/--mirror forms included, and every shape that
// only reads from main or names some other branch is left alone. The same
// commands must stay allowed for non-polecat sessions (crew, refinery, mayor
// all push the default branch directly by design).
func TestMatchesPolecatMainPush(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		polecat bool
		blocked bool
	}{
		// Blocked in a polecat session — destinations that are main/master.
		{"shorthand main", "git push origin main", true, true},
		{"shorthand master", "git push origin master", true, true},
		{"HEAD:main", "git push origin HEAD:main", true, true},
		{"sha:main", "git push origin 8f2a1b3:main", true, true},
		{"refs/heads form", "git push origin HEAD:refs/heads/main", true, true},
		{"full ref source and dest", "git push origin refs/heads/main", true, true},
		{"delete via empty source", "git push origin :main", true, true},
		{"delete via flag", "git push origin --delete main", true, true},
		{"force flag", "git push -f origin main", true, true},
		{"force-with-lease still targets main", "git push --force-with-lease origin main", true, true},
		{"leading plus", "git push origin +main", true, true},
		{"push --all carries main", "git push --all", true, true},
		{"push --mirror carries main", "git push origin --mirror", true, true},
		{"uppercase destination", "git push origin HEAD:MAIN", true, true},

		// Allowed in a polecat session — its own branch, or main as a source.
		{"polecat branch", "git push origin polecat/flint/gt-ibt8", true, false},
		{"bare HEAD push", "git push origin HEAD", true, false},
		{"HEAD to polecat branch", "git push origin HEAD:polecat/flint/x", true, false},
		{"main as source only", "git push origin main:polecat/flint/x", true, false},
		{"branch name containing main", "git push origin feature/main-fix", true, false},
		{"integration branch", "git push origin integration/epic-1", true, false},
		{"notes ref", "git push origin refs/notes/om", true, false},
		{"tag push", "git push origin v1.0.0", true, false},
		{"not a push", "git status", true, false},
		{"gt done", "gt done", true, false},

		// Not a polecat session: direct default-branch pushes stay allowed.
		{"crew push to main", "git push origin main", false, false},
		{"crew push --all", "git push --all", false, false},
		{"crew HEAD:main", "git push origin HEAD:main", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := matchesPolecatMainPush(lowerTokens(tt.command), tt.polecat)
			if (reason != "") != tt.blocked {
				t.Errorf("matchesPolecatMainPush(%q, polecat=%v) blocked=%v, want %v", tt.command, tt.polecat, reason != "", tt.blocked)
			}
		})
	}
}

// TestInPolecatSession pins which signals make a session a polecat one for
// this guard (gt-c38o): GT_ROLE decides whenever it is set, and
// GT_POLECAT_PATH is the fallback for a run with no role in its environment.
// Reading the path marker alone made the answer turn on which markers a
// session's environment happened to carry rather than on the role the town
// assigned it.
func TestInPolecatSession(t *testing.T) {
	const worktree = "/town/rig/polecats/flint/rig"
	tests := []struct {
		name          string
		gtRole        string
		gtPolecatPath string
		want          bool
	}{
		{"GT_ROLE names a polecat", "gastown/polecats/flint", "", true},
		{"GT_ROLE polecat and path marker agree", "gastown/polecats/flint", worktree, true},
		{"GT_ROLE bare polecat", "polecat", "", true},
		{"GT_ROLE crew beats a stale path marker", "gastown/crew/alice", worktree, false},
		{"GT_ROLE refinery beats a stale path marker", "gastown/refinery", worktree, false},
		{"GT_ROLE mayor beats a stale path marker", "mayor", worktree, false},
		{"GT_ROLE witness beats a stale path marker", "gastown/witness", worktree, false},
		{"no GT_ROLE falls back to the path marker", "", worktree, true},
		{"neither signal", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.gtRole)
			t.Setenv("GT_POLECAT_PATH", tt.gtPolecatPath)
			if got := inPolecatSession(); got != tt.want {
				t.Errorf("inPolecatSession() with GT_ROLE=%q GT_POLECAT_PATH=%q = %v, want %v",
					tt.gtRole, tt.gtPolecatPath, got, tt.want)
			}
		})
	}
}

// TestPolecatMainPushReachesGuard checks the wiring rather than the matcher:
// the full evaluateDangerousCommand path must block the incident command (a
// HEAD:main push, which is how granite landed on origin/main) whenever the
// session is a polecat's, and allow it for any other role.
//
// Which signal settles that is the gt-c38o fix — GT_ROLE decides and
// GT_POLECAT_PATH is the fallback (TestInPolecatSession pins the precedence)
// — so the role alone reaches the block, and a coordinator that carries a
// stale path marker keeps its direct default-branch push path.
func TestPolecatMainPushReachesGuard(t *testing.T) {
	const incident = "git push origin HEAD:main"
	const worktree = "/town/rig/polecats/flint/rig"

	tests := []struct {
		name          string
		gtRole        string
		gtPolecatPath string
		blocked       bool
	}{
		{"GT_ROLE polecat", "gastown/polecats/flint", "", true},
		{"GT_ROLE polecat and path marker agree", "gastown/polecats/flint", worktree, true},
		{"path marker alone, no GT_ROLE", "", worktree, true},
		{"refinery with a stale path marker", "gastown/refinery", worktree, false},
		{"crew with a stale path marker", "gastown/crew/alice", worktree, false},
		{"neither signal", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.gtRole)
			t.Setenv("GT_POLECAT_PATH", tt.gtPolecatPath)
			reason, _ := evaluateDangerousCommand(incident, 0, "")
			if (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) with GT_ROLE=%q GT_POLECAT_PATH=%q blocked=%v, want %v",
					incident, tt.gtRole, tt.gtPolecatPath, reason != "", tt.blocked)
			}
			if tt.blocked && reason != polecatMainPushReason {
				t.Errorf("evaluateDangerousCommand(%q) blocked by %q, want the polecat main-push rule %q",
					incident, reason, polecatMainPushReason)
			}
		})
	}

	// Nested payloads (bash -c) must be judged the same way.
	t.Run("nested payload", func(t *testing.T) {
		t.Setenv("GT_ROLE", "gastown/polecats/flint")
		t.Setenv("GT_POLECAT_PATH", "")
		nested := `bash -c "git push origin HEAD:main"`
		if reason, _ := evaluateDangerousCommand(nested, 0, ""); reason == "" {
			t.Fatalf("evaluateDangerousCommand(%q) allowed for a polecat session, want blocked", nested)
		}
	})
}

// TestGtMkrjRegressions pins the five exact command texts that false-fired
// dangerous-command before gt-mkrj: substring matching ("apt" inside
// "capture"/"adapt") and quote-unaware tokenization (a "//" jq/sed operator
// inside a quoted script read as a bare root-path argument) both fired on
// text that was never a real standalone shell argument. All five must now
// pass (no dangerous-command block) once matching is shell-aware and
// token-exact. See mail hq-wisp-io521 for the original reports.
func TestGtMkrjRegressions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{
			"sed script with jq alternative operator",
			`sed -i '' "s|OLD|jq -r '.[] // []'|" watch.sh`,
		},
		{
			"heredoc containing tmux capture-pane",
			"cat > watch.sh <<'EOF'\ntmux capture-pane -p -t sess\nEOF",
		},
		{
			"gt escalate message mentioning capture",
			`gt escalate -s HIGH "issue" -r "the word capture matched incorrectly"`,
		},
		{
			"jq filter with alternative operator",
			`jq -r '"\(.ts) \(.actor) -> \(.payload.to // "?"): done"'`,
		},
		{
			"gt mail send with apt substring in body",
			`gt mail send mayor/ -s "report" -m "adapt this script to capture output"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			lower := make([]string, len(tokens))
			for i, tok := range tokens {
				lower[i] = strings.ToLower(tok)
			}
			if reason := matchesSudo(lower); reason != "" {
				t.Fatalf("matchesSudo false-fired: %q", reason)
			}
			if reason := matchesPackageInstall(lower); reason != "" {
				t.Fatalf("matchesPackageInstall false-fired: %q", reason)
			}
			if reason := matchesDangerousRmRf(lower); reason != "" {
				t.Fatalf("matchesDangerousRmRf false-fired: %q", reason)
			}
			if reason := matchesDangerousGitPush(lower); reason != "" {
				t.Fatalf("matchesDangerousGitPush false-fired: %q", reason)
			}
			if reason, _ := matchesUnboundedScan(tokens, ""); reason != "" {
				t.Fatalf("matchesUnboundedScan false-fired: %q", reason)
			}
			for _, p := range fragmentPatterns {
				if matchesAllFragments(lower, p.contains) {
					t.Fatalf("fragmentPatterns false-fired: %q", p.reason)
				}
			}
		})
	}
}

// TestGluedOperatorsAreBlocked pins the narrowed gt-mkrj scope: a shell
// control operator (;, &, |) glued directly to an adjacent word with no
// surrounding whitespace must still act as a token boundary, so a
// dangerous command chained after it isn't hidden inside one opaque
// shlex token (e.g. "hi;rm" swallowing "rm").
func TestGluedOperatorsAreBlocked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"semicolon glued to rm -rf /", "echo hi;rm -rf /"},
		{"double-ampersand glued", "echo hi&&sudo rm -rf /"},
		{"pipe glued to git push --force", "echo hi|xargs -I{} git push --force"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked (glued operator hid a dangerous fragment)", tt.command)
			}
		})
	}
}

// TestGluedOperatorsInsideQuotesStayOpaque guards spaceOutShellOperators
// against over-reach: operator characters inside quotes (a sed script, a
// jq filter) must remain part of their opaque token, exactly like before
// this pass — see TestQuotedSQLStaysOpaque and TestGtMkrjRegressions for
// the same invariant on other matchers.
func TestGluedOperatorsInsideQuotesStayOpaque(t *testing.T) {
	t.Parallel()
	command := `sed -i '' "s|OLD|jq -r '.[] // []'|" watch.sh`
	if reason, _ := evaluateDangerousCommand(command, 0, ""); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — quoted operators must stay opaque", command, reason)
	}
}

// TestShellTokenizeSplitsUnquotedNewlines pins the gt-3j8u tokenizer rule
// every command-word matcher depends on: an unquoted newline ends a command
// exactly like ';', so it must surface as its own separator token — while a
// newline that is quoted (or a backslash-newline line continuation, which
// the shell deletes to JOIN the lines) must not.
func TestShellTokenizeSplitsUnquotedNewlines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"newline becomes a separator token", "cd /tmp\ngh pr create",
			[]string{"cd", "/tmp", ";", "gh", "pr", "create"}},
		{"blank line yields an empty segment", "echo one\n\ngit status",
			[]string{"echo", "one", ";", ";", "git", "status"}},
		{"newline still separates after a glued operator", "echo hi&&\nrm -rf /tmp/x",
			[]string{"echo", "hi", "&&", ";", "rm", "-rf", "/tmp/x"}},
		{"line continuation joins the two lines", "git \\\n  checkout -b feature/x",
			[]string{"git", "checkout", "-b", "feature/x"}},
		{"continuation inside double quotes joins too", "echo \"a\\\nb\"",
			[]string{"echo", "ab"}},
		{"escaped backslash before a newline still separates", "echo a\\\\\nb",
			[]string{"echo", `a\`, ";", "b"}},
		{"newline inside single quotes is literal", "printf 'a\nb'",
			[]string{"printf", "a\nb"}},
		{"newline inside double quotes is literal", "echo \"a\nb\"",
			[]string{"echo", "a\nb"}},
		{"newline inside a quoted argument is one token", "git commit -m \"fix: use gh pr create\"\nls",
			[]string{"git", "commit", "-m", "fix: use gh pr create", ";", "ls"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellTokenize(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("shellTokenize(%q) = %q, want %q", tt.command, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("shellTokenize(%q) = %q, want %q", tt.command, got, tt.want)
				}
			}
		})
	}
}

// TestShellVariableScanRootIsBlocked pins the narrowed gt-mkrj scope: a
// scan whose root path is routed through a shell variable assigned
// earlier in the same command line must be blocked exactly like a literal
// root argument.
func TestShellVariableScanRootIsBlocked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"bare variable assigned to /", "x=/; bfs $x -name regex.h"},
		{"braced variable form", "x=/; bfs ${x} -name regex.h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked (shell variable indirection hid an unbounded scan root)", tt.command)
			}
		})
	}
}

// TestShellVariableScanRootAllowsBoundedPath ensures the variable
// resolution added for gt-mkrj doesn't turn every $var-rooted scan into a
// false positive — only a variable that actually resolves to a denylisted
// root should block.
func TestShellVariableScanRootAllowsBoundedPath(t *testing.T) {
	t.Parallel()
	command := "x=./src; bfs $x -name regex.h"
	if reason, _ := evaluateDangerousCommand(command, 0, ""); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — variable resolves to a bounded path", command, reason)
	}
}

// TestCommandSubstitutionProgramStaysOpaque pins the second half of gt-n8ir.
// A quoted jq program using the "//" alternative operator arrived at the
// guard as a bare "//" token whenever the program sat inside "$(...)" inside
// a double-quoted word, and matchesUnboundedScan read that token as a root
// path — blocking a compound command over quoted text it is supposed to
// leave opaque. The body of a $(...) or `...` substitution is a command in
// its own right (the shell re-parses it), so its quoting must not close the
// enclosing word; the whole substitution has to stay inside one token, which
// is what spaceOutShellOperators now guarantees.
func TestCommandSubstitutionProgramStaysOpaque(t *testing.T) {
	t.Parallel()

	const program = `.[0] | "(.s) a=(.a // "-")"`
	const substitution = `$(jq -r '` + program + `')`
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The reporter's ddmin shape: any two of its three parts passed, all
		// three together were blocked.
		{"reporter's ddmin shape",
			`grep -c x f; echo "x: ` + substitution + `"; jq -r '"(.b)"'`, false},
		{"substitution alone with the jq program",
			`grep -c x f; echo "x: ` + substitution + `"`, false},
		{"scan tool after the substitution",
			`echo "x: ` + substitution + `"; grep -c x f; jq -r '"(.b)"'`, false},
		{"substitution as a grep argument",
			`grep -rn TODO ./src "` + substitution + `"`, false},
		{"the patrol shape the sweep used",
			`bd list --json | jq -r '.assignee // "-"'`, false},
		// An opaque token must not be allowed to swallow a real scan: a
		// substitution wrapping one still blocks, through the recursion into
		// the body's own text.
		{"a real root inside the substitution still blocks",
			`echo "x: $(grep -r TODO /)"`, true},
		{"a real root in a later segment still blocks",
			`grep -c x f; echo "x: ` + substitution + `"; find / -name y`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := evaluateDangerousCommand(tt.command, 0, "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if tt.blocked && alternative == "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked but returned no alternative text", tt.command)
			}
		})
	}

	// The token-level invariant the results above rest on, pinned exactly so
	// a future change to the quote scanner has to face it: the whole
	// substitution is one token, the jq program inside it is intact, and no
	// fragment of the program ("//", "a=(.a", ...) surfaces as a token of
	// its own.
	command := `grep -c x f; echo "x: ` + substitution + `"; jq -r '"(.b)"'`
	want := []string{
		"grep", "-c", "x", "f", ";",
		"echo", `x: $(jq -r '.[0] | "(.s) a=(.a // "-")"')`, ";",
		"jq", "-r", `"(.b)"`,
	}
	got := shellTokenize(command)
	if len(got) != len(want) {
		t.Fatalf("shellTokenize(%q) = %q, want %q", command, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("shellTokenize(%q) = %q, want %q", command, got, want)
		}
	}
	for _, tok := range got {
		if tok == "//" {
			t.Fatalf("shellTokenize(%q) leaked the jq program's // as a standalone token: %q", command, got)
		}
	}
}

// TestHeredocBodyStaysOpaque pins the narrowed gt-mkrj scope: heredoc
// bodies are DATA (a file being written, a message being composed), not
// shell syntax to evaluate, so a dangerous-looking phrase inside one must
// not block the command — mirroring TestQuotedSQLStaysOpaque for quoted
// strings.
func TestHeredocBodyStaysOpaque(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"sudo mentioned in heredoc body", "cat > note.md <<'EOF'\nsudo rm -rf /tmp\nEOF"},
		{"force-push phrase in heredoc body", "cat > note.md <<'EOF'\ngit push --force origin main\nEOF"},
		{"unquoted delimiter", "cat > note.md <<EOF\ndrop table users\nEOF"},
		{"strip-tabs delimiter form", "cat > note.md <<-'EOF'\n\tsudo rm -rf /\n\tEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — heredoc body is data", tt.command, reason)
			}
		})
	}
}

// TestHeredocDoesNotHideRealCommand ensures stripHeredocBodies only
// removes the body span and leaves surrounding shell text — including a
// second, genuinely dangerous command after the terminator — intact.
func TestHeredocDoesNotHideRealCommand(t *testing.T) {
	t.Parallel()
	command := "cat > note.md <<'EOF'\nordinary content\nEOF\nsudo rm -rf /"
	if reason, _ := evaluateDangerousCommand(command, 0, ""); reason == "" {
		t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — real command after heredoc must still be checked", command)
	}
}

// TestShellFedHeredocBodyIsInspected pins gt-9g0y: a heredoc body whose
// reader line runs a shell is a script that shell executes, so it is checked
// like any other nested shell command. Stripping it with the data bodies —
// which is what gt-mkrj did — left it checked by nothing at all.
func TestShellFedHeredocBodyIsInspected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"bash reads the body from stdin", "bash <<'EOF'\ngit reset --hard\nEOF"},
		{"sh with a strip-tabs delimiter", "sh <<-'EOF'\n\tsudo rm -rf /\n\tEOF"},
		{"shell invoked with -s", "bash -s <<EOF\ngit push --force origin main\nEOF"},
		{"body piped into a shell", "cat <<'EOF' | bash\ngit clean -fd\nEOF"},
		{"invoker split across a line continuation", "bash \\\n  <<'EOF'\ngit reset --hard\nEOF"},
		// bash -c runs its argument, but the body is still bash's stdin: the
		// "$(cat)" here expands to the body, which is then what runs.
		{"body read back through the -c substitution", "bash -c \"$(cat)\" <<'EOF'\nsudo rm -rf /\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — a shell invoker runs the heredoc body", tt.command)
			}
		})
	}
}

// TestHeredocBodyStaysOpaqueWithoutAShellReader keeps gt-9g0y from widening
// gt-mkrj: only a shell in command position on the heredoc's line makes the
// body code. A reader that cannot run it leaves the body as data.
func TestHeredocBodyStaysOpaqueWithoutAShellReader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"interpreter that is not a shell", "python3 - <<'EOF'\nsudo rm -rf /\nEOF"},
		{"shell name as an argument", "echo bash <<'EOF'\ngit reset --hard\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, ""); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data", tt.command, reason)
			}
		})
	}
}

// TestShellFedHeredocRecursesThroughNestedBodies pins that a shell-fed body is
// judged by the same nested-command walk as a bash -c payload: a shell between
// the outer body and the dangerous command does not end the inspection.
func TestShellFedHeredocRecursesThroughNestedBodies(t *testing.T) {
	t.Parallel()
	command := "bash <<'A'\nbash <<'B'\ngit reset --hard\nB\nA"
	if reason, _ := evaluateDangerousCommand(command, 0, ""); reason == "" {
		t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the inner body is a nested shell command", command)
	}
}

// TestNestedShellCPositionalArgsAreNotConcatenated pins the gt-mkrj fix to
// nestedCommands: "<shell> -c" takes exactly one command-string argument;
// anything after it is a positional parameter ($0, $1, ...) passed to that
// command, never appended shell text. Two separately-quoted args here are
// argv0 and argv1 for "echo a", not "echo a && rm -rf /" — so this must
// stay allowed even though joining tokens (the old behavior) would have
// fabricated a dangerous command line that was never actually live.
func TestNestedShellCPositionalArgsAreNotConcatenated(t *testing.T) {
	t.Parallel()
	command := `bash -c "echo a" "&&" "rm -rf /"`
	if reason, _ := evaluateDangerousCommand(command, 0, ""); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — extra -c args are positional params, not appended command text", command, reason)
	}
}

// TestDangerousGuard_Integration tests the full pattern set end-to-end.
func TestDangerousGuard_Integration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — privilege escalation
		{"sudo command", "sudo dnf install -y foo", true},
		{"sudo rm", "sudo rm -rf /var/cache", true},

		// Blocked — package installs
		{"apt install", "apt install -y curl", true},
		{"dnf install", "dnf install -y postgresql-contrib", true},
		{"brew install", "brew install node", true},
		{"npm install -g", "npm install -g typescript", true},
		{"pip install --system", "pip install --system requests", true},

		// Blocked — destructive operations
		{"rm -rf /", "rm -rf /", true},
		{"git push --force", "git push --force origin main", true},
		{"git reset --hard", "git reset --hard HEAD~1", true},
		{"git reset --soft onto origin/main", "git reset --soft origin/main", true},
		{"git clean -f", "git clean -f", true},
		{"git clean -fd", "git clean -fd", true},
		{"drop table", "DROP TABLE users", true},

		// Allowed
		{"rm -rf ./build/", "rm -rf ./build/", false},
		{"rm -rf /tmp/cache/", "rm -rf /tmp/cache/", false},
		{"git push --force-with-lease", "git push --force-with-lease origin main", false},
		{"git push normal", "git push origin main", false},
		{"git reset soft", "git reset --soft HEAD~1", false},
		{"git reset -- origin/main", "git reset -- origin/main", false},
		{"pip install (venv)", "pip install requests", false},
		{"npm install (local)", "npm install express", false},
		{"normal command", "ls -la", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lower := lowerTokens(tt.command)
			blocked := false
			if matchesSudo(lower) != "" {
				blocked = true
			} else if matchesPackageInstall(lower) != "" {
				blocked = true
			} else if matchesDangerousRmRf(lower) != "" {
				blocked = true
			} else if matchesDangerousGitPush(lower) != "" {
				blocked = true
			} else if r, _ := matchesDangerousGitReset(lower); r != "" {
				blocked = true
			} else if matchesGitClean(shellTokenize(tt.command)) != "" {
				blocked = true
			} else {
				for _, p := range fragmentPatterns {
					if matchesAllFragments(lower, p.contains) {
						blocked = true
						break
					}
				}
			}
			if blocked != tt.blocked {
				t.Errorf("command %q: blocked=%v, want %v", tt.command, blocked, tt.blocked)
			}
		})
	}
}

// bashMatcherGlob mirrors how Claude Code interprets an "if" condition
// written in "Bash(<pattern>)" permission-rule syntax: '*' matches any run
// of characters, including '/'. NOT used against Matcher — Claude Code's
// Matcher only ever matches the tool name (gt-5ihs); this glob applies to a
// Hook's If field.
func bashMatcherGlob(pattern, command string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(pattern, "Bash("), ")")
	re, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(inner), `\*`, ".*") + "$")
	if err != nil {
		return false
	}
	return re.MatchString(command)
}

// bashHookFires reports whether hook h (attached to a PreToolUse entry whose
// Matcher is the bare "Bash" tool name) actually runs for command, mirroring
// Claude Code's real dispatch: an empty If means the hook always fires for
// any Bash call (it self-filters on the command internally, e.g.
// dangerous-command); a non-empty If is evaluated as a permission-rule glob
// against the command (e.g. pr-workflow's per-pattern hooks).
func bashHookFires(h hooks.Hook, command string) bool {
	if h.If == "" {
		return true
	}
	return bashMatcherGlob(h.If, command)
}

// preToolUseMatcherAppliesToTool reports whether a bare tool-name PreToolUse
// matcher (e.g. "Bash", "Bash|Monitor", "Edit|Write|MultiEdit|NotebookEdit")
// dispatches for the given tool, matching Claude Code's own semantics: the
// matcher is a regex matched against the tool name (gt-5ihs) — so
// "Bash|Monitor" fires for either "Bash" or "Monitor", not only the exact
// string "Bash" (gt-vx2mm added Monitor to the base guards' matcher, which
// broke an exact-string comparison here).
func preToolUseMatcherAppliesToTool(matcher, tool string) bool {
	re, err := regexp.Compile("^(?:" + matcher + ")$")
	if err != nil {
		return matcher == tool
	}
	return re.MatchString(tool)
}

// dangerousCommandGuardMatcher reports whether the given command is routed
// by hooks.DefaultBase()'s PreToolUse config to the dangerous-command guard
// specifically, using Claude Code's real dispatch semantics: the entry's
// Matcher must apply to the "Bash" tool name — a permission-rule pattern
// written into Matcher never fires (gt-5ihs) — and each hook's If (if any)
// gates it further.
func dangerousCommandGuardMatcher(command string) bool {
	for _, entry := range hooks.DefaultBase().PreToolUse {
		if !preToolUseMatcherAppliesToTool(entry.Matcher, "Bash") {
			continue
		}
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, "dangerous-command") && bashHookFires(h, command) {
				return true
			}
		}
	}
	return false
}

// TestHookMatchersRouteKnownDangerousCommands guards against exactly the bug
// that bounced the first MR for gt-nqcy: matchesUnboundedScan (and friends)
// blocking a command in isolation proves nothing if the PreToolUse matcher
// that's supposed to feed it that command never fires for real invocations
// like "/usr/bin/find /" or "ls -laR /". This walks the real matcher config
// from internal/hooks, not a hand-rolled stand-in for it.
func TestHookMatchersRouteKnownDangerousCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"bfs rooted at root", "bfs -S dfs / -name regex.h -path *unicode*"},
		{"find rooted at root", "find / -name x"},
		{"find via absolute path to binary", "/usr/bin/find / -name x"},
		{"fd rooted at root", "fd regex.h /"},
		{"rg rooted at root", "rg TODO /"},
		{"du rooted at root", "du -sh /"},
		{"grep -r rooted at root", "grep -r TODO /"},
		{"grep -rn bundled flags rooted at root", "grep -rn TODO /"},
		// The gt-6e2l report shape: a town-root scan must reach the guard.
		// Town-root resolution itself is exercised in tap_guard_town_scan_test.go.
		{"grep -R over a town root (gt-6e2l)", `grep -R "record-run" -n /Users/me/gt`},
		{"ls -R rooted at root", "ls -R /"},
		{"ls -laR bundled flags rooted at root", "ls -laR /"},
		{"rm -rf root", "rm -rf /"},
		{"git push --force", "git push --force origin main"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !dangerousCommandGuardMatcher(tt.command) {
				t.Fatalf("command %q: no PreToolUse matcher in hooks.DefaultBase() routes it to the dangerous-command guard", tt.command)
			}
		})
	}
}

// TestHookMatchersDoNotOverfireOnSafeCommands ensures the broadened matchers
// (e.g. "Bash(*find*)", "Bash(ls -*)") still leave clearly safe, bounded
// commands alone — the guard itself would allow them anyway, but a matcher
// that fires on everything defeats the point of routing selectively.
func TestHookMatchersDoNotOverfireOnSafeCommands(t *testing.T) {
	t.Parallel()
	tests := []string{
		"find . -name x",
		"find /tmp -name x",
		"ls -la .",
		"ls /opt/homebrew",
		"grep TODO file.go",
		"git status",
	}
	for _, command := range tests {
		t.Run(command, func(t *testing.T) {
			if dangerousCommandGuardMatcher(command) {
				// Being routed to the guard isn't itself a failure — the guard
				// must still allow it. Confirm that's actually what happens.
				lower := lowerTokens(command)
				if reason, _ := matchesUnboundedScan(shellTokenize(command), ""); reason != "" {
					t.Fatalf("command %q reached the dangerous-command guard and was blocked: %q", command, reason)
				}
				if matchesDangerousRmRf(lower) != "" || matchesDangerousGitPush(lower) != "" {
					t.Fatalf("command %q reached the dangerous-command guard and was blocked", command)
				}
			}
		})
	}
}

// TestUnboundedScanLiteralHomeDir: the denylist catches the shell spellings
// of the home directory (~, $HOME, /Users) but an agent that writes the
// EXPANDED path names the same root and walked through unblocked — dog alpha
// ran `find /Users/sloan -maxdepth 3 -name .git` on 2026-09-18 and macOS
// privacy prompts for Documents/Desktop/Music landed on the operator. The
// literal home path must block exactly like ~; a path one level below it
// must stay allowed, or the fix is just "block scans" wearing a home check.
func TestUnboundedScanLiteralHomeDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"find literal home with -maxdepth", "find " + home + " -maxdepth 3 -name .git -type d", true},
		{"find literal home trailing slash", "find " + home + "/ -name x", true},
		{"grep -r literal home", "grep -r foo " + home, true},
		{"du literal home", "du -sh " + home, true},
		{"rg literal home", "rg pattern " + home, true},
		{"find path below home", "find " + home + "/project -name x", false},
		{"ls literal home (not recursive)", "ls -la " + home, false},
		{"find ~ still blocked", "find ~ -name x", true},
	}
	blockedCount := 0
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), "")
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("matchesUnboundedScan(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
			if got {
				blockedCount++
				if alternative == "" {
					t.Errorf("matchesUnboundedScan(%q) blocked but returned no alternative text", tt.command)
				}
			}
		})
	}
	// Count assertion: a version of the rule that blocks nothing, or blocks
	// everything, fails here even if every per-case check were rewritten.
	if blockedCount != 6 {
		t.Errorf("blocked %d of %d cases, want exactly 6", blockedCount, len(tests))
	}
}

// TestMatchesWitnessGitPush: any push from a witness is refused, with the
// exact shape from the 2026-09-18 incident first; the same commands pass for
// a polecat (whose branch pushes are its job) and a refinery. Read-only git
// from a witness stays allowed.
func TestMatchesWitnessGitPush(t *testing.T) {
	t.Parallel()
	blocked := []string{
		"cd /Users/sloan/gt/gastown/polecats/slate/gastown && git push origin polecat/slate/gt-nkyy+x:b43cc157 --force-with-lease=polecat/slate/gt-nkyy+x:8fdf345",
		"git push origin polecat/slate/gt-nkyy+x --force-with-lease=polecat/slate/gt-nkyy+x:8fdf345",
		"git push origin main",
		"git push",
		"git fetch origin && git push origin HEAD",
		"git -C /Users/sloan/gt/gastown/polecats/flint/gastown push origin bfe970a:polecat/flint/gt-3qfp+x",
		"git --no-pager push origin HEAD",
		"git --git-dir=/x/.git --work-tree /x push",
		"git -c push.default=current push",
		"cd /x && git push origin HEAD",
		"GIT_SSH_COMMAND=ssh git push",
		"env GIT_TRACE=1 git push",
		"timeout 60 git push origin HEAD",
		"eval git push origin HEAD",
		"nice -n 10 git push",
		"xargs -0 git push",
		"cd /x && timeout 60 git push origin HEAD",
	}
	allowed := []string{
		"git fetch origin polecat/slate/gt-nkyy+x",
		"git log --oneline -5",
		"git status --porcelain",
		"gt polecat list gastown",
		"echo push",
		"git -C /x log --oneline -3",
		"git -c color.ui=false status",
		"echo we should not git push here",
		"gt mail send mayor -s 'about git push' -m 'the polecat should git push itself'",
	}
	for _, c := range blocked {
		reason, alt := matchesWitnessGitPush(shellTokenize(c), true)
		if reason == "" || alt == "" {
			t.Errorf("witness command not blocked (or no alternative): %q", c)
		}
		if reason, _ := matchesWitnessGitPush(shellTokenize(c), false); reason != "" {
			t.Errorf("non-witness command blocked by the witness rule: %q", c)
		}
	}
	for _, c := range allowed {
		if reason, _ := matchesWitnessGitPush(shellTokenize(c), true); reason != "" {
			t.Errorf("witness read-only command blocked: %q (%s)", c, reason)
		}
	}
}

// Through evaluateDangerousCommand with the role taken from GT_ROLE, as the
// hook sees it: the witness is blocked, a polecat pushing its own branch and
// a refinery are not.
func TestWitnessGitPushReachesGuard(t *testing.T) {
	cmd := "git push origin polecat/slate/gt-nkyy+x --force-with-lease=polecat/slate/gt-nkyy+x:8fdf345"
	t.Setenv("GT_POLECAT_PATH", "")
	t.Setenv("GT_ROLE", "gastown/witness")
	if reason, _ := evaluateDangerousCommand(cmd, 0, ""); reason != witnessGitPushReason {
		t.Errorf("witness: reason = %q, want %q", reason, witnessGitPushReason)
	}
	t.Setenv("GT_ROLE", "gastown/polecats/slate")
	if reason, _ := evaluateDangerousCommand(cmd, 0, ""); reason != "" {
		t.Errorf("polecat own-branch push blocked: %q", reason)
	}
	t.Setenv("GT_ROLE", "gastown/refinery")
	if reason, _ := evaluateDangerousCommand(cmd, 0, ""); reason != "" {
		t.Errorf("refinery push blocked: %q", reason)
	}
}

// Regression tests for gt-3mp1: the if-glob evaluator in Claude Code
// has a bug where it treats certain shell patterns as "match any pattern
// starting with *" when it cannot statically resolve them. This caused
// every if-gated deny hook to fire on unrelated commands.
func TestDangerousCommand_Gt3mp1Regression(t *testing.T) {
	t.Parallel()
	// Pattern (a): Commands with { brace group containing a quoted string
	// trip the if-glob evaluator, which then matches any leading-* glob
	braceGroupCommands := []string{
		`echo x{'a'}y`,
		`echo x{"a"}y`,
		`python3 - <<'EOF'
print({'a'})
EOF`,
	}
	for _, cmd := range braceGroupCommands {
		t.Run("brace-group with quoted string", func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(cmd, 0, "")
			if reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed (brace-group with quoted string should not trip the guard)", cmd, reason)
			}
		})
	}

	// Pattern (b): Variable assigned from command substitution in the same
	// command line, then used as a bare double-quoted argument trips hooks
	commandSubCommands := []string{
		`M=$(echo abc); echo "$M"`,
		`ID=$(gt mail inbox --json 2>/dev/null | python3 -c "import json,sys; m=json.load(sys.stdin); print(m[0]['id'] if m else '')"); gt mail read "$ID"`,
		`MAIN=$(git ls-remote --heads origin main | cut -f1); git cat-file -e "$MAIN"`,
	}
	for _, cmd := range commandSubCommands {
		t.Run("command substitution with bare $VAR", func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(cmd, 0, "")
			if reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed (command substitution with bare $VAR should not trip the guard)", cmd, reason)
			}
		})
	}
}

// TestMatchesGoCleanSharedCache pins the gt-nqcy follow-up: 'go clean' with
// any of the cache-wiping flags must be blocked — it wipes a cache shared by
// every agent on the host — while every other 'go clean' shape (no flags,
// non-shared flags, the flag as prose inside quotes) stays allowed. Ported
// from the interim host-hygiene hook.
func TestMatchesGoCleanSharedCache(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Should block — each shared-cache flag individually.
		{"go clean -cache", "go clean -cache", true},
		{"go clean -testcache", "go clean -testcache", true},
		{"go clean -modcache", "go clean -modcache", true},
		{"go clean -fuzzcache", "go clean -fuzzcache", true},
		{"combined with other flags", "go clean -x -cache", true},
		{"flag order reversed", "go clean -cache -x", true},
		{"multiple shared flags", "go clean -cache -testcache -modcache", true},
		{"glued to a shell operator", "cd /tmp; go clean -modcache", true},

		// Should allow.
		{"safe flag on go clean, shared flag on a later unrelated command", "go clean -i; ls -la -cache", false},
		{"bare go clean", "go clean", false},
		{"go clean -i", "go clean -i", false},
		{"go clean -n", "go clean -n", false},
		{"go clean -x", "go clean -x", false},
		{"go clean -r", "go clean -r", false},
		{"go clean of a package", "go clean ./...", false},
		{"no go clean at all", "echo hello", false},
		{"flag mentioned in quoted prose", `gt mail send x -s "note" -m "never go clean -modcache"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := matchesGoCleanSharedCache(lowerTokens(tt.command))
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("matchesGoCleanSharedCache(%q) blocked=%v, want %v", tt.command, blocked, tt.blocked)
			}
		})
	}
}

// TestGoCleanSharedCacheReachesGuard checks the full evaluateDangerousCommand
// path, including recursion into bash -c/eval wrappers, for the shared-cache
// class — mirroring TestNestedShellCommands for the other matchers.
func TestGoCleanSharedCacheReachesGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"top-level", "go clean -modcache", true},
		{"bash -c wrapped", `bash -c "go clean -testcache"`, true},
		{"safe top-level", "go clean -i", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, "")
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, blocked, reason, tt.blocked)
			}
		})
	}
}

// TestIsIdleGatedSuiteStartCommand pins the gt-nqcy follow-up's detection of
// unwrapped full-suite starts, ported from the interim host-hygiene hook:
// 'make test', 'go test ./...', 'make build', and 'go build ./...' are
// gated; scoped invocations, 'gt slot run'-wrapped ones, and quoted prose
// are not.
func TestIsIdleGatedSuiteStartCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		gated   bool
	}{
		// Should gate.
		{"make test", "make test", true},
		{"make test with env prefix", "GOFLAGS=-p=8 make test", true},
		{"make test with -j flag", "make -j4 test", true},
		{"make build", "make build", true},
		{"go test whole repo", "go test ./...", true},
		{"go test whole repo bare dots", "go test ...", true},
		{"go build whole repo", "go build ./...", true},
		{"go test after unrelated segment", "echo hi && go test ./...", true},
		{"go test glued to semicolon", "cd /tmp;go test ./...", true},

		// Should NOT gate.
		{"scoped go test", "go test ./internal/beads/...", false},
		{"scoped go build", "go build ./cmd/gt", false},
		{"make lint", "make lint", false},
		{"wrapped in gt slot run", "gt slot run -- make test", false},
		{"wrapped go test in gt slot run", "gt slot run --role gastown/flint -- go test ./...", false},
		{"go test with flag before target (interim adjacency)", "go test -v ./...", false},
		{"go build with flag before target (interim adjacency)", "go build -v ./...", false},
		{"quoted mention", `echo "run make test if idle"`, false},
		{"no suite command at all", "echo hello", false},
		{"prose in a heredoc body written to a file", "cat > note.md <<'EOF'\nRun make test before committing.\nEOF", false},
		{"real command after the heredoc still gates", "cat > note.md <<'EOF'\nordinary content\nEOF\nmake test", true},
		{"slot-run mention trailing an unwrapped command is not a real wrapper", "make test # not actually gt slot run", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIdleGatedSuiteStartCommand(tt.command)
			if got != tt.gated {
				t.Errorf("isIdleGatedSuiteStartCommand(%q) = %v, want %v", tt.command, got, tt.gated)
			}
		})
	}
}

// TestEvaluateIdleGate checks the wiring between the pure command matcher
// and the CPU sample: a gated command is held when the sampled idle
// percentage is below idleGateThresholdPercent, and allowed through when it
// is at or above threshold, the sample fails (fail open), or the command
// isn't gated at all.
func TestEvaluateIdleGate(t *testing.T) {
	origCPUIdlePercent := cpuIdlePercent
	t.Cleanup(func() { cpuIdlePercent = origCPUIdlePercent })

	tests := []struct {
		name       string
		command    string
		sampleIdle int
		sampleOK   bool
		wantHeld   bool
	}{
		{"held when idle below threshold", "make test", 10, true, true},
		{"allowed when idle at threshold", "make test", 25, true, false},
		{"allowed when idle above threshold", "make test", 90, true, false},
		{"allowed when sample fails (fail open)", "make test", 0, false, false},
		{"allowed when not a gated command", "make lint", 0, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpuIdlePercent = func() (int, bool) { return tt.sampleIdle, tt.sampleOK }
			_, held := evaluateIdleGate(tt.command)
			if held != tt.wantHeld {
				t.Errorf("evaluateIdleGate(%q) held=%v, want %v", tt.command, held, tt.wantHeld)
			}
		})
	}
}
