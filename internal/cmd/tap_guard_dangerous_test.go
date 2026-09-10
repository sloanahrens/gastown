package cmd

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hooks"
)

func TestExtractCommand(t *testing.T) {
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

func TestMatchesSudo(t *testing.T) {
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
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"bash -c wrapped git reset --hard", `bash -c "git reset --hard"`, true},
		{"sh -c wrapped sudo", `sh -c "sudo rm -rf /"`, true},
		{"eval wrapped git clean -fd", `eval "git clean -fd"`, true},
		{"bash -c safe command", `bash -c "echo hello"`, false},
		{"nested but safe", `sh -c "ls -la"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

// TestQuotedDDLPayload pins the fix for SQL DDL patterns that are, in
// practice, always passed quoted (dolt sql -q "...", psql -c "...", mysql
// -e '...'), which shlex collapses into one multi-word token — invisible to
// the old exact-token fragment match (finding 4, gt-wisp-db27).
func TestQuotedDDLPayload(t *testing.T) {
	tests := []struct {
		name    string
		command string
		blocked bool
	}{
		{"dolt sql drop table", `dolt sql -q "DROP TABLE issues"`, true},
		{"psql truncate table", `psql -c "TRUNCATE TABLE users"`, true},
		{"mysql drop database", `mysql -e "DROP DATABASE prod"`, true},
		{"quoted but safe select", `dolt sql -q "SELECT * FROM issues"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, _ := evaluateDangerousCommand(tt.command, 0)
			got := reason != ""
			if got != tt.blocked {
				t.Errorf("evaluateDangerousCommand(%q) blocked=%v (reason=%q), want %v", tt.command, got, reason, tt.blocked)
			}
		})
	}
}

func TestMatchesUnboundedScan(t *testing.T) {
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
		{"find /Users/sloan", "find /Users/sloan -name x", false},
		{"grep without -r", "grep TODO file.go", false},
		{"ls -r reverse sort, not recursive", "ls -r /", false},
		{"ls plain", "ls /", false},
		{"du without root arg", "du -sh .", false},
		{"no scan tool at all", "echo hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, alternative := matchesUnboundedScan(shellTokenize(tt.command))
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

// TestGtMkrjRegressions pins the five exact command texts that false-fired
// dangerous-command before gt-mkrj: substring matching ("apt" inside
// "capture"/"adapt") and quote-unaware tokenization (a "//" jq/sed operator
// inside a quoted script read as a bare root-path argument) both fired on
// text that was never a real standalone shell argument. All five must now
// pass (no dangerous-command block) once matching is shell-aware and
// token-exact. See mail hq-wisp-io521 for the original reports.
func TestGtMkrjRegressions(t *testing.T) {
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
			if reason, _ := matchesUnboundedScan(tokens); reason != "" {
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

// TestDangerousGuard_Integration tests the full pattern set end-to-end.
func TestDangerousGuard_Integration(t *testing.T) {
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
		{"git clean -f", "git clean -f", true},
		{"git clean -fd", "git clean -fd", true},
		{"drop table", "DROP TABLE users", true},

		// Allowed
		{"rm -rf ./build/", "rm -rf ./build/", false},
		{"rm -rf /tmp/cache/", "rm -rf /tmp/cache/", false},
		{"git push --force-with-lease", "git push --force-with-lease origin main", false},
		{"git push normal", "git push origin main", false},
		{"git reset soft", "git reset --soft HEAD~1", false},
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

// dangerousCommandGuardMatcher reports whether the given command is routed
// by hooks.DefaultBase()'s PreToolUse config to the dangerous-command guard
// specifically, using Claude Code's real dispatch semantics: the entry's
// Matcher must be the bare "Bash" tool name — a pattern written into Matcher
// never fires (gt-5ihs) — and each hook's If (if any) gates it further.
func dangerousCommandGuardMatcher(command string) bool {
	for _, entry := range hooks.DefaultBase().PreToolUse {
		if entry.Matcher != "Bash" {
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
				if reason, _ := matchesUnboundedScan(shellTokenize(command)); reason != "" {
					t.Fatalf("command %q reached the dangerous-command guard and was blocked: %q", command, reason)
				}
				if matchesDangerousRmRf(lower) != "" || matchesDangerousGitPush(lower) != "" {
					t.Fatalf("command %q reached the dangerous-command guard and was blocked", command)
				}
			}
		})
	}
}
