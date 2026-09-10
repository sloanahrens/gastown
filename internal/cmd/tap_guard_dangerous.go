package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardDangerousCmd = &cobra.Command{
	Use:   "dangerous-command",
	Short: "Block dangerous commands (sudo, package installs, rm -rf, force push, etc.)",
	Long: `Block dangerous commands via Claude Code PreToolUse hooks.

This guard blocks operations that could cause irreversible damage:
  - sudo <anything>      (agents must never elevate privileges)
  - apt/apt-get/dnf/yum/pacman install (system package managers)
  - brew install          (Homebrew package installs)
  - pip install --system  (system-level Python installs)
  - npm install -g        (global npm installs)
  - gem install           (system-level Ruby installs)
  - rm -rf /             (only blocks root target; rm -rf ./build/ is allowed)
  - git push --force/-f  (--force-with-lease is allowed)
  - git reset --hard
  - git clean -f / git clean -fd
  - drop table/database
  - truncate table
  - find/bfs/fd/rg/grep -r/du/ls -R rooted at /, ~, $HOME, /Users, /System,
    /Library, or /opt (see gt-nqcy — an unbounded 'bfs /' froze a host)

The guard reads the tool input from stdin (Claude Code hook protocol)
and exits with code 2 to block dangerous operations.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardDangerous,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardDangerousCmd)
}

// dangerousPattern defines a pattern to match and its human-readable reason.
// All substrings must appear in the command (simple containment check).
// For patterns that need smarter matching (rm -rf, git push --force),
// use the dedicated match functions instead.
type dangerousPattern struct {
	contains []string
	reason   string
}

// fragmentPatterns use simple containment matching (all substrings must appear).
var fragmentPatterns = []dangerousPattern{
	{[]string{"git", "reset", "--hard"}, "Hard reset discards all uncommitted changes irreversibly"},
	{[]string{"git", "clean", "-f"}, "git clean -f deletes untracked files irreversibly"},
	{[]string{"drop", "table"}, "database table destruction"},
	{[]string{"drop", "database"}, "database destruction"},
	{[]string{"truncate", "table"}, "database table truncation"},
}

// safeForceFlags are git push flags that look like --force but are safe.
var safeForceFlags = []string{"--force-with-lease", "--force-if-includes"}

func runTapGuardDangerous(cmd *cobra.Command, args []string) error {
	// Read hook input from stdin (Claude Code protocol)
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}

	command := extractCommand(input)
	if command == "" {
		return nil
	}

	lower := strings.ToLower(command)

	// Check privilege escalation and package manager commands first
	if reason := matchesSudo(lower); reason != "" {
		printDangerousBlock(reason, command)
		return NewSilentExit(2)
	}
	if reason := matchesPackageInstall(lower); reason != "" {
		printDangerousBlock(reason, command)
		return NewSilentExit(2)
	}

	// Check special patterns that need smarter matching
	if reason := matchesDangerousRmRf(lower); reason != "" {
		printDangerousBlock(reason, command)
		return NewSilentExit(2)
	}
	if reason := matchesDangerousGitPush(lower); reason != "" {
		printDangerousBlock(reason, command)
		return NewSilentExit(2)
	}
	// Unbounded scans need the original (non-lowercased) command: ls -R
	// (recursive) and ls -r (reverse sort) mean different things, and
	// lowercasing would collapse that distinction.
	if reason, alternative := matchesUnboundedScan(command); reason != "" {
		printDangerousBlockWithAlternative(reason, command, alternative)
		return NewSilentExit(2)
	}

	// Check simple fragment patterns
	for _, pattern := range fragmentPatterns {
		if matchesAllFragments(lower, pattern.contains) {
			printDangerousBlock(pattern.reason, command)
			return NewSilentExit(2)
		}
	}

	return nil
}

// printDangerousBlock prints the standard block banner to stderr.
func printDangerousBlock(reason, originalCommand string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ DANGEROUS COMMAND BLOCKED                                    ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Command: %-53s ║\n", truncateStr(originalCommand, 53))
	fmt.Fprintf(os.Stderr, "║  Reason:  %-53s ║\n", truncateStr(reason, 53))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  If this is intentional, ask the user to run it manually.        ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}

// printDangerousBlockWithAlternative is printDangerousBlock plus an
// unabbreviated suggestion line printed below the fixed-width box, so the
// alternative isn't lost to truncateStr's 53-char box limit.
func printDangerousBlockWithAlternative(reason, originalCommand, alternative string) {
	printDangerousBlock(reason, originalCommand)
	if alternative != "" {
		fmt.Fprintln(os.Stderr, "  "+alternative)
		fmt.Fprintln(os.Stderr, "")
	}
}

// scanRootDenylist lists filesystem roots broad enough that a recursive
// scan from them can run for minutes at high CPU and peg the host — see
// gt-nqcy: 'bfs -S dfs / -name regex.h' ran 8m21s at 517% peak CPU and
// froze the operator's terminal. A deeper path under these roots (e.g.
// /opt/homebrew, /Users/me/project) is unaffected — only an exact root
// token is denied.
var scanRootDenylist = map[string]bool{
	"/": true, "/*": true, "~": true, "$home": true,
	"/users": true, "/system": true, "/library": true, "/opt": true,
}

func isUnboundedScanRoot(token string) bool {
	return scanRootDenylist[strings.ToLower(token)]
}

// alwaysRecursiveScanTools walk a directory tree (or the whole index, for
// rg/ag) on every ordinary invocation — there's no non-recursive mode to
// distinguish, so any bare root argument is enough to flag them.
var alwaysRecursiveScanTools = map[string]bool{
	"find": true, "bfs": true, "fd": true, "du": true, "rg": true, "ag": true,
}

// matchesUnboundedScan blocks find/bfs/fd/rg/ag/du, "grep -r", and "ls -R"
// invocations whose root argument is broad enough to scan the whole
// filesystem (see scanRootDenylist). It returns a short reason for the
// fixed-width block banner and a longer alternative-tools suggestion to
// print separately, or ("", "") if the command is fine.
func matchesUnboundedScan(command string) (reason, alternative string) {
	fields := strings.Fields(command)
	for i, f := range fields {
		base := strings.ToLower(f)
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}

		rest := fields[i+1:]
		isScan := alwaysRecursiveScanTools[base]
		if !isScan {
			switch base {
			case "grep":
				isScan = hasExactArg(rest, "-r", "-R", "--recursive")
			case "ls":
				isScan = hasShortFlagLetter(rest, 'R')
			}
		}
		if !isScan {
			continue
		}

		for _, arg := range rest {
			if !isUnboundedScanRoot(arg) {
				continue
			}
			reason = fmt.Sprintf("Unbounded scan (%s rooted at %s)", base, arg)
			alternative = "Alternative: brew --prefix, pkg-config, 'go env GOROOT'/'go env GOMODCACHE', " +
				"or a search rooted inside the repo/rig instead of the whole filesystem."
			return reason, alternative
		}
	}
	return "", ""
}

// hasExactArg reports whether any of args exactly matches one of the wanted
// values.
func hasExactArg(args []string, wanted ...string) bool {
	for _, a := range args {
		for _, w := range wanted {
			if a == w {
				return true
			}
		}
	}
	return false
}

// hasShortFlagLetter reports whether any bundled short option (e.g. "-lR")
// among args contains the given letter, matched case-sensitively — ls's
// "-R" (recursive) and "-r" (reverse sort) mean different things.
func hasShortFlagLetter(args []string, letter rune) bool {
	for _, a := range args {
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		if strings.ContainsRune(a, letter) {
			return true
		}
	}
	return false
}

// extractCommand extracts the bash command from Claude Code hook input JSON.
func extractCommand(input []byte) string {
	if len(input) == 0 {
		return ""
	}
	var hookInput struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(input, &hookInput); err != nil {
		return ""
	}
	return hookInput.ToolInput.Command
}

// matchesAllFragments returns true if all fragments appear in the command.
func matchesAllFragments(command string, fragments []string) bool {
	for _, f := range fragments {
		if !strings.Contains(command, strings.ToLower(f)) {
			return false
		}
	}
	return true
}

// matchesDangerousRmRf blocks "rm -rf /" targeting the root filesystem.
// Only blocks when the target is literally "/" or "/*". Normal cleanup
// commands like "rm -rf ./build/" are allowed.
func matchesDangerousRmRf(command string) string {
	if !strings.Contains(command, "rm") {
		return ""
	}
	fields := strings.Fields(command)
	hasRm := false
	hasRecursiveForce := false
	for _, f := range fields {
		if f == "rm" {
			hasRm = true
		}
		if strings.HasPrefix(f, "-") && strings.Contains(f, "r") && strings.Contains(f, "f") {
			hasRecursiveForce = true
		}
		if hasRm && hasRecursiveForce && (f == "/" || f == "/*") {
			return "filesystem destruction (rm -rf /)"
		}
	}
	return ""
}

// matchesSudo blocks any command that starts with or contains "sudo".
// Agents must never elevate privileges on the host system.
func matchesSudo(command string) string {
	fields := strings.Fields(command)
	for _, f := range fields {
		if f == "sudo" {
			return "Agents must never use sudo — do not elevate privileges or modify the host OS"
		}
	}
	return ""
}

// packageManagerPatterns lists system package manager install commands.
// Each entry has the command prefix tokens and a reason.
var packageManagerPatterns = []struct {
	tokens []string
	reason string
}{
	{[]string{"apt", "install"}, "System package install (apt) — use workspace tools instead"},
	{[]string{"apt-get", "install"}, "System package install (apt-get) — use workspace tools instead"},
	{[]string{"dnf", "install"}, "System package install (dnf) — use workspace tools instead"},
	{[]string{"yum", "install"}, "System package install (yum) — use workspace tools instead"},
	{[]string{"pacman", "-s"}, "System package install (pacman) — use workspace tools instead"},
	{[]string{"brew", "install"}, "Package install (brew) — use workspace tools instead"},
	{[]string{"gem", "install"}, "System gem install — use workspace tools instead"},
}

// matchesPackageInstall blocks system package manager install commands.
// Also blocks "pip install" with --system flag and "npm install -g" (global installs).
func matchesPackageInstall(command string) string {
	// Check simple token-based patterns (apt install, dnf install, etc.)
	for _, p := range packageManagerPatterns {
		if matchesAllFragments(command, p.tokens) {
			return p.reason
		}
	}

	// pip install --system (but not regular pip install into a venv)
	if strings.Contains(command, "pip") && strings.Contains(command, "install") && strings.Contains(command, "--system") {
		return "System-level pip install — use a virtualenv or workspace tools instead"
	}

	// npm install -g / npm install --global
	if strings.Contains(command, "npm") && strings.Contains(command, "install") {
		if strings.Contains(command, " -g ") || strings.Contains(command, " -g") || strings.Contains(command, "--global") {
			return "Global npm install — use workspace tools instead"
		}
	}

	return ""
}

// matchesDangerousGitPush blocks "git push --force" while allowing safe
// variants like "--force-with-lease" and "--force-if-includes".
func matchesDangerousGitPush(command string) string {
	if !strings.Contains(command, "git") || !strings.Contains(command, "push") {
		return ""
	}
	fields := strings.Fields(command)
	hasPush := false
	for i, f := range fields {
		if f == "push" && i > 0 && fields[i-1] == "git" {
			hasPush = true
			continue
		}
		if !hasPush {
			continue
		}
		if f == "--force" || f == "-f" {
			return "Force push rewrites remote history and can destroy others' work"
		}
		// Skip safe force variants (don't accidentally match their substrings)
		for _, safe := range safeForceFlags {
			if f == safe {
				break
			}
		}
	}
	return ""
}
