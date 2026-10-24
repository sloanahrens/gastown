package cmd

import (
	"os"
	"path/filepath"
	"reflect"
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
// is blocked (the implicit --mixed of a bare `git reset origin/main` included),
// as is a target written as a shell expansion, whose text the guard cannot
// resolve to the ref it really names (gt-tao2j); every reset to a local ref or
// a pathspec still works, since that is the ordinary squash and unstage
// vocabulary.
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
		{"reset --soft FETCH_HEAD", "git fetch origin && git reset --soft FETCH_HEAD", true},
		{"reset --soft @{u}", "git reset --soft @{u}", true},
		{"reset --soft @{upstream}", "git reset --soft @{upstream}", true},
		{"reset --soft main@{upstream}", "git reset --soft main@{upstream}", true},
		{"reset onto the bare remote name", "git reset --hard origin", true},
		{"bare reset origin/main (implicit --mixed)", "git reset origin/main", true},
		{"reset onto upstream/main", "git reset --soft upstream/main", true},
		{"reset onto long-form remote ref", "git reset --soft refs/remotes/origin/main", true},
		{"glued to a shell operator", "git add -A; git reset --soft origin/main", true},

		// Blocked — an ancestry suffix does not turn a remote target into a local one.
		{"soft to FETCH_HEAD~1", "git reset --soft FETCH_HEAD~1", true},
		{"mixed to FETCH_HEAD~1", "git reset --mixed FETCH_HEAD~1", true},
		{"soft to FETCH_HEAD^", "git reset --soft FETCH_HEAD^", true},
		{"mixed to FETCH_HEAD^", "git reset --mixed FETCH_HEAD^", true},
		{"soft to @{u}~1", "git reset --soft @{u}~1", true},
		{"mixed to @{u}~1", "git reset --mixed @{u}~1", true},
		{"soft to @{upstream}^", "git reset --soft @{upstream}^", true},
		{"soft to FETCH_HEAD~2", "git reset --soft FETCH_HEAD~2", true},
		{"soft to FETCH_HEAD^2", "git reset --soft FETCH_HEAD^2", true},
		{"soft to a stacked suffix", "git reset --soft FETCH_HEAD~2^", true},
		{"soft to a doubled caret", "git reset --soft @{u}^^", true},
		{"soft to bare remote name with suffix", "git reset --soft origin~1", true},
		{"soft to origin/main~1", "git reset --soft origin/main~1", true},
		{"soft to branch@{upstream}~1", "git reset --soft main@{upstream}~1", true},

		// Blocked — a peel suffix (gt-fxqs2) names the same commit the bare
		// revision does, whichever of git's two spellings it uses: "^{}" peels
		// to the commit, "^{<type>}" to an object it reaches.
		{"soft to a peeled @{u}", "git reset --soft @{u}^{}", true},
		{"soft to a commit-peeled FETCH_HEAD", "git reset --soft FETCH_HEAD^{commit}", true},
		{"soft to a peeled @{upstream}", "git reset --soft @{upstream}^{}", true},
		{"soft to a peeled branch@{upstream}", "git reset --soft main@{upstream}^{commit}", true},
		{"soft to a peeled origin/main", "git reset --soft origin/main^{commit}", true},
		{"soft to a peeled bare remote name", "git reset --soft origin^{}", true},
		{"soft to a peeled long-form remote ref", "git reset --soft refs/remotes/origin/main^{tree}", true},
		{"peel after an ancestry suffix", "git reset --soft FETCH_HEAD~1^{commit}", true},
		{"ancestry suffix after a peel", "git reset --soft @{u}^{commit}~2", true},
		{"doubled peel", "git reset --soft @{u}^{}^{}", true},
		{"mixed onto a peeled remote ref", "git reset --mixed origin/main^{commit}", true},

		// Blocked — a target the guard cannot resolve: the shell expands it
		// before git sees it, so the ref it really names is unreadable here
		// (gt-tao2j). A quoted "$(…)" arrives as one token and a bare "$BASE"
		// as one more; an unquoted substitution splits at its spaces, with
		// its first word still carrying the "$(".
		{"soft to a quoted substitution", `git reset --soft "$(git rev-parse origin/main)"`, true},
		{"soft to an unquoted substitution", "git reset --soft $(cat /tmp/base)", true},
		{"soft to an unquoted backtick substitution", "git reset --soft `cat /tmp/base`", true},
		{"soft to a variable", "git reset --soft $BASE", true},
		{"soft to a braced variable", "git reset --soft ${BASE}", true},
		{"soft to a positional parameter", "git reset --soft $1", true},
		{"soft to a backtick substitution", "git reset --soft `git rev-parse origin/main`", true},
		{"soft to a variable with a suffix", "git reset --soft $BASE~1", true},
		{"bare reset to a variable (implicit --mixed)", "git reset $BASE", true},
		{"soft to a variable after a quiet flag", "git reset -q --soft $BASE", true},
		{"soft to a substitution behind git -C", "git -C /tmp/repo reset --soft $(git rev-parse origin/main)", true},

		// Allowed — local targets, pathspecs, and quoted prose.
		{"soft to a local commit", "git reset --soft HEAD~1", false},
		{"soft to a local commit by caret", "git reset --soft HEAD^", false},
		{"soft to a local branch with suffix", "git reset --soft feature/x~3", false},
		{"soft to a local ref ending in digits", "git reset --soft release1", false},
		{"soft to a peeled local commit", "git reset --soft HEAD^{commit}", false},
		{"soft to a peeled local branch", "git reset --soft feature/x^{}", false},
		{"soft to a local reflog entry", "git reset --soft main@{1}", false},
		{"mixed to a local branch", "git reset --mixed feature/x", false},
		{"reset a pathspec named like a ref", "git reset -- origin/main", false},
		{"unstage a file", "git reset HEAD file.go", false},
		// Only the target is judged: a pathspec after it cannot reset
		// anything onto a ref, so an expanded one stays allowed.
		{"unstage an expanded pathspec", "git reset HEAD $file", false},
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
		{"long force flag", "git clean --force", true},
		{"long force flag before -d", "git clean --force -d", true},
		{"long force flag after -d", "git clean -d --force", true},
		{"long force flag with a dry-run flag", "git clean -n --force", true},
		{"long force flag behind a shell operator", "cd /tmp && git clean --force", true},
		{"glued to a shell operator", "cd /tmp && git clean -fdx", true},
		{"backgrounded behind a text-only command", "echo hi & git clean -f", true},
		{"glued to a background operator", "echo hi &git clean -fdx", true},
		{"backgrounded after another command", "sleep 5 & git clean -fd", true},
		{"force flag after a redirection", "git clean 2>&1 -f", true},
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
		{"long flag that merely shares the prefix", "git clean --force-with-lease", false},

		// Allowed — the words are present but no git clean runs.
		{"clean spelled in an echo argument", `echo "clean"`, false},
		{"git clean named inside a quoted echo argument", `echo "git clean -f"`, false},
		{"git clean named as an unquoted echo argument", "echo git clean -f", false},
		{"git clean named in an unquoted gt mail argument", "gt mail send x -s note -m never run git clean -f", false},
		{"git clean named inside a quoted grep pattern", `grep -rn "git clean -f" docs/`, false},
		{"force flag from a probe on another command", `test -f .git/MERGE_HEAD && echo "clean"`, false},
		{"git subcommand other than clean", "git status -f", false},
		{"force flag owned by a later command in the same segment", "git clean -n; git push -f origin main", false},
		{"words split across a background operator", `echo "git clean" & echo "-f"`, false},
		{"clean written to stderr", "echo clean 2>&1", false},
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
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (%q), want allowed", tt.command, reason)
			}
		})
	}
}

// TestMatchesGitResetHard pins the positional reading of `git reset --hard`
// (gt-24lz6): the flag belongs to the reset invocation, not to the token list,
// so the words landing in separate segments is not a hard reset.
func TestMatchesGitResetHard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — a real hard reset.
		{"git reset --hard", "git reset --hard", true},
		{"hard reset of HEAD~1", "git reset --hard HEAD~1", true},
		{"hard reset onto a remote ref", "git reset --hard origin/main", true},
		{"glued to a shell operator", "cd /repo && git reset --hard", true},
		{"behind git's -C option", "git -C /tmp/repo reset --hard", true},
		{"absolute path to git", "/usr/bin/git reset --hard", true},
		{"behind a launcher", "time git reset --hard", true},
		{"handed to xargs", "find . -name '*.go' | xargs git reset --hard", true},

		// Allowed — no hard reset runs.
		{"soft reset", "git reset --soft HEAD~1", false},
		{"bare reset", "git reset", false},
		{"hard flag on a later git subcommand", `git commit -m "--hard"`, false},
		{"reset in one segment, --hard in another", "git log --oneline; grep -n reset CHANGELOG.md; grep -n -- --hard CHANGELOG.md", false},
		{"behind a text-only command", "echo git reset --hard", false},
		{"behind an unquoted gt mail argument", "gt mail send x -s note -m never git reset --hard", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesGitResetHard(shellTokenize(tt.command))
			if blocked := got != ""; blocked != tt.blocked {
				t.Errorf("matchesGitResetHard(%q) blocked=%v (%q), want %v", tt.command, blocked, got, tt.blocked)
			}
		})
	}
}

// TestMatchesDDLDestruction pins the positional reading of the DDL block
// (gt-24lz6): the verb and its object must sit adjacent in one segment, with
// the verb in command position.
func TestMatchesDDLDestruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// Blocked — a real statement.
		{"drop table", "drop table users", true},
		{"uppercase DROP TABLE", "DROP TABLE users", true},
		{"mixed case drop Table", "drop Table users", true},
		{"drop database", "drop database mydb", true},
		{"truncate table", "truncate table logs", true},
		{"after a non-text command", "mysql -e drop table users", true},
		{"glued to a shell operator", "cd /tmp && truncate table logs", true},
		{"in a later segment", "echo preparing; drop database mydb", true},

		// Allowed — the words appear, but not as one statement.
		{"drop and table in different segments", "grep -n drop docs/notes.md; grep -n table docs/schema.md", false},
		{"truncate and table in different segments", "grep -n truncate notes.md; grep -n table schema.md", false},
		{"object before the verb", "table drop users", false},
		{"drop with an unrelated object", "drop index idx_users", false},
		{"behind a text-only command", "echo drop table users", false},
		{"behind a text-only command, uppercase", "echo DROP TABLE users", false},
		{"behind a text-only command, truncate", "grep -n truncate table docs/schema.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesDDLDestruction(shellTokenize(tt.command))
			if blocked := got != ""; blocked != tt.blocked {
				t.Errorf("matchesDDLDestruction(%q) blocked=%v (%q), want %v", tt.command, blocked, got, tt.blocked)
			}
		})
	}
}

// TestGitResetAndDDLMentionsOnCompoundLinesAreAllowed pins gt-24lz6 end to
// end: the two containment matchers that outlived gt-775d rejected ordinary
// work whose segments happened to spell the fragments between them. Each
// case below was blocked before the positional rewrite.
func TestGitResetAndDDLMentionsOnCompoundLinesAreAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{
			"git, reset and --hard supplied by three segments",
			"git log --oneline; grep -n reset CHANGELOG.md; grep -n -- --hard CHANGELOG.md",
		},
		{
			"drop and table supplied by two segments",
			"grep -n drop docs/notes.md; grep -n table docs/schema.md",
		},
		{
			"truncate and table supplied by two segments",
			"grep -n truncate docs/notes.md; grep -n table docs/schema.md",
		},
		{
			"drop table as a text-only argument",
			"echo drop table users",
		},
		{
			"git reset --hard as a text-only argument",
			"echo git reset --hard",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (%q), want allowed", tt.command, reason)
			}
		})
	}
}

// TestGitCleanLongForceFlagReachesGuard pins the other half of gt-24lz6: the
// long spelling of the force flag walked straight past the guard, which
// blocked only -f/-fd/-fdx. Every command here is a real untracked-file wipe.
func TestGitCleanLongForceFlagReachesGuard(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"git clean --force",
		"git clean --force -d",
		"git clean -d --force",
		"cd /tmp && git clean --force",
	} {
		t.Run(command, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — --force is -f", command)
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
		// Options may precede the install subcommand (gt-qis3f): the command
		// word is the manager and "install" is any argument, not necessarily
		// the first.
		{"apt-get -y install", "apt-get -y install curl", true},
		{"brew --quiet install", "brew --quiet install node", true},
		{"npm -g install", "npm -g install typescript", true},
		{"npm --global install", "npm --global install eslint", true},

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
		// gt-qis3f: the two words supplied by separate segments or a
		// text-only mention are not an install invocation.
		{"cross-segment apt/install", "grep -n apt README.md; grep -n install Makefile", false},
		{"echo apt install", "echo apt install foo", false},
		// gt-bdct5: an assignment's substitution must not hide the package
		// manager behind it, whether the value is a backtick substitution, a
		// quoted ")" or a ";" inside the substitution.
		{"install behind a backtick substitution", "y=`echo a` apt-get install foo", true},
		{"install behind a quoted paren in a substitution", `y=$(echo ")" ) apt-get install foo`, true},
		{"install behind a semicolon in a substitution", "y=$(echo a; echo b) apt-get install foo", true},
		// gt-8rxxy: a redirection ahead of the manager hid it from the scan.
		{"install behind a stderr redirect", "2>/dev/null apt-get install foo", true},
		{"install behind a redirect whose target is its own word", "2> /dev/null dnf install foo", true},
		{"install behind two redirections", "2>/dev/null 3>&1 yum install foo", true},
		{"install behind an fd allocated by name", "{fd}>/tmp/log apt-get install foo", true},
		{"install behind a heredoc operator with a strip-tabs tag", "<<- EOF apt-get install foo", true},
		// The word after a redirection is the target, not the manager.
		{"redirect target spelled like a manager", "2>apt-get install foo", false},
		// The substitution body may hold unquoted words, and the whole word is
		// stepped over (gt-8rxxy).
		{"install behind a process substitution with a spaced body", "<(echo hi) apt-get install foo", true},
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

// TestPackageInstallJudgedLikeThePlainForm pins the gt-8rxxy gate at the guard
// entry: a redirection ahead of the manager changes nothing the shell runs, so
// the prefixed install earns the reason the plain form earns.
func TestPackageInstallJudgedLikeThePlainForm(t *testing.T) {
	t.Parallel()
	want, _ := evaluateDangerousCommand("apt-get install -y curl", 0, noTownSession)
	if want == "" {
		t.Fatalf("the plain form was allowed; the comparison rests on it being blocked")
	}
	for _, prefix := range []string{
		"2>/dev/null ",
		"2> /dev/null ",
		">/dev/null ",
		"&>/dev/null ",
		"2>&1 ",
		"{log}>/dev/null ",
		"2>/dev/null 3>&1 ",
	} {
		command := prefix + "apt-get install -y curl"
		if got, _ := evaluateDangerousCommand(command, 0, noTownSession); got != want {
			t.Errorf("evaluateDangerousCommand(%q) = %q, want %q like the plain form", command, got, want)
		}
	}
}

// TestRedirectionWidth pins which tokens are redirections and how many tokens
// each occupies. The operator is matched whole, so a heredoc tag, an allocated
// fd name, a strip-tabs "-" or a clobber "|" is not mistaken for the file the
// redirection targets — the mistake that left the command word hidden behind
// it (gt-8rxxy).
func TestRedirectionWidth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		width int
	}{
		{"stdout redirect with an attached target", ">/dev/null", 1},
		{"stdout redirect with a target word", ">", 2},
		{"stderr redirect with an attached target", "2>/dev/null", 1},
		{"stderr redirect with a target word", "2>", 2},
		{"append redirect", ">>log", 1},
		{"append redirect with a target word", ">>", 2},
		{"read-write redirect", "<>file", 1},
		{"read-write redirect with a target word", "<>", 2},
		{"clobber redirect", ">|log", 1},
		{"clobber redirect with a target word", ">|", 2},
		{"redirect of both streams", "&>log", 1},
		{"redirect of both streams with a target word", "&>", 2},
		{"append of both streams", "&>>log", 1},
		{"append of both streams with a target word", "&>>", 2},
		{"fd duplication", "2>&1", 1},
		{"fd duplication of stdout", ">&2", 1},
		{"fd close", "2>&-", 1},
		{"stdin close", "<&-", 1},
		{"heredoc with an attached tag", "<<EOF", 1},
		{"heredoc with a tag word", "<<", 2},
		{"strip-tabs heredoc with an attached tag", "<<-EOF", 1},
		{"strip-tabs heredoc with a tag word", "<<-", 2},
		{"herestring with attached text", "<<<text", 1},
		{"herestring with a word", "<<<", 2},
		{"fd allocated by name", "{fd}>log", 1},
		{"fd allocated by name with a target word", "{fd}>", 2},
		{"named fd duplication", "{fd}>&2", 1},
		{"process substitution is a word, not a redirection", "<(cmd)", 0},
		{"output process substitution is a word", ">(cmd)", 0},
		{"unterminated fd name", "{fd", 0},
		{"plain word", "echo", 0},
		{"bare number", "2", 0},
		{"number followed by a word", "2foo", 0},
		{"operator inside a word", "foo>bar", 0},
		{"dash", "-", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redirectionWidth(tt.token); got != tt.width {
				t.Errorf("redirectionWidth(%q) = %d, want %d", tt.token, got, tt.width)
			}
		})
	}
}

// TestSegmentCommandWordPastRedirections pins the command word behind a
// leading redirection, including the operator forms whose target is not the
// word that follows them (gt-8rxxy).
func TestSegmentCommandWordPastRedirections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		segment []string
		want    string
		args    []string
	}{
		{"no redirection", []string{"make", "gate"}, "make", []string{"gate"}},
		{"redirection with an attached target", []string{"2>/dev/null", "apt-get", "install"}, "apt-get", []string{"install"}},
		{"redirection with a target word", []string{"2>", "/dev/null", "apt-get", "install"}, "apt-get", []string{"install"}},
		{"two redirections", []string{"2>/dev/null", "3>&1", "bash", "--norc"}, "bash", []string{"--norc"}},
		{"fd allocated by name", []string{"{fd}>log", "bash", "-c", "x"}, "bash", []string{"-c", "x"}},
		{"strip-tabs heredoc with a tag word", []string{"<<-", "EOF", "bash"}, "bash", nil},
		{"heredoc with a tag word", []string{"<<", "EOF", "bash"}, "bash", nil},
		{"herestring with a word", []string{"<<<", "make gate", "bash"}, "bash", nil},
		{"process substitution ahead of the command", []string{"<(cmd)", "make", "gate"}, "make", []string{"gate"}},
		{"output process substitution ahead of the command", []string{">(tee)", "apt-get", "install"}, "apt-get", []string{"install"}},
		{"process substitution with a spaced body", []string{"<(echo", "hi)", "apt-get", "install"}, "apt-get", []string{"install"}},
		{"process substitution whose body holds its own pair", []string{"<(echo", "$(date)", "hi)", "make"}, "make", nil},
		{"process substitution that never closes", []string{"<(echo", "hi", "apt-get", "install"}, "", nil},
		{"redirection only", []string{"2>/dev/null"}, "", nil},
		{"operator with no target", []string{"2>"}, "", nil},
		{"assignment after a redirection", []string{"2>/dev/null", "X=1", "make"}, "make", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			word, args := segmentCommandWordPastRedirections(tt.segment)
			if word != tt.want {
				t.Fatalf("segmentCommandWordPastRedirections(%q) word = %q, want %q", tt.segment, word, tt.want)
			}
			if strings.Join(args, "\x00") != strings.Join(tt.args, "\x00") {
				t.Errorf("segmentCommandWordPastRedirections(%q) args = %q, want %q", tt.segment, args, tt.args)
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

// TestSlotRunPayloadsWithoutDashDash pins the fix for an argv run through
// gt slot run with no `--` before it: slot.Run execs the positionals either
// way, so the payload is read from the words after slot run's own flags
// rather than from after a `--` (gt-sni9y). The `--` forms keep the payload
// they had, and a word that only looks like a flag (-rf) stays in the argv —
// it is what slot will exec, and dropping it would hide `rm -rf /`.
func TestSlotRunPayloadsWithoutDashDash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"no -- and no flags", "gt slot run git push origin main", []string{"git push origin main"}},
		{"no -- after a role flag", "gt slot run --role gastown/nux git push origin main", []string{"git push origin main"}},
		{"-- trailing the argv", "gt slot run git push origin main --", []string{"git push origin main"}},
		{"no -- after a glued role flag", "gt slot run --role=gastown/nux git push origin main", []string{"git push origin main"}},
		{"-- before the argv", "gt slot run -- make test", []string{"make test"}},
		{"-- before the argv, after a role flag", "gt slot run --role gastown/nux -- make test", []string{"make test"}},
		{"-- timeout value is not the command", "gt slot run --timeout 5m make test", []string{"make test"}},
		{"flag-shaped argv word stays", "gt slot run rm -rf /", []string{"rm -rf /"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := slotRunPayloads(shellTokenize(tt.command))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("slotRunPayloads(shellTokenize(%q)) = %q, want %q", tt.command, got, tt.want)
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
		// A -o cluster owns the next word as its value, so the -c payload
		// behind it is still the payload.
		{"bash -euo pipefail -c payload", `bash -euo pipefail -c 'git reset --hard origin/main'`, true},
		{"bash -eo pipefail -c payload", `bash -eo pipefail -c "git clean -fd"`, true},
		{"bash -o pipefail -c payload", `bash -o pipefail -c 'git reset --hard origin/main'`, true},
		{"bash +o noglob -c payload", `bash +o noglob -c 'git reset --hard origin/main'`, true},
		{"bash -euo pipefail -c safe command", `bash -euo pipefail -c 'echo hello'`, false},
		{"bash -oe pipefail -c payload", `bash -oe pipefail -c 'git reset --hard origin/main'`, true},
		{"bash -co pipefail payload", `bash -co pipefail 'git reset --hard origin/main'`, true},
		{"bash -O extglob -c payload", `bash -O extglob -c 'git reset --hard origin/main'`, true},
		{"bash -oo with two values -c payload", `bash -oo pipefail errexit -c 'git reset --hard origin/main'`, true},
		{"script file piped into bash", `cat ./run.sh | bash`, false},
		{"bash -euo pipefail script file", `bash -euo pipefail ./run.sh -c 'git reset --hard origin/main'`, false},
		// A quoted script fed to a shell on stdin runs, whether a pipe or a
		// here-string carries it (the test guards read the same shapes).
		{"quoted reset piped into bash", `echo 'git reset --hard origin/main' | bash`, true},
		{"quoted reset piped into sh -s", `echo 'git reset --hard origin/main' | sh -s`, true},
		{"quoted reset printf piped into a path-qualified shell", `printf '%s\n' 'git reset --hard origin/main' | /bin/bash`, true},
		{"quoted reset piped through cat into bash", `echo 'git reset --hard origin/main' | cat | bash`, true},
		{"quoted reset piped into bash -eo pipefail", `echo 'git reset --hard origin/main' | bash -eo pipefail`, true},
		{"quoted reset here-string into bash", `bash <<< 'git reset --hard origin/main'`, true},
		{"quoted reset here-string glued", `sh <<<"git clean -fd"`, true},
		{"quoted reset here-string into bash -euo pipefail", `bash -euo pipefail <<< 'git reset --hard origin/main'`, true},
		{"quoted reset piped into bash after a chain", `cd /tmp && echo 'git reset --hard origin/main' | bash`, true},
		{"quoted reset piped into grep", `echo 'git reset --hard origin/main' | grep reset`, false},
		{"quoted reset piped into a script file", `echo 'git reset --hard origin/main' | bash ./run.sh`, false},
		{"quoted reset here-string into grep", `grep reset <<< 'git reset --hard origin/main'`, false},
		{"harmless script piped into bash", `echo 'ls -la' | bash`, false},
		// A redirection word is not a script operand: the shell still reads
		// its script from stdin (gt-sveq8).
		{"quoted reset piped into bash 2>&1", `echo 'git reset --hard origin/main' | bash 2>&1`, true},
		{"quoted reset here-string into bash 2>&1", `bash 2>&1 <<< 'git reset --hard origin/main'`, true},
		{"quoted reset piped into bash > out", `echo 'git reset --hard origin/main' | bash > out`, true},
		{"harmless script piped into bash 2>&1", `echo 'ls -la' | bash 2>&1`, false},
		{"quoted reset piped into bash 2>&1 script file", `echo 'git reset --hard origin/main' | bash 2>&1 ./run.sh`, false},
		{"quoted reset piped into bash < script file", `echo 'git reset --hard origin/main' | bash < ./run.sh`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
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
			reason, _ := evaluateDangerousCommand(command, 0, noTownSession)
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

		// A body holding a parenthesis is cut whole, not at the first ")"
		// (gt-d1mlb).
		{"quoted paren before a reset", `x=$(grep -E '(a|b)' f; git reset --hard)`, true},
		{"nested substitution before a reset", `x=$(echo $(date); git reset --hard)`, true},
		{"reset inside a nested substitution", `x=$(echo $(git reset --hard))`, true},
		{"subshell before a reset", `x=$( (cd d); git reset --hard )`, true},
		{"quoted paren, safe body", `x=$(grep -E '(a|b)' f)`, false},
		{"single-quoted substitution is data", `echo '$(git reset --hard)'`, false},
		{"single-quoted backtick is data", "echo '`git reset --hard`'", false},
		{"escaped substitution is data", `echo \$(git reset --hard)`, false},
		{"eval of a single-quoted substitution runs it", `eval '$(git reset --hard)'`, true},
		{"bash -c of a single-quoted substitution runs it", `bash -c 'x=$(grep -E "(a|b)" f; git reset --hard)'`, true},

		// A substitution the shell would reject is judged rather than passed
		// unread (gt-6v07e).
		{"unterminated dollar-paren is judged", `echo $(git reset --hard`, true},
		{"unterminated backtick is judged", "echo `git reset --hard", true},
		{"unterminated quote in the body is judged", `echo $(git reset --hard "x`, true},
		{"unterminated dollar-paren, safe body", `echo $(ls -la`, false},
		{"unterminated quote, safe body", `echo $(ls -la "x`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
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
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), noTownSession)
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
		{"heads short form", "git push origin HEAD:heads/main", true, true},
		{"heads short form, master", "git push origin HEAD:heads/master", true, true},
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
	t.Parallel()
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
		{"GT_ROLE witness beats a stale path marker", "gastown/witness", worktree, false},
		{"GT_ROLE mayor beats a stale path marker", "mayor", worktree, false},
		{"GT_ROLE witness beats a stale path marker", "gastown/witness", worktree, false},
		{"no GT_ROLE falls back to the path marker", "", worktree, true},
		{"neither signal", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{"GT_ROLE": tt.gtRole, "GT_POLECAT_PATH": tt.gtPolecatPath}, "")
			if got := inPolecatSession(proc); got != tt.want {
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
	t.Parallel()
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
		{"crew with a stale path marker", "gastown/crew/max", worktree, false},
		{"crew with a stale path marker", "gastown/crew/alice", worktree, false},
		{"neither signal", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": tt.gtRole, "GT_POLECAT_PATH": tt.gtPolecatPath}, "")}
			reason, _ := evaluateDangerousCommand(incident, 0, sess)
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
		sess := guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/polecats/flint"}, "")}
		nested := `bash -c "git push origin HEAD:main"`
		if reason, _ := evaluateDangerousCommand(nested, 0, sess); reason == "" {
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
			if reason, _ := matchesUnboundedScan(tokens, noTownSession); reason != "" {
				t.Fatalf("matchesUnboundedScan false-fired: %q", reason)
			}
			if reason := matchesGitResetHard(tokens); reason != "" {
				t.Fatalf("matchesGitResetHard false-fired: %q", reason)
			}
			if reason := matchesDDLDestruction(tokens); reason != "" {
				t.Fatalf("matchesDDLDestruction false-fired: %q", reason)
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
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason == "" {
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
	if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
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

// TestShellTokenizeSplitsBackgroundAmpersand pins the gt-wwwht tokenizer
// rule: a background '&' starts a new command exactly as ';' does and must
// surface as its own separator token — while a '&' that belongs to a
// redirection (2>&1, &>file) stays glued to the redirect and splits nothing.
// The split is what stops a text-only command in front of the '&' from
// speaking for the real command behind it ("echo hi & git clean -f").
func TestShellTokenizeSplitsBackgroundAmpersand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"background ampersand is its own token", "echo hi & git clean -f",
			[]string{"echo", "hi", "&", "git", "clean", "-f"}},
		{"glued background ampersand still splits", "echo hi&git clean -f",
			[]string{"echo", "hi", "&", "git", "clean", "-f"}},
		{"file-descriptor duplication stays one token", "git clean -f 2>&1",
			[]string{"git", "clean", "-f", "2>&1"}},
		{"stderr redirection stays one token", "foo >&2",
			[]string{"foo", ">&2"}},
		{"stdout-and-stderr redirect stays one token", "make test &>/tmp/log",
			[]string{"make", "test", "&>/tmp/log"}},
		{"appending stdout-and-stderr redirect stays one token", "make test &>>/tmp/log",
			[]string{"make", "test", "&>>/tmp/log"}},
		{"ampersand spaced from a redirect is background", "make test & >/tmp/log",
			[]string{"make", "test", "&", ">/tmp/log"}},
		{"double ampersand is one token", "echo hi && git status",
			[]string{"echo", "hi", "&&", "git", "status"}},
		{"quoted ampersand stays opaque", `echo "a & b"`,
			[]string{"echo", "a & b"}},
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
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason == "" {
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
	if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
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
			reason, alternative := evaluateDangerousCommand(tt.command, 0, noTownSession)
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
		// The apostrophe is comment text, not a quote the shell opened, so
		// the operator below is still a real one and its body still data
		// (gt-79opc).
		{"apostrophe in a comment above the operator", "# don't touch the host\ncat > note.md <<'EOF'\nsudo rm -rf /tmp\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — heredoc body is data", tt.command, reason)
			}
		})
	}
}

// TestEvaluateDangerousCommandUnquotedHeredocSubstitution pins gt-x50o8: the
// shell expands $(...) and backticks in an unquoted heredoc body, so a
// dangerous command written there runs even though the body is otherwise data.
// A quoted tag keeps the body literal, so the same text stays allowed.
func TestEvaluateDangerousCommandUnquotedHeredocSubstitution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		command     string
		wantBlocked bool
	}{
		{"unquoted dollar-paren", "cat > /tmp/x <<EOF\n$(git reset --hard origin/main)\nEOF", true},
		{"unquoted backtick", "cat > /tmp/x <<EOF\n`git reset --hard origin/main`\nEOF", true},
		{"unquoted strip-tabs form", "cat > /tmp/x <<-EOF\n\t$(git reset --hard origin/main)\n\tEOF", true},
		{"unquoted substitution amid prose", "cat > /tmp/x <<EOF\nreset now: $(git reset --hard) done\nEOF", true},
		{"unquoted harmless substitution", "cat > /tmp/x <<EOF\nbranch: $(git rev-parse --abbrev-ref HEAD)\nEOF", false},
		// A backslash in an unquoted body escapes the "$(", so the reader
		// gets the text literally and the shell runs nothing: data, not a
		// command (gt-y6mhx). An even run of backslashes escapes itself, so
		// the substitution is live again.
		{"escaped dollar-paren is literal", "cat > /tmp/x <<EOF\n\\$(git reset --hard origin/main)\nEOF", false},
		{"escaped dollar-paren amid prose", "cat > /tmp/x <<EOF\nwrite \\$(git reset --hard) verbatim\nEOF", false},
		{"escaped backtick is literal", "cat > /tmp/x <<EOF\n\\`git reset --hard origin/main\\`\nEOF", false},
		{"doubled backslash leaves the substitution live", "cat > /tmp/x <<EOF\n\\\\$(git reset --hard origin/main)\nEOF", true},
		{"single-quoted tag is literal", "cat > /tmp/x <<'EOF'\n$(git reset --hard origin/main)\nEOF", false},
		{"double-quoted tag is literal", "cat > /tmp/x <<\"EOF\"\n$(git reset --hard origin/main)\nEOF", false},
		{"single-quoted tag backtick is literal", "cat > /tmp/x <<'EOF'\n`git reset --hard origin/main`\nEOF", false},
		{"double-quoted tag backtick is literal", "cat > /tmp/x <<\"EOF\"\n`git reset --hard origin/main`\nEOF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.wantBlocked && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the shell runs the substitution", tt.command)
			}
			if !tt.wantBlocked && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed", tt.command, reason)
			}
		})
	}
}

// TestEvaluateDangerousCommandHeredocFedShellBehindLauncher pins that a launcher
// in front of the shell does not hide it from the heredoc reader check: timeout
// runs the bash that reads the body, so the body is judged as a script. Each
// row has a mirror without the launcher.
func TestEvaluateDangerousCommandHeredocFedShellBehindLauncher(t *testing.T) {
	t.Parallel()
	const body = "git reset --hard origin/main\nEOF"
	tests := []struct {
		name        string
		reader      string
		wantBlocked bool
	}{
		{"bash reads the body", "bash <<'EOF'", true},
		{"timeout 5 bash reads the body", "timeout 5 bash <<'EOF'", true},
		{"doas -u alice bash reads the body", "doas -u alice bash <<'EOF'", true},
		{"timeout 5 grep reads the body as data", "timeout 5 grep reset <<'EOF'", false},
		{"grep reads the body as data", "grep reset <<'EOF'", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			command := tt.reader + "\n" + body
			reason, _ := evaluateDangerousCommand(command, 0, noTownSession)
			if got := reason != ""; got != tt.wantBlocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", command, got, reason, tt.wantBlocked)
			}
		})
	}
}

// TestHeredocDoesNotHideRealCommand ensures stripHeredocBodies only
// removes the body span and leaves surrounding shell text — including a
// second, genuinely dangerous command after the terminator — intact. The
// middle cases are the spellings of "<<" the shell does not read as a
// heredoc operator: a shift inside a $(("..")) arithmetic expansion and a
// "<" behind a backslash (gt-yza8u), a shift after a nested $((".."))
// whose "))" the outer expansion outlives (gt-schaa), and a shift in any
// of the other arithmetic contexts — a command-position "((" or "for ((",
// a "$[", an array subscript, and the offset of a "${name:..}" (gt-dherr).
// The rest are the "${..}" spellings whose word is a pattern or plain text —
// the "#" and "%" trims, the "/" replacement, the "^" case modification, and
// the "+" and "=" operator words (gt-adt8r): nothing inside a parameter
// expansion is a redirection, so a "<<" there is a literal too.
// Reading any of them as a heredoc stripped the lines below it, so the live
// command standing where the body would have been was never judged at all.
// A "let" argument is the exception that stays a heredoc: bash reads its
// "<<" as a redirection, so stripping that body is the right answer.
func TestHeredocDoesNotHideRealCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{
			name:      "a real command after a real heredoc",
			command:   "cat > note.md <<'EOF'\nordinary content\nEOF\nsudo rm -rf /",
			wantBlock: true,
		},
		{
			name:      "arithmetic shift, not a heredoc operator",
			command:   "echo $((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "escaped < is a literal, then a single-< redirect",
			command:   "echo \\<<EOF\nsudo rm -rf /\nEOF",
			wantBlock: true,
		},
		{
			name:      "a double angle in a comment is text, not a heredoc",
			command:   "echo hi # cat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: true,
		},
		{
			name:      "arithmetic shift over a force push",
			command:   "echo $((1<<EOF))\ngit push --force origin x\nEOF",
			wantBlock: true,
		},
		{
			name:      "shift after a nested arithmetic expansion",
			command:   "echo $(( $((1)) << x ))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift after a parenthesized arithmetic term",
			command:   "echo $(( (1) << x ))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a command-position arithmetic command",
			command:   "((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a command-position arithmetic assignment",
			command:   "(( y = 1 << x ))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic expansion of the old form",
			command:   "echo $[1<<x]\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a substring offset",
			command:   "v=abcdef; echo ${v:1<<x:2}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a C-style for header",
			command:   "for ((i=1<<x;i<3;i++)); do :; done\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to then",
			command:   "if true; then((1<<x))\nsudo rm -rf /\nx\nfi",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to do",
			command:   "while true; do((1<<x))\nsudo rm -rf /\nx\ndone",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to else",
			command:   "if false; then :; else((1<<x))\nsudo rm -rf /\nx\nfi",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to elif",
			command:   "if false; then :; elif((1<<x))\nsudo rm -rf /\nx\nthen :; fi",
			wantBlock: true,
		},
		{
			name:      "shift in a C-style for header glued to for",
			command:   "for((i=1<<x;i<3;i++)); do :; done\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to while",
			command:   "while((1<<x))\nsudo rm -rf /\nx\ndo :; done",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to time",
			command:   "time((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to a negation",
			command:   "!((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to a brace group",
			command:   "{((1<<x));}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to a case pattern",
			command:   "case y in y)((1<<x));; esac\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command glued to a function definition",
			command:   "f()((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an arithmetic command after time and a space",
			command:   "time ((1<<x))\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an array subscript",
			command:   "a[1<<x]=3\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a trim-right pattern",
			command:   "echo ${v%<<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a trim-left pattern",
			command:   "echo ${v#<<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a replace pattern",
			command:   "echo ${v/<<x/y}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in an if-set word",
			command:   "echo ${v+<<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in a case-modification pattern",
			command:   "echo ${v^<<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "shift in the pattern of a nested expansion",
			command:   "echo ${a:-${v%<<x}}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "a real heredoc body stays data",
			command:   "cat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "the [ test command is not an arithmetic context",
			command:   "if [ -f x ]; then\ncat <<EOF\nsudo rm -rf /\nEOF\nfi",
			wantBlock: false,
		},
		{
			name:      "a default-value expansion does not hide the heredoc after it",
			command:   "echo ${v:-fallback}\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "a nested expansion does not hide the heredoc after it",
			command:   "echo ${a:-${b}}\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "a hash in an expansion word is no comment, so the heredoc after it stays data",
			command:   "echo ${v% # }\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			// The "# } <<EOF" is a comment in the substitution's command text,
			// so no heredoc opens and the "sudo" line is a live command: blocked.
			name:      "a hash in a command substitution nested in an expansion is a comment, so the sudo line after it is live",
			command:   "echo ${v:-$(echo # } <<EOF\n)}\nsudo rm -rf /\nEOF",
			wantBlock: true,
		},
		{
			// The "}" in "$(echo })" is that command's argument. The expansion
			// ends at the last "}", so the "<<x" before it is text in the word
			// and the "sudo" line is a live command: blocked.
			name:      "a brace in a nested command substitution does not close the expansion",
			command:   "echo ${v:-$(echo }) <<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			// The "${b}" opened inside the substitution closes itself, so the
			// "}" before the final one still belongs to the outer expansion.
			name:      "an expansion nested in a command substitution closes at its own brace",
			command:   "echo ${v:-$(echo ${b}) <<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "a comment after a closed nested substitution in an expansion",
			command:   "echo ${v:-$(echo x) # }\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "a shift in a let argument is a real heredoc",
			command:   "let y=1<<x\nsudo rm -rf /\nx",
			wantBlock: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.wantBlock && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the shell reads a command here, so it must still be checked", tt.command)
			}
			if !tt.wantBlock && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data for its reader", tt.command, reason)
			}
		})
	}
}

// TestHeredocInsideQuotedSubstitutionStaysOpaque is the double-quoted half of
// the reading above. A heredoc a double-quoted command substitution holds is
// still a heredoc: its body is data, so an apostrophe in it opens no quote and
// a dangerous phrase in it is not a command. The ")" that closes the
// substitution ends the double quote the "$(" interrupted, so text after it is
// judged as it would be with no heredoc at all, and a substitution nested
// inside closes at its own paren rather than swallowing the one that follows
// (gt-lx8i8).
func TestHeredocInsideQuotedSubstitutionStaysOpaque(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{
			name:      "dangerous phrase in a body with an apostrophe is data",
			command:   "x=\"$(cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n)\"",
			wantBlock: false,
		},
		{
			name:      "real command after the substitution is still judged",
			command:   "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"; sudo rm -rf /tmp",
			wantBlock: true,
		},
		{
			name:      "shell-fed body inside a double-quoted substitution is still a script",
			command:   "x=\"$(bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)\"",
			wantBlock: true,
		},
		{
			name:      "nested substitution inside the body's substitution closes at its own paren",
			command:   "x=\"$(echo $(date); cat <<'EOF'\nit's a note\nEOF\n)\"; sudo rm -rf /tmp",
			wantBlock: true,
		},
		{
			name:      "second heredoc after the substitution is data",
			command:   "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"; cat > note.md <<'Z'\nsudo rm -rf /tmp\nZ\n",
			wantBlock: false,
		},
		{
			name:      "heredoc after a nested substitution is not inside it",
			command:   "x=\"$(echo $(date))\"; cat > note.md <<'Z'\nsudo rm -rf /tmp\nZ\n",
			wantBlock: false,
		},
		{
			name:      "command after a nested substitution is still judged",
			command:   "x=\"$(echo $(date))\"; sudo rm -rf /tmp",
			wantBlock: true,
		},
		{
			name:      "arithmetic inside the substitution does not close it",
			command:   "x=\"$(echo $((1+2)); cat <<'EOF'\nit's a note\nEOF\n)\"; sudo rm -rf /tmp",
			wantBlock: true,
		},
		{
			name:      "shell-fed body inside a nested substitution is still a script",
			command:   "x=\"$(echo $(bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n))\"",
			wantBlock: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if got := reason != ""; got != tt.wantBlock {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.wantBlock)
			}
		})
	}
}

// gt-1f28p: a heredoc fed to a shell inside backticks or a process substitution
// is judged the way one inside "$(..)" is (gt-2am7w). The reader line alone
// hides the shell: "x=`bash" is an assignment, and "cat <(bash" names cat.
func TestShellFedHeredocInBacktickOrProcessSubstitution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		command    string
		wantBlock  bool
		wantReason string // the reason the judged body is refused, when it is
	}{
		{
			name:       "shell-fed heredoc inside backticks is a script",
			command:    "x=`bash <<EOF\ngit reset --hard\nEOF\n`",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:       "shell-fed heredoc inside a process substitution is a script",
			command:    "cat <(bash <<EOF\ngit reset --hard\nEOF\n)",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "shell-fed heredoc inside a process output substitution is a script",
			command:   "cat > >(bash <<EOF\ngit reset --hard\nEOF\n)",
			wantBlock: true,
		},
		{
			name:      "shell-fed heredoc inside backticks in double quotes is a script",
			command:   "echo \"`bash <<EOF\ngit reset --hard\nEOF\n`\"",
			wantBlock: true,
		},
		{
			name:      "shell-fed heredoc inside backticks inside a substitution is a script",
			command:   "x=$(echo `bash <<EOF\ngit reset --hard\nEOF\n`)",
			wantBlock: true,
		},
		{
			name:      "shell-fed heredoc in a process substitution inside a substitution is a script",
			command:   "x=$(cat <(bash <<EOF\ngit reset --hard\nEOF\n))",
			wantBlock: true,
		},
		{
			name:      "a command after a closed process substitution is still judged",
			command:   "cat <(ls); sudo rm -rf /tmp",
			wantBlock: true,
		},
		{
			name:      "heredoc fed to cat inside backticks stays data",
			command:   "x=`cat <<EOF\ngit reset --hard\nEOF\n`",
			wantBlock: false,
		},
		{
			name:      "heredoc fed to cat inside a process substitution stays data",
			command:   "cat <(cat <<EOF\ngit reset --hard\nEOF\n)",
			wantBlock: false,
		},
		{
			name:      "harmless shell-fed heredoc inside backticks is allowed",
			command:   "x=`bash <<EOF\necho hi\nEOF\n`",
			wantBlock: false,
		},
		{
			name:      "heredoc after a closed backtick pair stays data",
			command:   "x=`echo hi`; cat > note.md <<'Z'\ngit reset --hard\nZ\n",
			wantBlock: false,
		},
		{
			name:      "a close paren in backtick text is that command's own, so the heredoc after it stays data",
			command:   "x=`echo )`; cat <<EOF\ngit reset --hard\nEOF",
			wantBlock: false,
		},
		{
			name:      "a substitution in backtick text closes at its own paren",
			command:   "x=`echo $(date)`; cat <<EOF\ngit reset --hard\nEOF",
			wantBlock: false,
		},
		{
			name:       "shell-fed heredoc inside a substitution in backtick text is a script",
			command:    "x=`echo $(bash <<EOF\ngit reset --hard\nEOF\n)`",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:       "shell-fed heredoc inside a process substitution in backtick text is a script",
			command:    "x=`cat <(bash <<EOF\ngit reset --hard\nEOF\n)`",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "heredoc fed to cat inside double-quoted backticks stays data",
			command:   "echo \"`cat <<EOF\ngit reset --hard\nEOF\n`\"",
			wantBlock: false,
		},
		{
			name:      "heredoc inside backticks then a heredoc after them stays data",
			command:   "x=`cat <<EOF\ngit reset --hard\nEOF\n`; cat <<Z\ngit reset --hard\nZ",
			wantBlock: false,
		},
		{
			name:      "heredoc fed to cat in backticks closed inside a substitution stays data, and so does the one after",
			command:   "x=$(echo `cat <<EOF\ngit reset --hard\nEOF\n`); cat <<Z\ngit reset --hard\nZ",
			wantBlock: false,
		},
		{
			name:       "a substitution still open when the backtick closes does not hide the command after it",
			command:    "x=`echo $(echo`; git reset --hard",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:       "a process substitution still open when the backtick closes does not hide the command after it",
			command:    "x=`cat <(echo`; git reset --hard",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "a close paren in a process substitution's own text ends only that text",
			command:   "cat <(echo ); cat <<EOF\ngit reset --hard\nEOF",
			wantBlock: false,
		},
		{
			name:      "a quoted close paren in a process substitution is text",
			command:   "cat <(echo \")\"); cat <<EOF\ngit reset --hard\nEOF",
			wantBlock: false,
		},
		{
			name:       "a process substitution nested in another is command text",
			command:    "cat <(cat <(bash <<EOF\ngit reset --hard\nEOF\n))",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "a process substitution fed from a file is command text",
			command:   "cat < <(bash <<EOF\ngit reset --hard\nEOF\n)",
			wantBlock: true,
		},
		{
			name:       "shell-fed heredoc in a process substitution inside an expansion is a script",
			command:    "echo ${v:-<(bash <<EOF\ngit reset --hard\nEOF\n)}",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "a process substitution inside an expansion leaves the heredoc after it data",
			command:   "echo ${v:-<(echo x)}; cat <<EOF\ngit reset --hard\nEOF",
			wantBlock: false,
		},
		{
			name:      "a close paren in a heredoc body inside a process substitution is body text",
			command:   "cat <(cat <<EOF\n)\ngit reset --hard\nEOF\n)",
			wantBlock: false,
		},
		{
			name:       "shell-fed heredoc with an indented terminator is a script",
			command:    "cat <(bash <<-EOF\n\tgit reset --hard\n\tEOF\n)",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "heredoc fed to cat with an indented terminator stays data",
			command:   "cat <(cat <<-EOF\n\tgit reset --hard\n\tEOF\n)",
			wantBlock: false,
		},
		{
			name:       "a quoted paren in a shell-fed body does not end the process substitution early",
			command:    "cat <(bash <<EOF\necho \")\" ; git reset --hard\nEOF\n)",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if got := reason != ""; got != tt.wantBlock {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.wantBlock)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want it to name %q", tt.command, reason, tt.wantReason)
			}
		})
	}
}

// An escaped backtick inside backtick text opens a command of its own, and the
// shell re-parses the outer text before it runs that command: the reader of a
// heredoc there is the word after the escaped backtick, not the assignment the
// outer text starts with. Reading the outer text's start as the reader line hid
// the shell, so the body went unchecked (gt-tzh06).
func TestShellFedHeredocInNestedBacktickText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		command    string
		wantBlock  bool
		wantReason string // the reason the judged body is refused, when it is
	}{
		{
			name:       "shell-fed heredoc in escaped backticks is a script",
			command:    "x=`echo \\`bash <<EOF\ngit reset --hard\nEOF\n\\``",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
		{
			name:      "heredoc fed to cat in escaped backticks stays data",
			command:   "x=`echo \\`cat <<EOF\ngit reset --hard\nEOF\n\\``; cat <<Z\ngit reset --hard\nZ",
			wantBlock: false,
		},
		{
			name:       "a data heredoc after the escaped texts does not hide the script",
			command:    "x=`echo \\`bash <<EOF\ngit reset --hard\nEOF\n\\``; cat <<Z\ngit reset --hard\nZ",
			wantBlock:  true,
			wantReason: "Hard reset",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if got := reason != ""; got != tt.wantBlock {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, got, reason, tt.wantBlock)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want it to name %q", tt.command, reason, tt.wantReason)
			}
		})
	}
}

// A "}" inside the backtick text of a "${..}" word is that command's own text,
// as it is inside "$(..)" (gt-nwd3k): the expansion ends at the last "}", so the
// "<<x" before it is text in the word and the "sudo" line is a live command
// (gt-9udxk).
func TestHeredocWalkBacktickBraceInsideParamExpansion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		command   string
		wantBlock bool
	}{
		{
			name:      "a brace in backtick text does not close the expansion",
			command:   "echo ${v:-`echo }` <<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "a brace in backtick text inside double quotes does not close the expansion",
			command:   "echo \"${v:-`echo }` <<x}\"\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "the same brace in a command substitution still does not close the expansion",
			command:   "echo ${v:-$(echo }) <<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "a closed backtick pair in an expansion leaves its closing brace live",
			command:   "echo ${v:-`echo x`}\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "an escaped backtick opens no command text",
			command:   "echo ${v:-\\`echo }\\`}\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "a brace in backtick text nested in backtick text does not close the expansion around it",
			command:   "echo `echo ${v:-\\`echo }\\` <<x}`\nsudo rm -rf /\nx",
			wantBlock: true,
		},
		{
			name:      "a closed nested backtick pair leaves the braces after it live",
			command:   "echo `echo ${v:-\\`echo x\\`}`\ncat <<EOF\nsudo rm -rf /\nEOF",
			wantBlock: false,
		},
		{
			name:      "an unescaped backtick ends the outer text whatever is nested in it",
			command:   "echo ${v:-`echo \\`echo }` <<x}\nsudo rm -rf /\nx",
			wantBlock: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.wantBlock && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked: the shell reads a command here", tt.command)
			}
			if !tt.wantBlock && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed: the body is data for its reader", tt.command, reason)
			}
		})
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
		// The pipe stage is continued onto its own line, so the operator line's
		// newline comes after it and the body starts below that.
		{"top-level body piped through a continued line", "cat <<'EOF' \\\n| bash\nsudo rm -rf /tmp\nEOF"},
		{"invoker split across a line continuation", "bash \\\n  <<'EOF'\ngit reset --hard\nEOF"},
		// bash -c runs its argument, but the body is still bash's stdin: the
		// "$(cat)" here expands to the body, which is then what runs.
		{"body read back through the -c substitution", "bash -c \"$(cat)\" <<'EOF'\nsudo rm -rf /\nEOF"},
		// An unquoted "$(" runs its command text in the shell, so the reader
		// line the heredoc sits on begins at the assignment, not at bash.
		{"shell-fed body inside an unquoted substitution", "x=$(bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		{"shell-fed body inside an unquoted substitution opened on an earlier line", "x=$(\nbash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		// The pipe stage sits on the continuation line, after the operator line's newline.
		{"shell-fed body piped through a continued line inside an unquoted substitution", "x=$(cat <<'EOF' | \\\nbash\nsudo rm -rf /tmp\nEOF\n)"},
		{"shell-fed body after a nested substitution in the command text", "x=$(echo $(echo a); bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		{"shell-fed body after a quoted parenthesis in the command text", "x=$(echo \")\"; bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		{"shell-fed body after a subshell inside an unquoted substitution", "x=$( (true); bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		// A leading subshell paren puts the shell at the start of the command.
		{"shell-fed body behind a subshell paren", "(bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		// An assignment whose value holds a substitution runs that substitution
		// first, and the shell that follows it still reads the body.
		{"shell-fed body behind an assignment with a substitution", "y=$(echo a) bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		// The same holds when the substitution is a backtick one (gt-bdct5).
		{"shell-fed body behind an assignment with a backtick substitution", "y=`echo a` bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		// A brace group puts a second opener between the launcher or assignment
		// and the shell, so the command word lies behind both of them.
		{"shell-fed body behind a brace group after a launcher", "time { bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n}"},
		{"shell-fed body behind a brace group after an assignment with a substitution", "y=$(echo a) { bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n}"},
		// A case pattern opens no construct, so only the trim before the reader
		// reaches the shell that follows it.
		{"shell-fed body behind a case pattern whose case is on an earlier line", "case $x in\na) bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n;;\nesac\n"},
		// A negation is a launcher the command word skips, so the subshell
		// paren behind it is the command word the shell sits at.
		{"shell-fed body behind a subshell paren after a negation", "! ( bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n)"},
		// A quoted "$(" is a literal, not a substitution: the shell still runs
		// bash after the assignment, so the body it reads is still checked.
		{"shell-fed body behind a literal dollar-paren assignment", "X='$(' bash <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		// A redirection ahead of the shell hides it from a command-word scan
		// unless the redirection is stepped over (gt-pdbzu).
		{"shell-fed body behind a leading stderr redirect", "2>/dev/null bash <<'EOF'\ngit reset --hard\nEOF"},
		{"shell-fed body behind a redirect whose target is its own word", "2> /dev/null bash <<'EOF'\ngit reset --hard\nEOF"},
		{"shell-fed body behind a leading stdout redirect", ">/dev/null bash <<'EOF'\ngit reset --hard\nEOF"},
		{"shell-fed body behind two leading redirections", "2>/dev/null 3>&1 bash <<'EOF'\ngit reset --hard\nEOF"},
		{"shell-fed body behind a redirect after a launcher", "env 2>/dev/null bash <<'EOF'\ngit reset --hard\nEOF"},
		{"shell-fed body behind a redirect after an assignment", "X=1 2>/dev/null bash <<'EOF'\ngit reset --hard\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — a shell invoker runs the heredoc body", tt.command)
			}
		})
	}
}

// TestShellFedHeredocJudgedLikeThePlainForm pins the gt-pdbzu gate: a
// redirection ahead of the shell changes nothing the shell does with the body,
// so every prefixed form is judged with the reason the plain form earns.
func TestShellFedHeredocJudgedLikeThePlainForm(t *testing.T) {
	t.Parallel()
	const body = "\ngit reset --hard\nEOF"
	want, _ := evaluateDangerousCommand("bash <<'EOF'"+body, 0, noTownSession)
	if want == "" {
		t.Fatalf("the plain form was allowed; the comparison rests on it being blocked")
	}
	for _, prefix := range []string{
		"2>/dev/null ",
		"2> /dev/null ",
		">/dev/null ",
		"&>/dev/null ",
		"2>&1 ",
		"2>/dev/null 3>&1 ",
		"env 2>/dev/null ",
		"X=1 2>/dev/null ",
	} {
		command := prefix + "bash <<'EOF'" + body
		if got, _ := evaluateDangerousCommand(command, 0, noTownSession); got != want {
			t.Errorf("evaluateDangerousCommand(%q) = %q, want %q like the plain form", command, got, want)
		}
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
		{"non-shell reader inside an unquoted substitution", "x=$(cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n)"},
		{"non-shell reader behind a subshell paren", "(cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n)"},
		{"non-shell reader behind an assignment with a substitution", "y=$(echo a) cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n"},
		{"non-shell reader behind a brace group after a launcher", "time { cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n}"},
		{"non-shell reader behind a brace group after an assignment with a substitution", "y=$(echo a) { cat <<'EOF'\nit's a note: sudo rm -rf /tmp\nEOF\n}"},
		// The continued line is a redirect, not a shell, so the body stays data.
		{"non-shell reader continued onto a second line", "cat <<'EOF' \\\n> note.txt\nsudo rm -rf /tmp\nEOF"},
		{"heredoc after a closed substitution is not in its command text", "x=$(echo bash)\ncat <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		// The "$(echo a)" closes its frame at the ")" that returns to zero, so
		// the bash on the next line is not command text of the heredoc's line.
		{"heredoc after a closed substitution does not inherit the next line", "x=$(echo a)\nbash\ncat <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		{"heredoc after a nested substitution closes back to the outer frame", "x=$(echo $(echo a) b)\ncat <<'EOF'\nsudo rm -rf /tmp\nEOF\n"},
		// Stepping over the redirection must not turn its target into a command:
		// the body is written to the file "bash", no shell reads it.
		{"shell name as a redirection target", "2>bash <<'EOF'\ngit reset --hard\nEOF"},
		{"non-shell reader behind a leading stderr redirect", "2>/dev/null python3 - <<'EOF'\nsudo rm -rf /\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data", tt.command, reason)
			}
		})
	}
}

// TestHeredocCommandTextStopsAtTheReaderLine: the command text of a "$(..)"
// is read to name the heredoc's reader, not to pull an earlier line of the
// substitution into the body's judgement. A bash line above a cat heredoc is
// a separate command, so the body it does not read stays data, while the same
// body on the substitution opener's line still reaches bash (gt-9q6sf).
func TestHeredocCommandTextStopsAtTheReaderLine(t *testing.T) {
	t.Parallel()
	const body = "git reset --hard"
	direct := "x=$(bash <<'EOF'\n" + body + "\nEOF\n)"
	if reason, _ := evaluateDangerousCommand(direct, 0, noTownSession); reason == "" {
		t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — bash reads the body", direct)
	}
	earlier := "x=$(bash ./gen.sh\ncat <<'EOF'\n" + body + "\nEOF\n)"
	if reason, _ := evaluateDangerousCommand(earlier, 0, noTownSession); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — cat writes the body as data", earlier, reason)
	}
}

// TestHeredocCommandTextClampOnContinuedReaderLines: the command text starts at
// the heredoc reader's own logical line, so a line above reaches it only when a
// backslash joins that line to the reader's. A continuation makes the line above
// one command line with the reader, and its shell invoker reads the body; a
// continuation inside the reader line leaves the line above out, so its cat
// reads the body as data. The reader line itself is read whole either way, so a
// pipe stage continued onto the next physical line still reaches bash
// (gt-3yhp0).
func TestHeredocCommandTextClampOnContinuedReaderLines(t *testing.T) {
	t.Parallel()
	const body = "git reset --hard"
	tests := []struct {
		name     string
		command  string
		contains string // text the command text must reach, proving the clamp did not cut it
		lacks    string // text the command text must not reach, proving the clamp cut it
		blocked  bool
	}{
		{
			"continuation joins the bash line above onto the reader",
			"x=$(bash ./gen.sh \\\ncat <<'EOF'\n" + body + "\nEOF\n)",
			"bash ./gen.sh", "", true,
		},
		{
			"continuation inside the reader line keeps the line above out",
			"x=$(bash ./gen.sh\ncat \\\n<<'EOF'\n" + body + "\nEOF\n)",
			"", "bash ./gen.sh", false,
		},
		{
			"a pipe stage continued onto the next line still reaches bash",
			"x=$(bash ./gen.sh\ncat <<'EOF' | \\\nbash\n" + body + "\nEOF\n)",
			"bash", "bash ./gen.sh", true,
		},
		{
			"a redirection ahead of the reader command",
			"x=$(bash ./gen.sh\n2>/dev/null cat <<'EOF'\n" + body + "\nEOF\n)",
			"2>/dev/null cat", "bash ./gen.sh", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			spans := scanHeredocs(tt.command)
			if len(spans) != 1 {
				t.Fatalf("scanHeredocs(%q) found %d heredocs, want 1", tt.command, len(spans))
			}
			cmdText := spans[0].cmdText
			if tt.contains != "" && !strings.Contains(cmdText, tt.contains) {
				t.Errorf("cmdText %q does not reach %q, want the reader's logical line read whole", cmdText, tt.contains)
			}
			if tt.lacks != "" && strings.Contains(cmdText, tt.lacks) {
				t.Errorf("cmdText %q reaches %q, want it to start at the reader's line", cmdText, tt.lacks)
			}
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want blocked=%v", tt.command, blocked, reason, tt.blocked)
			}
		})
	}
}

// TestHeredocOperatorLineEndHonorsEscapesAndComments pins where the operator's
// line ends, which decides where the shell reads the body. An even run of
// backslashes escapes itself, so the newline after "\\" ends the line. A
// backslash inside a "#" comment is comment text, not a continuation, and a
// "#" inside quotes starts no comment. A misplaced end either judges a data
// body as command text or leaves the command after the real terminator
// unjudged (gt-jnomk).
func TestHeredocOperatorLineEndHonorsEscapesAndComments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The newline ends the line, so the terminator is directly below and
		// the rm is a command of its own, not an unterminated body.
		{"escaped backslash before the newline ends the operator line", "cat <<'EOF' \\\\\nEOF\nsudo rm -rf /tmp", true},
		{"backslash ending a comment does not continue the operator line", "cat <<'EOF' # note \\\nEOF\nsudo rm -rf /tmp", true},
		// The body is cat's input, so it is data, not a command to judge.
		{"body under a comment ending in a backslash stays data", "cat <<'EOF' # note \\\nsudo rm -rf /tmp\nEOF", false},
		{"body after an escaped backslash stays data", "cat <<'EOF' \\\\\nsudo rm -rf /tmp\nEOF", false},
		// The " #" is inside quotes, so the backslash continues the line and
		// the pipe stage on the next line reads the body.
		{"hash inside quotes is not a comment", "cat <<'EOF' ' #' \\\n| bash\nsudo rm -rf /tmp\nEOF", true},
		// The newline inside the open quote is part of the word, so the line
		// runs on to the continued pipe stage that feeds the body to bash.
		{"newline inside single quotes does not end the operator line", "cat <<'EOF' 'x\n' \\\n| bash\nsudo rm -rf /tmp\nEOF", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.blocked && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the line ends at the newline the shell reads", tt.command)
			}
			if !tt.blocked && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data", tt.command, reason)
			}
		})
	}
}

// TestHeredocReaderWalkBackHonorsBackslashParity pins gt-fke2f: the walk-back
// joins the line above the heredoc operator only when that line ends in an odd
// run of backslashes. In an even run each backslash escapes the next, so the
// shell ends the line at the newline and its words are not on the reader line
// — the body is data, not a script the shell above runs.
func TestHeredocReaderWalkBackHonorsBackslashParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// "bash \\" is bash with one literal backslash argument, and the
		// newline ends that command; cat is the reader of the body below it.
		{"an escaped backslash above the operator line ends it", "bash \\\\\ncat <<'EOF'\nsudo rm -rf /tmp\nEOF", false},
		// Trailing spaces after the even run do not make it a continuation.
		{"an escaped backslash followed by spaces ends the line", "bash \\\\  \ncat <<'EOF'\nsudo rm -rf /tmp\nEOF", false},
		// An odd run still continues the line, so the invoker above joins the
		// operator line and the body is its script.
		{"an odd backslash run above the operator line continues it", "bash \\\\\\\n  <<'EOF'\ngit reset --hard\nEOF", true},
		// A non-word-leading "#" is literal, so the backslash after it still
		// continues the line and the invoker above joins the operator line.
		{"a non-comment hash above a trailing backslash continues the line", "bash x#y \\\n<<'EOF'\ngit reset --hard\nEOF", true},
		// A double quote opened on an earlier line is still open on this one,
		// so the "#" is literal and the backslash continues the line into the
		// operator line; bash, which opened the quote, reads the body.
		{"a quote opened above carries a hash line into the operator line", "bash \"q\nz # w \\\n\" <<'EOF'\nsudo rm -rf /tmp\nEOF", true},
		// A comment that starts its own line ends at the newline, so the line
		// above is not joined and cat reads the body as data.
		{"a comment starting a line does not join the operator line", "echo a\n# bash \\\ncat <<'EOF'\ngit reset --hard\nEOF", false},
		// The even run is an escaped backslash inside the open double quote,
		// so the newline after it is still inside the string and the line
		// continues into the operator line, which bash's quote reaches.
		{"an even backslash run inside an open quote still continues the line", "bash \"q\nz \\\\\n\" <<'EOF'\nsudo rm -rf /tmp\nEOF", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.blocked && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the continued line keeps the invoker on the reader line", tt.command)
			}
			if !tt.blocked && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the shell ends the line at the newline, so the body is data", tt.command, reason)
			}
		})
	}
}

// TestHeredocReaderWalkBackAllowedCasesInspectTheBody pins gt-c6kva: an allowed
// row is allowed because its body is data to cat, and the walk still reached
// that body. The control keeps the same operator line and body under a shell
// reader, which must block; without it, a guard that never read the body would
// pass the allowed rows too.
func TestHeredocReaderWalkBackAllowedCasesInspectTheBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		control string
	}{
		{"an escaped backslash above the operator line ends it", "bash \\\\\ncat <<'EOF'\nsudo rm -rf /tmp\nEOF", "bash \\\\\nbash <<'EOF'\nsudo rm -rf /tmp\nEOF"},
		{"an escaped backslash followed by spaces ends the line", "bash \\\\  \ncat <<'EOF'\nsudo rm -rf /tmp\nEOF", "bash \\\\  \nbash <<'EOF'\nsudo rm -rf /tmp\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data to cat", tt.command, reason)
			}
			if reason, _ := evaluateDangerousCommand(tt.control, 0, noTownSession); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the same body under a shell reader is a script, so the walk must reach it", tt.control)
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
	if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
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
	if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
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
		{"git reset --soft onto a quoted command substitution", `git reset --soft "$(git rev-parse origin/main)"`, true},
		{"git reset --soft onto a bare variable", "git reset --soft $BASE", true},
		{"git reset --soft onto a variable holding the remote ref", "BASE=$(git rev-parse origin/main); git reset --soft $BASE", true},
		{"git clean -f", "git clean -f", true},
		{"git clean -fd", "git clean -fd", true},
		{"git clean --force", "git clean --force", true},
		{"go clean -modcache behind go's -C dir flag", "go -C /tmp clean -modcache", true},
		{"drop table", "DROP TABLE users", true},

		// Blocked — the argv after gt slot run -- is exec'd, not text (gt-hqenc)
		{"git reset --hard under gt slot run", "gt slot run --role gastown/nux -- git reset --hard origin/main", true},
		{"git push --force under gt slot run", "gt slot run --role gastown/nux -- git push --force origin x", true},
		{"git clean -fd under gt slot run", "gt slot run --role gastown/nux -- git clean -fd", true},
		{"rm -rf / under gt slot run", "gt slot run --role gastown/nux -- rm -rf /", true},
		{"env-prefixed rm -rf / under gt slot run", "gt slot run -- FOO=1 rm -rf /", true},
		{"bash -c under gt slot run", "gt slot run -- bash -c 'git reset --hard'", true},
		{"gt slot run after another command", "cd x && gt slot run -- git push --force origin x", true},
		{"git clean -fd under gt slot run with no --", "gt slot run git clean -fd", true},
		{"rm -rf / under gt slot run with no -- after a role flag", "gt slot run --role gastown/nux rm -rf /", true},

		// Blocked — a "<<" the shell never takes as a redirection leaves the
		// lines after it live, so nothing there may be stripped (gt-79opc).
		// Each of these evaluates to allowed before the fix: the operator
		// has no terminator line, so the body it was read as swallowed the
		// rest of the command.
		{"git push --force after a quoted <<EOF", "git commit -m \"explain <<EOF handling\"\ngit push --force origin x", true},
		{"git reset --hard after a <<< here-string", "cat <<< hello\ngit reset --hard origin/main", true},
		{"sudo after a quoted <<EOF", "git commit -m \"say <<EOF here\"\nsudo rm -rf /etc", true},
		// A backslash that ends a # comment does not join the operator line to
		// the comment above it, so bash is the reader of the body (gt-3vky2).
		{"a comment ending in a backslash does not join the operator line", "echo hi # note \\\nbash <<EOF\ngit reset --hard\nEOF", true},
		{"a bash continuation above the operator line still joins it", "bash \\\n<<EOF\ngit reset --hard\nEOF", true},
		// In $'...' a backslash escapes the quote, so $'\'' ends the word and
		// the operator line ends at its own newline: the git reset --hard
		// below the real terminator stays live (gt-s3pi5).
		{"git reset --hard after an ANSI-C string on the operator line", "cat <<EOF $'\\''\necho hi\nEOF\necho \\'\ngit reset --hard\nEOF", true},
		{"a plain single-quoted word keeps its backslash literal", "cat <<EOF 'a\\'\necho hi\nEOF\ngit reset --hard", true},

		// Allowed — the same shape with a terminator still reads as data
		{"an ANSI-C string on the operator line above a data reader", "cat <<EOF $'\\''\ndrop table users\nEOF", false},
		{"terminated heredoc body stays data", "cat <<EOF\ndrop table users\nEOF", false},
		{"terminated quoted-tag heredoc body stays data", "git commit -m \"explain <<EOF\"\ncat <<EOF\ndrop table users\nEOF", false},
		{"a comment ending in a backslash above a data reader is allowed", "echo hi # note \\\ncat <<'EOF'\ngit reset --hard\nEOF", false},

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
		{"drop and table in separate segments", "grep -n drop a.md; grep -n table b.md", false},
		{"drop table behind a text-only command", "echo drop table users", false},
		{"make test under gt slot run", "gt slot run -- make test", false},
		{"gt slot run with a role flag", "gt slot run --role gastown/nux -- make test", false},
		{"make test under gt slot run with no --", "gt slot run make test", false},
		{"make test under gt slot run with a role flag and no --", "gt slot run --role gastown/nux make test", false},
		{"dangerous words in a quoted argument under gt slot run", "gt slot run -- git commit -m 'never git push --force'", false},
		{"gt slot run named in text", "echo gt slot run -- git reset --hard", false},
		{"gt slot run named in a gt mail argument", "gt mail send x -s note -m gt slot run -- git reset --hard", false},
		{"git reset --hard behind a text-only command", "echo git reset --hard", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The real entry point, so a row also covers the nested-command
			// walk (bash -c, eval, gt slot run --), not just the top-level
			// matchers.
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			blocked := reason != ""
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
				if reason, _ := matchesUnboundedScan(shellTokenize(command), noTownSession); reason != "" {
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
	t.Parallel()
	home := t.TempDir()
	sess := guardSession{proc: fakeGuardProcess(map[string]string{"HOME": home}, "")}

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
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command), sess)
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
			reason, _ := evaluateDangerousCommand(cmd, 0, noTownSession)
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
			reason, _ := evaluateDangerousCommand(cmd, 0, noTownSession)
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
		{"double-dash flag", "go clean --modcache", true},
		{"flag with =true value", "go clean -modcache=true", true},
		{"path-qualified go", "/usr/local/go/bin/go clean -modcache", true},
		{"path-qualified go, double-dash with value", "/usr/local/go/bin/go clean --testcache=true", true},
		{"go's global -C dir flag before the subcommand", "go -C /tmp clean -modcache", true},
		{"glued -C=dir spelling", "go -C=/tmp clean -cache", true},
		{"repeated -C dir flags", "go -C /tmp -C /var/tmp clean -testcache", true},
		{"-C dir flag and a shared flag with a value", "go -C /tmp clean --fuzzcache=true", true},
		{"-C dir flag after another command on the line", "cd x && go -C /tmp clean -modcache", true},
		{"versioned toolchain binary", "go1.22.0 clean -modcache", true},
		{"versioned toolchain binary, cache flag", "go1.21 clean -cache", true},
		{"path-qualified versioned toolchain binary", "/usr/local/go/bin/go1.22.0 clean -testcache", true},
		{"gotip toolchain binary", "gotip clean -cache", true},
		{"versioned toolchain binary behind -C dir", "go1.22.0 -C /tmp clean -modcache", true},
		// golang.org/dl installs the pre-releases under their own names; the
		// release form alone did not cover them, so the same cache wipe slipped
		// through as `go1.22rc1 clean -modcache` (gt-7cmjp).
		{"release-candidate toolchain binary", "go1.22rc1 clean -modcache", true},
		{"beta toolchain binary", "go1.22beta1 clean -modcache", true},
		{"alpha toolchain binary", "go1.23alpha1 clean -testcache", true},
		{"path-qualified pre-release toolchain binary", "/usr/local/go/bin/go1.22rc1 clean -cache", true},

		// Should allow.
		{"safe flag on go clean, shared flag on a later unrelated command", "go clean -i; ls -la -cache", false},
		{"versioned toolchain binary with a safe flag", "go1.22.0 clean -i", false},
		{"bare go clean", "go clean", false},
		{"go clean -i", "go clean -i", false},
		{"go clean -n", "go clean -n", false},
		{"path-qualified go clean -i", "/usr/local/go/bin/go clean -i", false},
		{"cache as a non-flag argument", "go clean cache", false},
		{"go clean -x", "go clean -x", false},
		{"go clean -r", "go clean -r", false},
		{"go clean of a package", "go clean ./...", false},
		{"go clean of a package behind -C dir", "go -C /tmp clean ./...", false},
		{"-C dir flag on a command that is not go clean", "ls -C /tmp clean -cache", false},
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
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, blocked, reason, tt.blocked)
			}
		})
	}
}

// TestMatchesPolecatFullSuiteUncached pins the gt-v4r0x rule: an uncached
// whole-module 'go test' is denied in a polecat session, in any flag order,
// with or without -json, and behind Go's global -C dir flag (gt-soohm), while
// cached full-tree runs, scoped uncached runs, and -run-filtered runs stay
// allowed — and the same full-tree command stays allowed outside a polecat
// session (crew, refinery, mayor, operator).
func TestMatchesPolecatFullSuiteUncached(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		polecat bool
		blocked bool
	}{
		// Blocked in a polecat session — full tree with -count=1.
		{"plain full tree uncached", "go test -count=1 ./...", true, true},
		{"json full tree uncached", "go test -count=1 -json ./...", true, true},
		{"json before count", "go test -json -count=1 ./...", true, true},
		{"flag order count last", "go test -json ./... -count=1", true, true},
		{"space-separated count", "go test -count 1 ./...", true, true},
		{"bare wildcard", "go test -count=1 ...", true, true},
		{"module-prefixed wildcard", "go test -count=1 github.com/steveyegge/gastown/...", true, true},
		{"full tree plus scoped pkg", "go test -count=1 ./internal/cmd ./...", true, true},
		{"after a shell separator", "cd internal && go test -count=1 ./...", true, true},
		{"other flags present", "go test -count=1 -timeout 10m -race ./...", true, true},
		{"double-dash value flag before the wildcard", "go test -count=1 --run TestFoo ./...", true, true},

		// Go's global -C dir flag sits between the binary and the subcommand
		// in both of its spellings without hiding a full-tree run (gt-soohm).
		{"-C dir before the subcommand", "go -C /tmp test -count=1 ./...", true, true},
		{"-C=dir before the subcommand", "go -C=/tmp test -count=1 ./...", true, true},
		{"-C dir with a space-separated count", "go -C /tmp test -count 1 ./...", true, true},

		// Allowed in a polecat session.
		{"cached full tree", "go test ./...", true, false},
		{"-C dir with a cached full tree", "go -C /tmp test ./...", true, false},
		{"-C dir with a scoped uncached run", "go -C /tmp test -count=1 ./internal/cmd/...", true, false},
		{"-C dir that another tool owns", "make -C /tmp test -count=1 ./...", true, false},
		{"json cached full tree", "go test -json ./...", true, false},
		{"scoped uncached", "go test -count=1 ./internal/cmd/...", true, false},
		{"scoped package uncached", "go test -count=1 ./internal/cmd", true, false},
		{"run-filtered uncached", "go test -count=1 -run TestFoo ./internal/cmd", true, false},
		{"double-dash value is not a package", "go test -count=1 --run ./... ./internal/cmd", true, false},
		{"double-dash value with = is not a package", "go test -count=1 --run=./... ./internal/cmd", true, false},
		{"single-dash value is not a package", "go test -count=1 -run ./... ./internal/cmd", true, false},
		{"presubmit", "make presubmit", true, false},
		{"go build full tree", "go build ./...", true, false},
		{"count not one", "go test -count=3 ./...", true, false},
		{"prose in quoted mail body", `gt mail send x -s "note" -m "never go test -count=1 ./..."`, true, false},
		{"count on a later unrelated command", "go test ./... ; ls -count=1", true, false},

		// Not a polecat session: the same command stays allowed.
		{"crew full tree uncached", "go test -count=1 ./...", false, false},
		{"crew json full tree uncached", "go test -count=1 -json ./...", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesPolecatFullSuiteUncached(lowerTokens(tt.command), tt.polecat)
			if (reason != "") != tt.blocked {
				t.Errorf("matchesPolecatFullSuiteUncached(%q, polecat=%v) blocked=%v, want %v", tt.command, tt.polecat, reason != "", tt.blocked)
			}
			if tt.blocked && !strings.Contains(alternative, "make presubmit") {
				t.Errorf("matchesPolecatFullSuiteUncached(%q) alternative=%q, want it to name make presubmit", tt.command, alternative)
			}
		})
	}
}

// TestPolecatFullSuiteUncachedReachesGuard checks the full
// evaluateDangerousCommand path — including recursion into bash -c/eval
// wrappers — for the uncached-full-suite class, and that the session gate is
// honored end to end.
func TestPolecatFullSuiteUncachedReachesGuard(t *testing.T) {
	t.Parallel()
	polecat := guardSession{proc: fakeGuardProcess(map[string]string{"GT_ROLE": "gastown/polecats/agate"}, "")}
	tests := []struct {
		name    string
		command string
		sess    guardSession
		blocked bool
	}{
		{"top-level polecat", "go test -count=1 ./...", polecat, true},
		{"-C dir polecat", "go -C /tmp test -count=1 ./...", polecat, true},
		{"-C dir polecat, cached", "go -C /tmp test ./...", polecat, false},
		{"bash -c wrapped polecat", `bash -c "go test -count=1 -json ./..."`, polecat, true},
		{"top-level non-polecat", "go test -count=1 ./...", noTownSession, false},
		{"scoped polecat", "go test -count=1 ./internal/cmd/...", polecat, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, tt.sess)
			if blocked := reason != ""; blocked != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, blocked, reason, tt.blocked)
			}
		})
	}
}

// TestIsIdleGatedSuiteStartCommand pins the gt-nqcy follow-up's detection of
// unwrapped full-suite starts, ported from the interim host-hygiene hook:
// 'go test ./...' and 'go build ./...' are gated wherever they run, their -C
// dir spellings included (gt-soohm), 'make test' and 'make build' in a Go
// tree too; scoped invocations, 'gt slot run'-wrapped ones, and quoted prose
// are not. The non-Go tree cases live in
// TestIsIdleGatedSuiteStartOutsideGoTree, which may not be buildable here.
func TestIsIdleGatedSuiteStartCommand(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "clone")
	tests := []struct {
		name    string
		command string
		cwd     string
		gated   bool
	}{
		// Should gate.
		{"make test", "make test", goTree, true},
		{"make test with env prefix", "GOFLAGS=-p=8 make test", goTree, true},
		{"make test with -j flag", "make -j4 test", goTree, true},
		{"make test after another goal", "make clean test", goTree, true},
		{"make build after another goal", "make clean build", goTree, true},
		{"make test after a variable assignment", "make GOFLAGS=-count=1 test", goTree, true},
		{"make build", "make build", goTree, true},
		{"make -C into the Go tree", "make -C " + goTree + " build", "", true},
		{"make test with an unreadable cwd", "make test", "", true},
		{"go test whole repo", "go test ./...", goTree, true},
		{"go test whole repo bare dots", "go test ...", goTree, true},
		{"go build whole repo", "go build ./...", goTree, true},
		{"go test after unrelated segment", "echo hi && go test ./...", goTree, true},
		{"go test glued to semicolon", "cd /tmp;go test ./...", goTree, true},
		{"go -C into a tree, whole repo test", "go -C " + goTree + " test ./...", goTree, true},
		{"go -C=dir, whole repo build", "go -C=" + goTree + " build ./...", goTree, true},

		// Should NOT gate.
		{"scoped go test", "go test ./internal/beads/...", goTree, false},
		{"scoped go build", "go build ./cmd/gt", goTree, false},
		{"go -C dir with a scoped test", "go -C " + goTree + " test ./internal/beads/...", goTree, false},
		{"go -C dir with a flag before the whole-repo arg", "go -C " + goTree + " test -v ./...", goTree, false},
		{"make lint", "make lint", goTree, false},
		{"make clean without a gated goal", "make clean", goTree, false},
		{"wrapped in gt slot run", "gt slot run -- make test", goTree, false},
		{"wrapped go test in gt slot run", "gt slot run --role gastown/flint -- go test ./...", goTree, false},
		{"go test with flag before target (interim adjacency)", "go test -v ./...", goTree, false},
		{"go build with flag before target (interim adjacency)", "go build -v ./...", goTree, false},
		{"quoted mention", `echo "run make test if idle"`, goTree, false},
		{"no suite command at all", "echo hello", goTree, false},
		{"prose in a heredoc body written to a file", "cat > note.md <<'EOF'\nRun make test before committing.\nEOF", goTree, false},
		{"real command after the heredoc still gates", "cat > note.md <<'EOF'\nordinary content\nEOF\nmake test", goTree, true},
		{"slot-run mention trailing an unwrapped command is not a real wrapper", "make test # not actually gt slot run", goTree, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIdleGatedSuiteStartCommand(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got != tt.gated {
				t.Errorf("isIdleGatedSuiteStartCommand(%q, cwd %s) = %v, want %v", tt.command, tt.cwd, got, tt.gated)
			}
		})
	}
}

// A rig outside every Go module runs its own make targets, which are not the
// module-wide Go suite and build the gate's premise names — the fractals rig's
// test target is `npm test` into vitest, so holding it costs the rig its
// suite while protecting nothing on the shared host (gt-mjfir). The tree that
// decides is the one make runs in, so a -C into a Go tree from the non-Go rig
// is still gated, and one out of it is not.
func TestIsIdleGatedSuiteStartOutsideGoTree(t *testing.T) {
	t.Parallel()
	npmTree := nonGoTree(t)
	goTree := fakeModule(t, "clone")
	tests := []struct {
		name    string
		command string
		cwd     string
		gated   bool
	}{
		{"make test", "make test", npmTree, false},
		{"make build", "make build", npmTree, false},
		{"make test with env prefix", "GOFLAGS=-p=8 make test", npmTree, false},
		{"make -C into the Go tree", "make -C " + goTree + " test", npmTree, true},
		{"make -C out of the Go tree", "make -C " + npmTree + " test", goTree, false},
		{"a makefile the guard cannot read", "make -f other.mk test", npmTree, true},
		{"go test whole repo", "go test ./...", npmTree, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isIdleGatedSuiteStartCommand(fakeGuardProcess(nil, tt.cwd), tt.command)
			if got != tt.gated {
				t.Errorf("isIdleGatedSuiteStartCommand(%q, cwd %s) = %v, want %v", tt.command, tt.cwd, got, tt.gated)
			}
		})
	}
}

// The walk's reading of a "||" chain (gt-0lzdi): the shell runs the chain's
// right-hand cd only when the left one fails, so the segments after the chain
// run in that cd's directory only for a left cd the shell certainly refused.
// One that resolves leaves the branch the "||" took unshown, and one the guard
// cannot resolve is not assumed to have failed.
func TestCdWalkOrChain(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	left := filepath.Join(base, "left")
	right := filepath.Join(base, "right")
	for _, dir := range []string{left, right} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	proc := fakeGuardProcess(map[string]string{"HOME": base}, base)
	gone := filepath.Join(base, "gone")

	tests := []struct {
		name    string
		command string
		want    string
		known   bool
	}{
		{"a left cd that resolves leaves the branch unshown", "cd " + left + " || cd " + right + " && grep -rn TODO", "", false},
		{"the same with the chain followed by a semicolon", "cd " + left + " || cd " + right + " ; grep -rn TODO", "", false},
		{"a later cd is not named on a resolved left cd", "cd " + left + " || cd " + right + " && cd " + right + " && grep -rn TODO", "", false},
		{"a left cd the shell refuses takes the right branch", "cd " + gone + " || cd " + right + " && grep -rn TODO", right, true},
		{"the same with a relative right-hand cd", "cd gone || cd right && grep -rn TODO", right, true},
		{"a left cd the shell refuses, with a later cd carried", "cd " + gone + " || cd " + left + " && cd " + right + " && grep -rn TODO", right, true},
		{"cd - is not assumed to have failed", "cd - || cd " + right + " && grep -rn TODO", "", false},
		{"an unexpanded left cd is not assumed to have failed", "cd $GT_NO_SUCH_DIR || cd " + right + " && grep -rn TODO", "", false},
		{"a resolved left cd alone still carries", "cd " + left + " && grep -rn TODO", left, true},
		{"a refused cd before && keeps its unknown reading", "cd " + gone + " && grep -rn TODO", "", false},
		{"a cd in a pipeline still does not carry", "cd " + left + " | grep -rn TODO", base, true},

		// The walk keeps going past a change it cannot place, so that a later
		// absolute cd names the directory again (gt-n7ksl) — but only where
		// the shell still gets there: an "&&" list ends at a cd that failed or
		// cannot be placed, so no cd inside the list after that dead end runs.
		{"a cd inside the dead list is not named", "cd " + gone + " && cd " + left + " ; grep -rn TODO", base, true},
		{"the same after a cd the walk cannot place", "cd - && cd " + left + " ; grep -rn TODO", "", false},
		{"a semicolon still carries the walk past a failed cd", "cd " + gone + " ; cd " + left + " && grep -rn TODO", left, true},

		// A cd the shell refuses leaves the shell where it was, so the walk
		// keeps the directory it already has rather than lose it (gt-34vra):
		// the segment guards judge the line's commands in A, not in the hook
		// cwd, and a relative target after the refusal resolves against A.
		{"a refused cd keeps the directory the walk had", "cd " + left + " ; cd " + gone + " ; grep -rn TODO", left, true},
		{"a relative target after a refused cd keeps its base", "cd " + gone + " ; cd left && grep -rn TODO", left, true},

		// The "&&" list the refusal dead-ended still ends, at the ";" that
		// follows it: the shell reads on from there, so a cd after that ";" is
		// one the walk reads and a cd inside the list is not (gt-34vra).
		{"the walk reads on from the semicolon after a dead list", "cd " + gone + " && true ; cd " + left + " ; grep -rn TODO", left, true},
		{"the same into a relative tree", "cd " + gone + " && true ; cd left && grep -rn TODO", left, true},

		// A "||" or a background "&" inside that dead list is a point the walk
		// does not read from: the "||" turns on whether the list left of it
		// failed and the "&" runs what follows in this shell, so neither
		// leaves the directory the walk was carrying where the shell is.
		{"a || inside the dead list is not read past", "cd " + gone + " && cd " + left + " || cd " + right + " ; grep -rn TODO", "", false},
		{"a background cd inside the dead list is not read past", "cd " + gone + " && cd " + left + " & cd " + right + " ; grep -rn TODO", "", false},
		{"a dead list with no semicolon after it names nothing", "cd " + gone + " && cd " + left, "", false},
		{"a dead list with no semicolon leaves the base unnamable", "cd " + gone + " && cd " + left + " && grep -rn TODO", "", false},

		// A brace group is not a subshell: it runs in this shell, so the cd it
		// holds carries to the segments after it, while the same cd inside a
		// "( ... )" group stays out of them (gt-ajyw8).
		{"a cd inside a brace group carries", "cd " + left + " ; { cd " + right + " ; } ; grep -rn TODO", right, true},
		{"a cd inside a brace group carries after &&", "cd " + left + " && { cd " + right + " ; } && grep -rn TODO", right, true},
		{"a cd inside a subshell does not carry", "cd " + left + " ; (cd " + right + ") ; grep -rn TODO", left, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			got, ok := scanWalkRoot(proc, tokens, len(tokens), shellVarAssignments(tokens))
			if ok != tt.known || (ok && got != tt.want) {
				t.Errorf("scanWalkRoot(%q) = (%q, %v), want (%q, %v)", tt.command, got, ok, tt.want, tt.known)
			}
		})
	}
}

// A "(" opened inside a subshell nests one more level: the ")" that closes the
// inner group leaves the walk still inside the outer one, so a cd later in the
// outer group is the subshell's and the shell stays where it was (gt-1daus).
func TestCdWalkRootNestedSubshell(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	left := filepath.Join(base, "left")
	right := filepath.Join(base, "right")
	for _, dir := range []string{left, right} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	proc := fakeGuardProcess(map[string]string{"HOME": base}, base)

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"a cd after a nested group in the outer group", "cd " + left + " && (true; (true); cd " + right + ";) ; grep -rn TODO", left},
		{"a cd inside the nested group", "cd " + left + " && (true; (cd " + right + ")) ; grep -rn TODO", left},
		{"two nested levels", "cd " + left + " && (true; (true; (true); cd " + right + ");) ; grep -rn TODO", left},
		{"a cd after the closed group still carries", "cd " + left + " && (true; (true)) ; cd " + right + " ; grep -rn TODO", right},
		{"a flat group is unchanged", "cd " + left + " && (cd " + right + ") ; grep -rn TODO", left},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := shellTokenize(tt.command)
			got, status := cdWalkRoot(proc, tokens, len(tokens), base, shellVarAssignments(tokens))
			if status != cdWalkPlaced || got != tt.want {
				t.Errorf("cdWalkRoot(%q) = (%q, %v), want (%q, placed)", tt.command, got, status, tt.want)
			}
		})
	}
}

// The acceptance criterion for gt-me4vs: a segment is judged in the directory
// it runs in, not the one the hook was invoked from. The hole was an agent
// whose cwd is a non-Go tree running the whole module suite through
// 'cd <go tree> && make test' — the cd carries, by the same walk the scan rule
// uses (scanWalkRoot/dirChangeTarget), so the make target is resolved in the Go
// tree it actually runs in. The same line staying in the non-Go tree is left
// alone. The compose rules (make -C, the separators that do not carry, a cd the
// guard cannot resolve) are the walk's, shared with the container-suite and
// test-scope rules.
func TestIdleGateFollowsCdOnTheLine(t *testing.T) {
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
	// Links into and out of the Go tree; the target is a package inside it, a
	// link to the root being found by the lexical walk too (gt-ofj05).
	goLink := symlinkedTree(t, filepath.Join(goRoot, "internal", "cmd"))
	nonGoLink := symlinkedTree(t, nonGo)

	tests := []struct {
		name    string
		cwd     string
		command string
		gated   bool
	}{
		// The hole: the non-Go tree's segment runs in the Go tree anyway.
		{"cd into the Go tree then make test", nonGo, "cd " + goRoot + " && make test", true},
		{"cd into the Go tree then make build", nonGo, "cd " + goRoot + " && make build", true},
		{"cd into the Go tree after a semicolon", nonGo, "cd " + goRoot + "; make test", true},
		{"relative cd into a nested Go tree", nonGo, "cd nested && make test", true},
		{"chained cds, last one into the Go tree", nonGo, "cd nested && cd " + goRoot + " && make test", true},
		{"unrelated segment before the cd", nonGo, "echo building && cd " + goRoot + " && make test", true},

		// The same line in a tree the gate has nothing to hold stays allowed.
		{"cd to the non-Go tree then make test", nonGo, "cd " + nonGo + " && make test", false},
		{"chained cds, last one out of the Go tree", nonGo, "cd " + goRoot + " && cd " + nonGo + " && make test", false},

		// Leaving the Go tree is judged where the command runs too, so the
		// hook's own tree no longer decides a segment that left it.
		{"cd out of the Go tree then make test", goRoot, "cd " + nonGo + " && make test", false},

		// A cd and make's own -C compose: the -C is resolved against the
		// directory the segment runs in, which the cd is what set.
		{"cd into the Go tree, make -C back out", nonGo, "cd " + goRoot + " && make -C " + nonGo + " test", false},
		{"cd out of the Go tree, make -C back in", nonGo, "cd " + nonGo + " && make -C " + goRoot + " test", true},

		// Only "&&" and ";" carry the change — a cd in a pipeline or a
		// background job runs in a subshell of its own, so both keep the
		// directory the shell already had, and one before "||" carries its
		// own change nowhere either (scanWalkRoot's rule; gt-0lzdi).
		{"cd in a pipeline does not carry", nonGo, "cd " + goRoot + " | make test", false},
		{"background cd does not carry", nonGo, "cd " + goRoot + " & make test", false},
		{"cd before || does not carry", nonGo, "cd " + goRoot + " || make test", false},

		// A "||" chain runs the segment in its right-hand cd's directory only
		// when the left cd certainly failed, so the chain neither hides the
		// Go tree the shell is really in (gt-0lzdi) nor holds a segment that
		// stays in the non-Go tree.
		{"cd out of the Go tree before a || chain", goRoot, "cd " + goRoot + " || cd " + nonGo + " && make test", true},
		{"cd into the Go tree before a || chain", nonGo, "cd " + nonGo + " || cd " + goRoot + " && make test", false},
		{"a refused cd runs make in the || chain's Go tree", nonGo, "cd /nonexistent/gt-0lzdi || cd " + goRoot + " && make test", true},
		{"an unresolvable cd does not name the || chain", goRoot, "cd - || cd " + nonGo + " && make test", true},

		// A symlinked directory is judged as the tree it names, both when make
		// changes into it and when the cd does (gt-ofj05).
		{"make -C through a link into the Go tree", nonGo, "make -C " + goLink + " test", true},
		{"cd through a link into the Go tree", nonGo, "cd " + goLink + " && make test", true},
		{"symlinked session cwd in the Go tree", goLink, "make test", true},

		// A cd this process cannot place leaves the directory unknown, so the
		// hold stands rather than being answered by the hook's own cwd. A cd
		// the shell refuses does not: it left the shell where it was
		// (gt-ofj05).
		{"unset variable in the cd target", nonGo, `cd "$GT_OFJ05_UNSET" && make build`, true},
		{"command substitution in the cd target", nonGo, `cd "$(printf /x)" && make test`, true},
		{"a refused cd keeps the hook cwd, Go tree", goRoot, "cd /nonexistent/gt-me4vs && make test", true},
		{"a refused cd keeps the hook cwd, non-Go tree", nonGo, "cd /nonexistent/gt-me4vs && make test", false},

		// The controls: a directory the walk can place is still read on its own
		// merits, so a non-Go tree stays allowed however the line reaches it.
		{"no cd at all, non-Go hook cwd", nonGo, "make test", false},
		{"make -C through a link to a non-Go tree", nonGo, "make -C " + nonGoLink + " test", false},
		{"cd through a link to a non-Go tree", nonGo, "cd " + nonGoLink + " && make test", false},
		{"symlinked session cwd in a non-Go tree", nonGoLink, "make test", false},

		// The walk keeps going past a change it cannot resolve, so a later
		// absolute cd names the tree again, and a change that does not carry
		// never blanks the directory in the first place (gt-n7ksl). pushd
		// changes the directory the same way cd does, and its stack is one the
		// walk does not track, so a popd leaves the segment refused.
		{"a missing cd, then an absolute cd into the Go tree", nonGo, "cd /nonexistent/gt-n7ksl ; cd " + goRoot + " && make test", true},
		{"an unresolvable cd in a pipeline keeps the Go tree", goRoot, "cd /nonexistent/gt-n7ksl | make test", true},

		// A cd the shell refuses leaves the shell where it was, so the walk
		// keeps the tree it had and the segment is judged there (gt-34vra);
		// the "&&" list that refusal dead-ends still ends at the ";" that
		// follows, and the cd the walk reads from there is the one that
		// decides the segment.
		{"a refused cd keeps the Go tree the walk was in", nonGo, "cd " + goRoot + " ; cd /nonexistent/gt-34vra ; make test", true},
		{"a refused cd keeps the non-Go tree the walk was in", goRoot, "cd " + nonGo + " ; cd /nonexistent/gt-34vra ; make test", false},
		{"the walk reads on from the semicolon after a dead list", nonGo, "cd /nonexistent/gt-34vra && true ; cd " + goRoot + " ; make test", true},
		{"an unplaceable cd in a pipeline keeps the Go tree", goRoot, `cd "$GT_OFJ05_UNSET" | make test`, true},
		{"an unplaceable cd in a pipeline stays exempt", nonGo, `cd "$GT_OFJ05_UNSET" | make test`, false},
		{"pushd into the Go tree then make test", nonGo, "pushd " + goRoot + " && make test", true},
		{"pushd then popd leaves the segment refused", goRoot, "pushd " + nonGo + " && popd && make test", true},
		{"pushd -n does not leave the Go tree", goRoot, "pushd -n " + nonGo + " && make test", true},

		// A brace group runs in this shell, so a cd inside one reaches the
		// segment after it — a subshell's does not, and the group's own closing
		// "}" must not hide the make that follows either (gt-ajyw8).
		{"cd into the Go tree inside a brace group", nonGo, "cd " + nonGo + " ; { cd " + goRoot + " ; } ; make test", true},
		{"cd out of the Go tree inside a brace group", goRoot, "cd " + goRoot + " ; { cd " + nonGo + " ; } ; make test", false},
		{"make test inside a brace group in the Go tree", nonGo, "{ cd " + goRoot + " ; make test ; }", true},
		{"cd into the Go tree inside a subshell does not carry", nonGo, "cd " + nonGo + " ; (cd " + goRoot + ") ; make test", false},
		{"cd out of the Go tree inside a subshell does not carry", goRoot, "cd " + goRoot + " ; (cd " + nonGo + ") ; make test", true},

		// The whole-repo go forms are held wherever they run, so the cd
		// changes nothing for them.
		{"go test whole repo in the non-Go tree", nonGo, "go test ./...", true},
		{"cd into the Go tree then go test whole repo", nonGo, "cd " + goRoot + " && go test ./...", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(nil, tt.cwd)
			got := isIdleGatedSuiteStartCommand(proc, tt.command)
			if got != tt.gated {
				t.Errorf("isIdleGatedSuiteStartCommand(%q, cwd %s) = %v, want %v", tt.command, tt.cwd, got, tt.gated)
			}
		})
	}
}

// TestEvaluateIdleGate checks the wiring between the pure command matcher and
// the CPU idle sample: a gated command is held when the sampled idle share is
// below idleGateCPUIdleThreshold, and allowed through when it is at or above
// it, or when the command isn't gated at all. The boundary cases pin that the
// idle share alone decides (gt-6rieh).
func TestEvaluateIdleGate(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "clone")

	tests := []struct {
		name     string
		command  string
		sample   float64
		sampleOK bool
		// wantRead is whether the gate reached the sample at all. A command
		// the gate does not match is answered before the host is read.
		wantRead bool
		wantHeld bool
	}{
		{"held below the idle threshold", "make test", 24.9, true, true, true},
		{"allowed at the idle threshold", "make test", 25, true, true, false},
		{"allowed on a half-idle host", "make test", 48.6, true, true, false},
		{"held at a tenth idle", "make test", 10, true, true, true},
		{"allowed when not a gated command", "make lint", 0, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := func() (float64, bool) { return tt.sample, tt.sampleOK }
			cpuIdle, idleKnown, heldAction := evaluateIdleGate(fakeGuardProcess(nil, goTree), tt.command, sample)
			if held := heldAction != ""; held != tt.wantHeld {
				t.Errorf("evaluateIdleGate(%q) at CPU idle %.1f%% held=%v, want %v", tt.command, tt.sample, held, tt.wantHeld)
			}
			if idleKnown != tt.wantRead {
				t.Errorf("evaluateIdleGate(%q) reported the reading as read=%v, want %v", tt.command, idleKnown, tt.wantRead)
			}
			if idleKnown && cpuIdle != tt.sample {
				t.Errorf("evaluateIdleGate(%q) = reading %.1f, want the sampled %.1f", tt.command, cpuIdle, tt.sample)
			}
		})
	}
}

// An unread sample is not a busy host: a host whose top cannot be driven
// holds nothing, and the gate reports the reading as unread rather than as
// the 0 an idle-but-unreadable host would otherwise be indistinguishable
// from (gt-6rieh). The unread answer never reaches the banner, because only
// a known reading can hold.
func TestEvaluateIdleGateUnreadSampleHoldsNothing(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "clone")

	for _, command := range []string{"make test", "make lint"} {
		sample := func() (float64, bool) { return 0, false }
		cpuIdle, idleKnown, heldAction := evaluateIdleGate(fakeGuardProcess(nil, goTree), command, sample)
		if heldAction != "" {
			t.Errorf("evaluateIdleGate(%q) with an unread sample held %q, want nothing held", command, heldAction)
		}
		if idleKnown {
			t.Errorf("evaluateIdleGate(%q) reported an unread sample as known (%.1f%%)", command, cpuIdle)
		}
	}

	// The gated command is the one the unread sample could have held: it is
	// held when the same host reads idle, so the case above is the failing
	// sample and not a command the gate does not match.
	sample := func() (float64, bool) { return 1, true }
	if _, _, held := evaluateIdleGate(fakeGuardProcess(nil, goTree), "make test", sample); held == "" {
		t.Fatal("make test is not held even at 1% idle: the unread case above proves nothing")
	}
}

// The acceptance criterion for gt-6rieh: the HOLD banner names the reading
// that held it — the CPU idle share, not the load average it replaced — with
// the value the gate sampled and the threshold it missed.
func TestIdleGateHoldBannerNamesTheIdleReading(t *testing.T) {
	t.Parallel()
	goTree := fakeModule(t, "clone")
	proc := fakeGuardProcess(map[string]string{"GT_POLECAT": "coral", "GT_ROLE": "gastown/polecats/coral"}, goTree)
	proc.cpuIdle = func() (float64, bool) { return 12.4, true }

	var buf strings.Builder
	input := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
	if err := tapGuardDangerous(strings.NewReader(input), &buf, proc); err == nil {
		t.Fatalf("tapGuardDangerous(make test) was not held; stderr:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "CPU idle:  12.4% (need >= 25%)") {
		t.Errorf("the HOLD banner does not name the CPU idle reading that held it:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Load avg") {
		t.Errorf("the HOLD banner still reports a load average:\n%s", buf.String())
	}
}

// The acceptance criterion for gt-p7g2m, through the real hook entry point:
// the HOLD banner's advice names the command it held. A polecat held on 'make
// build' must not be told to run the test suite — that advice costs it a
// second full-module run instead of the one it wanted — while a 'make test'
// hold keeps the GOFLAGS advice.
func TestIdleGateHoldAdviceNamesTheHeldCommand(t *testing.T) {
	t.Parallel()
	busy := func() (float64, bool) { return idleGateCPUIdleThreshold - 15, true }
	goTree := fakeModule(t, "clone")

	for _, tt := range []struct {
		command string
		want    string
		notWant string
	}{
		{"make build", "GOFLAGS=-p=8 make build", "make test"},
		{"make test", "GOFLAGS=-p=8 make test", "make build"},
		{"go build ./...", "GOFLAGS=-p=8 go build ./...", "make test"},
		{"go test ./...", "GOFLAGS=-p=8 go test ./...", "make build"},
		{"go -C /tmp test ./...", "GOFLAGS=-p=8 go -C /tmp test ./...", "make test"},
		// The -C directory is named `go`: spelled back from the directory
		// rather than the binary, the advice named "/tmp/go test ./..." — no
		// command at all. The held invocation is the one the operator typed
		// (gt-7cmjp).
		{"go -C /tmp/go test ./...", "GOFLAGS=-p=8 go -C /tmp/go test ./...", "make test"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{
				"GT_POLECAT": "coral",
				"GT_ROLE":    "gastown/polecats/coral",
			}, goTree)
			proc.cpuIdle = busy

			input := `{"tool_name":"Bash","tool_input":{"command":"` + tt.command + `"}}`
			var buf strings.Builder
			if err := tapGuardDangerous(strings.NewReader(input), &buf, proc); err == nil {
				t.Fatalf("tapGuardDangerous(%q) was not held; stderr:\n%s", tt.command, buf.String())
			}
			if !strings.Contains(buf.String(), "SUITE START HELD") {
				t.Fatalf("no HOLD banner for %q:\n%s", tt.command, buf.String())
			}
			if !strings.Contains(buf.String(), tt.want) {
				t.Errorf("banner for %q does not advise %q:\n%s", tt.command, tt.want, buf.String())
			}
			if strings.Contains(buf.String(), tt.notWant) {
				t.Errorf("banner for %q advises something it did not hold (%q):\n%s", tt.command, tt.notWant, buf.String())
			}
		})
	}
}

// The acceptance criterion, through the real hook entry point: a polecat in a
// non-Go rig runs its rig's suite target while the host's CPU idle share is
// below the idle gate's threshold (gt-mjfir). The Go tree beside it shows the
// gate still holds what it was built for, and names the reason on stderr.
func TestRunTapGuardDangerous_NonGoRigSuiteTargetWhileHostBusy(t *testing.T) {
	t.Parallel()
	busy := func() (float64, bool) { return idleGateCPUIdleThreshold - 15, true }
	for _, tt := range []struct {
		name   string
		rigDir string
		held   bool
	}{
		{"non-Go rig", nonGoTree(t), false},
		{"Go tree", fakeModule(t, "clone"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{
				"GT_POLECAT": "coral",
				"GT_ROLE":    "fractals/polecats/coral",
			}, tt.rigDir)
			proc.cpuIdle = busy

			input := `{"tool_name":"Bash","tool_input":{"command":"make test"}}`
			var buf strings.Builder
			err := tapGuardDangerous(strings.NewReader(input), &buf, proc)
			if held := err != nil; held != tt.held {
				t.Errorf("tapGuardDangerous(make test, cwd %s) held = %v (err %v) stderr:\n%s", tt.rigDir, held, err, buf.String())
			}
			if tt.held && !strings.Contains(buf.String(), "SUITE START HELD") {
				t.Errorf("the Go tree's hold carried no banner:\n%s", buf.String())
			}
		})
	}
}

// The acceptance criterion for gt-me4vs, through the real hook entry point: a
// polecat whose cwd is a non-Go tree is held when the same line cds into a Go
// tree first, and left alone when it stays in the non-Go tree. The hook
// threads the guard process through, so this also pins that the segment walk
// reads the session's own working directory rather than the guard's.
func TestRunTapGuardDangerous_IdleGateFollowsCdOnTheLine(t *testing.T) {
	t.Parallel()
	busy := func() (float64, bool) { return idleGateCPUIdleThreshold - 15, true }
	goRoot := fakeModule(t, "clone")
	nonGo := nonGoTree(t)
	for _, tt := range []struct {
		name    string
		command string
		held    bool
	}{
		{"cd into the Go tree", "cd " + goRoot + " && make test", true},
		{"stays in the non-Go tree", "cd " + nonGo + " && make test", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{
				"GT_POLECAT": "coral",
				"GT_ROLE":    "fractals/polecats/coral",
			}, nonGo)
			proc.cpuIdle = busy

			input := `{"tool_name":"Bash","tool_input":{"command":"` + tt.command + `"}}`
			var buf strings.Builder
			err := tapGuardDangerous(strings.NewReader(input), &buf, proc)
			if held := err != nil; held != tt.held {
				t.Errorf("tapGuardDangerous(%q, cwd %s) held = %v (err %v), want %v; stderr:\n%s", tt.command, nonGo, held, err, tt.held, buf.String())
			}
			if tt.held && !strings.Contains(buf.String(), "SUITE START HELD") {
				t.Errorf("the Go tree's hold carried no banner:\n%s", buf.String())
			}
		})
	}
}

// TestShellTokenizeEndsCommentsAtNewline pins gt-vonb1: a '#' that starts a
// word is a shell comment that runs to the end of its LINE. Because the
// tokenizer rewrites an unquoted newline to " ; " before shlex runs, shlex
// (which also treats a word-leading '#' as a comment) used to swallow the rest
// of the whole command, hiding every later line from every guard.
func TestShellTokenizeEndsCommentsAtNewline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"trailing comment ends at the newline", "echo ok # note\ngit reset --hard",
			[]string{"echo", "ok", ";", "git", "reset", "--hard"}},
		{"glued comment ends at the newline", "echo ok #note\ngit push --force",
			[]string{"echo", "ok", ";", "git", "push", "--force"}},
		{"comment after a separator", "ls; # x\ngit reset --hard HEAD~3",
			[]string{"ls", ";", ";", "git", "reset", "--hard", "HEAD~3"}},
		{"leading comment line", "# c\nrm -rf /",
			[]string{";", "rm", "-rf", "/"}},
		{"comment-only command has nothing to run", "# just a note",
			[]string{}},
		{"hash inside a word is not a comment", "echo a#b\nrm -rf /",
			[]string{"echo", "a#b", ";", "rm", "-rf", "/"}},
		{"hash inside double quotes is literal", "echo \"a # b\"\nls",
			[]string{"echo", "a # b", ";", "ls"}},
		{"hash inside single quotes is literal", "echo 'a # b'\nls",
			[]string{"echo", "a # b", ";", "ls"}},
		{"escaped hash is literal", "echo \\#x\nls",
			[]string{"echo", "#x", ";", "ls"}},
		{"parameter length is not a comment", "echo ${#x}\nls",
			[]string{"echo", "${#x}", ";", "ls"}},
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

// TestDangerousCommandBlockedAfterComment is the end-to-end form of gt-vonb1:
// a comment on an earlier line must not let a dangerous later line through.
func TestDangerousCommandBlockedAfterComment(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"echo ok # note\ngit reset --hard",
		"# step 1\ngit reset --hard HEAD~3",
		"ls; # x\ngit clean -fd",
		"echo ok #note\ngit push --force origin main",
	} {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked", command)
		}
	}
}

// TestShellTokenizeEscapedCharBeforeHash pins gt-w6tug: a '#' that follows an
// escaped character is inside the current word, not at the start of one, so it
// starts no comment. startsShellWord used to read an escaped ' ' or ';' as a
// word boundary, and the comment rule then dropped the rest of the LINE, so
// "echo foo\ #x; rm -rf /tmp/p" lost the rm the shell still runs (the escaped
// space keeps '#x' in the same word, and the ';' after it still separates).
func TestShellTokenizeEscapedCharBeforeHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"escaped space before the hash keeps a later rm",
			"echo foo\\ #x; rm -rf /tmp/p",
			[]string{"echo", "foo #x", ";", "rm", "-rf", "/tmp/p"}},
		{"escaped semicolon before the hash keeps a later rm",
			"echo a\\;#b; rm -rf /tmp/p",
			[]string{"echo", "a;#b", ";", "rm", "-rf", "/tmp/p"}},
		{"a real comment after an unescaped space still hides the rm",
			"echo foo #x; rm -rf /tmp/p",
			[]string{"echo", "foo"}},
		{"an escaped backslash leaves a real comment hiding the rm",
			"echo a\\\\ #c; rm -rf /tmp/p",
			[]string{"echo", "a\\"}},
		{"a comment on line one still leaves line two checked",
			"echo foo #x\ngit reset --hard",
			[]string{"echo", "foo", ";", "git", "reset", "--hard"}},
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

// TestDangerousCommandBlockedAfterEscapedHash is the end-to-end form of
// gt-w6tug: an escaped character before a '#' must not hide a dangerous command
// the shell still runs after the next separator.
func TestDangerousCommandBlockedAfterEscapedHash(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"echo foo\\ #x; git reset --hard",
		"echo a\\;#b; git reset --hard HEAD~3",
	} {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked", command)
		}
	}
}

// TestShellTokenizeContinuationBeforeHash pins gt-aa1ji: a backslash-newline is
// a line continuation the shell DELETES, so the rune after it is preceded by
// whatever came before the backslash. startsShellWord used to judge the '#' by
// the literal newline it sits behind, read it as word-leading, and drop the
// rest of the LINE — so "echo a\<newline>#b; rm -rf /tmp/p" lost the rm the
// shell still runs ('#b' stays inside the word "a#b", and the ';' after it
// still separates). A real comment, one whose '#' is not preceded by a deleted
// continuation, must still be dropped to the end of its line.
func TestShellTokenizeContinuationBeforeHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{"continuation before the hash keeps a later rm",
			"echo a\\\n#b; rm -rf /tmp/p",
			[]string{"echo", "a#b", ";", "rm", "-rf", "/tmp/p"}},
		{"chained continuations keep a later rm",
			"echo a\\\n\\\n#b; rm -rf /tmp/p",
			[]string{"echo", "a#b", ";", "rm", "-rf", "/tmp/p"}},
		{"continuation inside double quotes keeps a later rm",
			"echo \"a\\\n#b\"; rm -rf /tmp/p",
			[]string{"echo", "a#b", ";", "rm", "-rf", "/tmp/p"}},
		{"a bare newline before the hash is still a comment",
			"echo a\n#b; rm -rf /tmp/p",
			[]string{"echo", "a", ";"}},
		{"a continuation at the start of the line is still a comment",
			"\\\n#b; rm -rf /tmp/p",
			[]string{}},
		{"a real comment on a later line is still dropped",
			"echo ok # note\nrm -rf /tmp/p",
			[]string{"echo", "ok", ";", "rm", "-rf", "/tmp/p"}},
		{"a real comment after a continuation is still dropped",
			"echo x\\\n#y\n# note\nrm -rf /tmp/p",
			[]string{"echo", "x#y", ";", ";", "rm", "-rf", "/tmp/p"}},
		{"an escaped backslash before the newline still separates",
			"echo a\\\\\n#b; rm -rf /tmp/p",
			[]string{"echo", `a\`, ";"}},
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

// TestDangerousCommandBlockedAfterContinuationHash is the end-to-end form of
// gt-aa1ji: a backslash-newline before a '#' must not hide a dangerous command
// the shell still runs after the next separator.
func TestDangerousCommandBlockedAfterContinuationHash(t *testing.T) {
	t.Parallel()
	for _, command := range []string{
		"echo a\\\n#b; git reset --hard",
		"echo a\\\n\\\n#b; rm -rf /",
		"echo \"a\\\n#b\"; git clean -fd",
	} {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked", command)
		}
	}
}

// TestSplitSpacedCommandFailsTowardChecking pins the gt-vonb1 backstop: if
// shlex yields no tokens for text that still has words in it, the words are
// returned rather than silently dropped.
func TestSplitSpacedCommandFailsTowardChecking(t *testing.T) {
	t.Parallel()
	got := splitSpacedCommand("# x rm -rf /", "# x rm -rf /")
	want := []string{"#", "x", "rm", "-rf", "/"}
	if len(got) != len(want) {
		t.Fatalf("splitSpacedCommand = %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("splitSpacedCommand = %q, want %q", got, want)
		}
	}
	if got := splitSpacedCommand("", " ; "); len(got) != 1 || got[0] != ";" {
		t.Fatalf("separator-only text = %q, want [;]", got)
	}
}

// $'...' is an ANSI-C string in which a backslash escapes the quote: the \' in
// $'\'a' does not end the string. Read as a plain single quote it did, and the
// quote that followed swallowed the commands behind it (deep-review gt-dilqk).
func TestANSICQuotedStringKeepsTheCommandsBehindIt(t *testing.T) {
	t.Parallel()
	tokens := []struct {
		name    string
		command string
		want    []string
	}{
		{"escaped quote stays inside the string", `echo $'\'a' ; rm -rf / ; echo \'`, []string{"echo", "'a", ";", "rm", "-rf", "/", ";", "echo", "'"}},
		{"words in the string stay one token", `echo $'a b' c`, []string{"echo", "a b", "c"}},
		{"an apostrophe in the string", `echo $'it\'s' ok`, []string{"echo", "it's", "ok"}},
		{"an escaped backslash ends the string normally", `echo $'\\' ; ls`, []string{"echo", `\`, ";", "ls"}},
		{"a double quote in the string", `echo $'say "hi"' x`, []string{"echo", `say "hi"`, "x"}},
		{"a plain single quote is unchanged", `echo 'a $b' c`, []string{"echo", "a $b", "c"}},
	}
	for _, tt := range tokens {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shellTokenize(tt.command); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("shellTokenize(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
	if reason, _ := evaluateDangerousCommand(`echo $'\'a' ; rm -rf / ; echo \'`, 0, noTownSession); reason == "" {
		t.Error("rm -rf / behind an ANSI-C string with an escaped quote is not blocked")
	}
	if reason, _ := evaluateDangerousCommand(`echo $'rm -rf /'`, 0, noTownSession); reason != "" {
		t.Errorf("rm -rf / inside an ANSI-C string is only text, but was blocked (%q)", reason)
	}
}

// gt-javvg: bash deletes a backslash-newline before it reads the quote, so a
// "$" and its "'" with a continuation between them still open an ANSI-C string,
// where the \' does not end it. Read as a plain quote, the rm behind it was
// hidden inside what the guard took for quoted text.
func TestDangerousGuardANSICLineContinuation(t *testing.T) {
	t.Parallel()
	joined := "echo $\\\n'a\\'' ; rm -rf / ; echo \\'"
	if reason, _ := evaluateDangerousCommand(joined, 0, noTownSession); !strings.Contains(reason, "filesystem destruction") {
		t.Errorf("evaluateDangerousCommand(%q) reason = %q, want filesystem destruction", joined, reason)
	}
	plain := "echo $'a\\'' ; rm -rf / ; echo \\'"
	if reason, _ := evaluateDangerousCommand(plain, 0, noTownSession); !strings.Contains(reason, "filesystem destruction") {
		t.Errorf("evaluateDangerousCommand(%q) reason = %q, want filesystem destruction", plain, reason)
	}
	text := "echo $\\\n'rm -rf /'"
	if reason, _ := evaluateDangerousCommand(text, 0, noTownSession); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the rm is text inside the string", text, reason)
	}
}

// gt-0vbb2: the heredoc scanners open and close $'...' strings where bash does,
// and read "$$" as the PID, so a "<<" inside an ANSI-C string is text and not a
// heredoc, and the operator line ends at its own newline.
func TestHeredocANSICQuotesFollowBash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		// The "<<" is text inside the string, so the rm is a live command.
		{"a << inside an ANSI-C string is not a heredoc", "echo $'a\\'\n<<EOF\n'\nrm -rf /\nEOF", true},
		// gt-javvg: the continuation between $ and ' does not hide the string.
		{"a << inside an ANSI-C string opened across a continuation is not a heredoc", "echo $\\\n'a\\'\n<<EOF\n'\nrm -rf /\nEOF", true},
		// gt-9xutc: the operator line's own ANSI-C string opens across a
		// continuation and closes before the newline, so the line ends there
		// and the rm is body text, read as data. Read as a plain quote, the
		// string ran on past the newline and hid the body's end.
		{"an ANSI-C string on the operator line opened across a continuation ends the line", "cat <<EOF $\\\n'a\\''\nrm -rf /\nEOF", false},
		{"the same string without the continuation ends the line", "cat <<EOF $'a\\''\nrm -rf /\nEOF", false},
		// "$$" is the PID, so 'a\' is a plain quote and <<EOF is a redirection
		// whose body is data.
		{"the quote after $$ is plain, so the walk reads the heredoc", "cat $$'a\\' <<EOF\nrm -rf /\nEOF", false},
		{"the quote after $$ ends the operator line at its newline", "cat <<EOF $$'a\\'\nrm -rf /\nEOF", false},
		{"a live command below the terminator is judged", "cat <<EOF $$'a\\'\necho hi\nEOF\nrm -rf /", true},
		{"a hash after a space inside an ANSI-C string is not a comment", "echo $'a #b'\ncat <<EOF\nrm -rf /\nEOF", false},
		{"a hash after a space inside a plain quote is not a comment", "echo 'a #b'\ncat <<EOF\nrm -rf /\nEOF", false},
		{"a hash after a space inside a double quote is not a comment", "echo \"a #b\"\ncat <<EOF\nrm -rf /\nEOF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if tt.blocked && reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked — the rm is a live command", tt.command)
			}
			if !tt.blocked && reason != "" {
				t.Errorf("evaluateDangerousCommand(%q) blocked (reason=%q), want allowed — the body is data", tt.command, reason)
			}
		})
	}
}

// gt-0vbb2: a "$'" opens an ANSI-C string only when the "$" is one the shell
// reads as its own. An escaped "$" is literal text, and the second "$" of a PID
// "$$" is its tail, so the quote after it is plain.
func TestIsANSICDollar(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		text string
		i    int
		want bool
	}{
		{"the quote opens a string at the start", "$'a'", 0, true},
		{"a dollar with no quote after it opens nothing", "$a", 0, false},
		{"a quote with no dollar before it opens nothing", "'a'", 0, false},
		{"the dollar is the last rune", "a$", 1, false},
		{"an index past the end opens nothing", "$'", 5, false},
		{"a negative index opens nothing", "$'", -1, false},
		{"an escaped dollar is literal", `\$'`, 1, false},
		{"an escaped backslash leaves the dollar live", `\\$'`, 2, true},
		{"the second dollar of PID is a tail", "$$'", 1, false},
		{"a third dollar after PID is live", "$$$'", 2, true},
		{"the fourth dollar is the tail of a second PID", "$$$$'", 3, false},
		{"an escaped first dollar leaves the second live", `\$$'`, 2, true},
		{"a continuation between the dollar and the quote opens a string", "$\\\n'a'", 0, true},
		{"a continuation then a plain letter opens nothing", "$\\\na", 0, false},
		{"an escaped backslash before a newline is not a continuation", "$\\\\\n'a'", 0, false},
		{"a PID tail across a continuation is still a tail", "$$\\\n'a'", 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isANSICDollar([]rune(tt.text), tt.i); got != tt.want {
				t.Errorf("isANSICDollar(%q, %d) = %v, want %v", tt.text, tt.i, got, tt.want)
			}
		})
	}
}

// gt-0vbb2: a "$" is the second half of a PID "$$" when the "$" before it is
// unescaped and begins that pair itself. The "$"s of a run alternate, so a long
// run is judged by the parity of its position.
func TestIsPIDTail(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		text string
		i    int
		want bool
	}{
		{"the first dollar is never a tail", "$$", 0, false},
		{"a dollar after a word is never a tail", "a$", 1, false},
		{"the second dollar of PID is a tail", "$$", 1, true},
		{"a third dollar after PID is not a tail", "$$$", 2, false},
		{"a fourth dollar after PID is a tail", "$$$$", 3, true},
		{"an escaped first dollar leaves the second one live", `\$$`, 2, false},
		{"an escaped first dollar makes the third one a tail", `\$$$`, 3, true},
		{"a tail after a word", "a$$", 2, true},
		{"an even place in a long run is not a tail", strings.Repeat("$", 4001), 4000, false},
		{"an odd place in a long run is a tail", strings.Repeat("$", 4001), 3999, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPIDTail([]rune(tt.text), tt.i); got != tt.want {
				t.Errorf("isPIDTail(%q, %d) = %v, want %v", tt.text, tt.i, got, tt.want)
			}
		})
	}
}

// gt-48buu: a launcher's own flags (env -i, nice -n, time -p) sit between the
// launcher and the shell, so the shell is still the command word and its
// heredoc body is judged as a script rather than stripped as data.
func TestHeredocFedToShellBehindLauncherFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
	}{
		{"env with a flag", "env -i bash <<EOF\ngit reset --hard\nEOF"},
		{"nice with a value flag", "nice -n 5 bash <<EOF\ngit reset --hard\nEOF"},
		{"time with a flag", "time -p bash <<EOF\ngit reset --hard\nEOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason != gitResetHardReason {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want %q", tt.command, reason, gitResetHardReason)
			}
		})
	}
}

// endsCommandReservedWord decides whether a "((" glued to a word opens an
// arithmetic command. A word that merely ends in a reserved word, or sits
// inside a larger word, is not one (gt-he2fi).
func TestEndsCommandReservedWord(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		text    string
		escaped bool
		want    bool
	}{
		{"then at the start", "then", false, true},
		{"do after a semicolon", "a; do", false, true},
		{"else after a newline", "x\nelse", false, true},
		{"for glued to the header", "for", false, true},
		{"time after a newline", "x\ntime", false, true},
		{"negation at the start", "!", false, true},
		{"negation after a semicolon", "a; !", false, true},
		{"brace group opener after a semicolon", "a; {", false, true},
		{"case pattern close", "y)", false, true},
		{"negation inside a longer word", "a!", false, false},
		{"brace inside a longer word", "a{", false, false},
		{"time inside a longer word", "runtime", false, false},
		{"escaped negation", `\!`, false, false},
		{"escaped close paren", ")", true, false},
		{"reserved word after an open paren", "(do", false, true},
		{"word ending in a reserved word", "undo", false, false},
		{"reserved word inside a longer one", "dothen", false, false},
		{"escaped first character", `\do`, false, false},
		{"parameter expansion of the name", "$do", false, false},
		{"upper case is not reserved", "Then", false, false},
		{"a word that is not reserved", "echo", false, false},
		{"last rune was escaped", "then", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runes := []rune(tt.text)
			if got := endsCommandReservedWord(runes, len(runes)-1, tt.escaped); got != tt.want {
				t.Errorf("endsCommandReservedWord(%q, escaped=%v) = %v, want %v", tt.text, tt.escaped, got, tt.want)
			}
		})
	}
	if endsCommandReservedWord(nil, -1, false) {
		t.Error("endsCommandReservedWord at the start of the command = true, want false")
	}
}

// An escaped backtick pair inside a backtick span is literal to the outer scan
// but live to the body's own parse, so the command it holds is judged wherever
// the shell would run it — on a command line and in an unquoted heredoc body
// alike (gt-h3312). The negative pairs pin the other side of the cut: a body
// holding no dangerous command is allowed, a quoted heredoc body is data for its
// reader, and a backtick pair that is itself escaped opens no substitution.
func TestDangerousGuardJudgesEscapedNestedBacktick(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"a nuke in the nested command", "echo `echo \\`rm -rf /\\``", true},
		{"a reset onto the remote in the nested command", "echo `echo \\`git reset --hard origin/main\\``", true},
		{"the nuke in an unquoted heredoc body", "cat > f <<EOF\necho `echo \\`rm -rf /\\``\nEOF", true},
		{"a reset in an escaped-dollar substitution", "echo `echo \\$(git reset --hard)`", true},
		{"a harmless escaped-dollar substitution", "echo `echo \\$(echo hi)`", false},
		{"a harmless nested command", "echo `echo \\`echo hi\\``", false},
		{"the same body quoted stays data", "cat > f <<'EOF'\necho `echo \\`rm -rf /\\``\nEOF", false},
		{"an escaped backtick pair opens no substitution", "echo \\`rm -rf /\\`", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
		})
	}
}

// gt-5wtmz: a ")" inside a "#" comment is comment text that closes no "$(",
// and a "#" after a ")" that ends a word part ("$(date)#x") opens no comment.
func TestDangerousGuardParenInCommentDoesNotEndSubstitution(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"a reset after a comment holding a closing paren", "x=$(\n# a) b\ngit reset --hard)", true},
		{"a nuke after a comment holding a closing paren", "out=$( # note )\nrm -rf /)", true},
		{"a force push whose flag is glued to the closing paren", "x=$(echo ok # )\ngit push --force)", true},
		{"a process substitution after a substitution close", "cat $(date)#<(git reset --hard)", true},
		{"a process substitution after an arithmetic close", "cat $((1))#<(git reset --hard)", true},
		{"a process substitution after a quoted paren close", `cat $(")")#<(git reset --hard)`, true},
		{"a substitution the quotes after a word part keep literal", "echo $(date)#'x $(git reset --hard)'", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
		})
	}
}

// gt-h45w3: a quoted "$(" makes shlex fail, so the naive split reads the
// command, and the quote characters it keeps hid a sudo body and an
// assignment's "$(". A quoted ")" inside an unquoted substitution must not
// end it early.
func TestDangerousGuardQuotedParenShapes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"a sudo body behind a double-quoted dollar-paren", `echo "$(" bash -c 'sudo apt-get install foo'`, true},
		{"a sudo body behind a double-quoted dollar-paren and text", `echo "$(" text bash -c 'sudo apt-get install foo'`, true},
		{"a sudo body behind a dollar-paren with text in its quotes", `"$(x" text bash -c "sudo apt-get install foo"`, true},
		{"a sudo body behind an assignment of a double-quoted dollar-paren", `x="$(" bash -c 'sudo apt-get install foo'`, true},
		{"a nuke behind an assignment of a double-quoted dollar-paren", `x="$(" bash -c "rm -rf /"`, true},
		{"a nuke after a quoted close-paren in a substitution", `echo $(echo ")" && rm -rf /)`, true},
		{"a sudo body after a quoted close-paren in a substitution", `x=$(echo ")" && bash -c 'sudo apt-get install foo')`, true},
		{"a sudo body after a quoted close-paren and a semicolon", `echo $(echo ")" ; bash -c 'sudo apt-get install foo')`, true},
		{"a nuke after a double-quoted dollar-paren in a substitution", `echo $(echo "$(" ; rm -rf /)`, true},
		{"a nuke after a double-quoted dollar-paren and a nested group", `echo $(echo "$(" ; (echo ok) ; rm -rf /)`, true},
		{"a quoted dollar-paren and a nested group with harmless commands", `echo $(echo "$(" ; (echo ok) ; echo hi)`, false},
		{"a sudo body behind text then a quote that ends the dollar-paren", `echo "a$(" bash -c 'sudo apt-get install foo'`, true},
		{"a sudo body behind a single-quoted dollar-paren and text", `echo '$(' text bash -c 'sudo apt-get install foo'`, true},
		{"a sudo body behind a dollar-paren and an escaped quote", `echo "\"$(" bash -c 'sudo apt-get install foo'`, true},
		{"a sudo body behind a quoted close-paren with an apostrophe inside", `echo "it's)" "$(" bash -c 'sudo apt-get install foo'`, true},
		{"a double-quoted dollar-paren with no reader after it", `echo "$(" ; echo ok`, false},
		{"a single-quoted dollar-paren with no reader after it", `echo '$(' bash -c 'echo hi'`, false},
		{"a double-quoted dollar-paren with a harmless shell body", `echo "$(" text bash -c "echo hi"`, false},
		{"a harmless body behind an assignment of a double-quoted dollar-paren", `x="$(" bash -c 'echo hi'`, false},
		{"a quoted close-paren in a substitution with harmless commands", `echo $(echo ")" && echo ok)`, false},
		{"a quoted close-paren in a substitution with a harmless shell body", `x=$(echo ")" ; bash -c 'echo hi')`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
		})
	}
}

// gt-w3j7z: shlex fails on the operator-spaced text of a command that holds a
// quote and a paren, so splitSpacedCommand falls back to lenientShellWords.
// Reading the RAW command, that fallback kept a newline (or a glued ;, &&, |)
// inside the enclosing word instead of as the separator spaceOutShellOperators
// made it, so the rm on a later line sat mid-segment as an argument of the
// previous line's command. bash runs those lines before it reports the syntax
// error on the last one.
func TestDangerousGuardLenientFallbackKeepsSeparators(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"a double-quoted paren on the last line", "echo ok\nrm -rf /\necho \"(", true},
		{"a single-quoted paren on the last line", "echo ok\nrm -rf /\necho '(", true},
		{"a harmless last line behind an unclosed quote and a paren", "echo ok\nrm -rf /\necho \"(x", true},
		{"a semicolon glued to the earlier line", "echo ok; rm -rf /; echo \"(", true},
		{"a pipe glued on both sides", "echo ok|rm -rf /|echo \"(", true},
		{"a doubled ampersand glued on both sides", "echo ok&&rm -rf /&&echo \"(", true},
		{"a single ampersand glued on both sides", "echo ok&rm -rf /&echo \"(", true},
		{"a doubled pipe glued on both sides", "echo ok||rm -rf /||echo \"(", true},
		{"an ampersand redirection keeps the nuke an argument", "echo ok >&rm -rf / \"(", false},
		{"the nuke as an argument of the line before", "echo ok\necho rm -rf /\necho \"(", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
			if tt.blocked && !strings.Contains(reason, "filesystem destruction") {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want filesystem destruction", tt.command, reason)
			}
		})
	}
}

// gt-w9eli: a command holding a quote but no paren takes the strings.Fields
// fallback in splitSpacedCommand. It split the raw text, so the newlines and
// ";" that spaceOutShellOperators turns into separators stayed inside their
// words, and a later line's rm sat mid-segment as an argument of the line
// before it. bash runs each line before it reports the unclosed quote.
func TestDangerousGuardFieldsFallbackKeepsSeparators(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"an unclosed double quote on the last line", "echo ok\nrm -rf /\necho \"", true},
		{"an unclosed double quote with text on the last line", "echo ok\nrm -rf /\necho \"x", true},
		{"a harmless last line behind a closed quote and an unclosed one", "echo ok\nrm -rf /\necho \"x\" \"y", true},
		{"a semicolon glued to the earlier line", "echo ok;rm -rf /;echo \"", true},
		{"a pipe glued on both sides", "echo ok|rm -rf /|echo \"", true},
		{"a doubled ampersand glued on both sides", "echo ok&&rm -rf /&&echo \"", true},
		{"a single ampersand glued on both sides", "echo ok&rm -rf /&echo \"", true},
		{"a doubled pipe glued on both sides", "echo ok||rm -rf /||echo \"", true},
		{"a semicolon glued before an unclosed single quote", "echo ok;rm -rf /;echo 'x", true},
		{"a nuke inside a quote glued to a separator stays text", "echo \"a;rm -rf /\" '", false},
		{"a harmless malformed command with glued separators stays allowed", "echo ok;ls;echo 'x", false},
		{"the nuke as an argument of the line before", "echo ok\necho rm -rf /\necho \"", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession)
			if (reason != "") != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked = %v (reason %q), want %v", tt.command, reason != "", reason, tt.blocked)
			}
			if tt.blocked && !strings.Contains(reason, "filesystem destruction") {
				t.Errorf("evaluateDangerousCommand(%q) reason = %q, want filesystem destruction", tt.command, reason)
			}
		})
	}
}

// TestQuotedDollarParenPadsOnlyItsSubstitutionsCloser pins the reach of the
// ")" padding: a substitution whose word was ended by a quoted dollar-paren
// has its OWN ")" spaced out, so the word glued before it ("/" in
// "rm -rf /)") is a token of its own; a ")" nested inside that substitution
// closes a group and is emitted exactly as in any other substitution body,
// staying glued to the word before it ("ok)" below). Without the second half
// the padding would rewrite every ")" in the scope, not just the one the
// gt-qg832 shape hid (gt-qg832).
func TestQuotedDollarParenPadsOnlyItsSubstitutionsCloser(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "the closer is padded so the glued word stands alone",
			command: `echo $(echo "$(" ; rm -rf /)`,
			want:    []string{"echo", "$(echo", "$(", ";", "rm", "-rf", "/", ")"},
		},
		{
			name:    "a nested group's closer stays glued to its word",
			command: `echo $(echo "$(" ; (echo ok); rm -rf /)`,
			want:    []string{"echo", "$(echo", "$(", ";", "(echo", "ok)", ";", "rm", "-rf", "/", ")"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellTokenize(tt.command)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("shellTokenize(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

// TestDangerousGuardClobberAndFdVarRedirections pins gt-l18zp: >|file,
// 2>|file and {fd}>file are redirections, so the shell invoker behind them is
// still the command that reads the heredoc. The operator-spacing pass split
// the "|" out and the scan read it as the redirect's target, leaving the
// target word as the command; "{fd}>" lost its brace to the group-opener trim.
// Either way the reader went unseen and the body behind 2>/dev/null was
// blocked while these were not.
func TestDangerousGuardClobberAndFdVarRedirections(t *testing.T) {
	t.Parallel()
	body := " bash <<EOF\nrm -rf /\nEOF"
	for _, tt := range []struct {
		name    string
		command string
	}{
		{"clobber", ">|/dev/null" + body},
		{"fd clobber", "2>|/dev/null" + body},
		{"braced fd", "{fd}>/dev/null" + body},
		{"clobber with a separate target", ">| /dev/null" + body},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if reason, _ := evaluateDangerousCommand(tt.command, 0, noTownSession); reason == "" {
				t.Errorf("evaluateDangerousCommand(%q) allowed a shell fed rm -rf / behind a redirection", tt.command)
			}
		})
	}
}

// TestDangerousGuardClosedQuotedSubstitutionArgIsData pins gt-l18zp: in
// x="$("echo" "a; git push -f origin main")" the quoted argument is one
// argument to echo, and the shell prints it as one line. The quote-tracking
// read the substitution flat, so the ";" inside that argument became a
// separator and the words after it read as a live force push.
func TestDangerousGuardClosedQuotedSubstitutionArgIsData(t *testing.T) {
	t.Parallel()
	command := `x="$("echo" "a; git push -f origin main")"`
	if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
		t.Errorf("evaluateDangerousCommand(%q) = %q, want allowed: the argument is data (bash prints it as one line)", command, reason)
	}
}

// gt-h45w3: lenientShellWords reads quotes the way shlex does and, unlike it,
// never fails on a quote left open.
func TestLenientShellWords(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		command string
		want    []string
	}{
		{"plain words", `a  b	c`, []string{"a", "b", "c"}},
		{"double quotes group a word", `echo "a b" c`, []string{"echo", "a b", "c"}},
		{"single quotes keep a backslash", `echo 'a\b' c`, []string{"echo", `a\b`, "c"}},
		{"an escaped double quote stays in a double-quoted word", `echo "a\"b" c`, []string{"echo", `a"b`, "c"}},
		{"a backslash before an ordinary rune stays in double quotes", `echo "a\nb"`, []string{"echo", `a\nb`}},
		{"an apostrophe in double quotes is text", `echo "it's" x`, []string{"echo", "it's", "x"}},
		{"an escaped space joins a word", `echo a\ b`, []string{"echo", "a b"}},
		{"empty quotes are an empty word", `a "" b`, []string{"a", "", "b"}},
		{"adjacent quoted parts are one word", `x="$("y`, []string{"x=$(y"}},
		{"an open double quote runs to the end", `echo "$( bash -c`, []string{"echo", "$( bash -c"}},
		{"an open single quote runs to the end", `echo 'a b`, []string{"echo", "a b"}},
		{"a trailing backslash is kept", `echo a\`, []string{"echo", `a\`}},
		{"empty command", ``, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := lenientShellWords(tt.command); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("lenientShellWords(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}
