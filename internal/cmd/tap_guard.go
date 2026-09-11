package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Block forbidden operations (PreToolUse hook)",
	Long: `Block forbidden operations via Claude Code PreToolUse hooks.

Guard commands exit with code 2 to BLOCK tool execution when a policy
is violated. They're called before the tool runs, preventing the
forbidden operation entirely.

Available guards:
  pr-workflow        - Block PR creation and feature branches
  bd-init            - Block bd init in wrong directories
  mol-patrol         - Block mol patrol from agent contexts
  dangerous-command  - Block rm -rf, force push, hard reset, git clean
  formula-allowlist  - Constrain dog sessions to their formula's declared commands

External guards (standalone scripts, not compiled into gt):
  context-budget   - scripts/guards/context-budget-guard.sh

Example hook configuration (matcher is the TOOL NAME only; a command
pattern like "Bash(gh pr create*)" goes in "if", never in "matcher" —
gt-5ihs, a matcher-only pattern never fires):
  {
    "PreToolUse": [{
      "matcher": "Bash",
      "hooks": [{"command": "gt tap guard pr-workflow", "if": "Bash(gh pr create*)"}]
    }]
  }`,
}

var tapGuardPRWorkflowCmd = &cobra.Command{
	Use:   "pr-workflow",
	Short: "Block PR creation and feature branches",
	Long: `Block PR workflow operations in Gas Town.

Gas Town workers push directly to main. PRs add friction that breaks
the autonomous execution model (GUPP principle).

This guard blocks:
  - gh pr create
  - git checkout -b (feature branches)
  - git switch -c (feature branches)

Exit codes:
  0 - Operation allowed (not in Gas Town agent context, not maintainer origin)
  2 - Operation BLOCKED (in agent context OR maintainer origin)

The guard blocks in two scenarios:
  1. Running as a Gas Town agent (crew, polecat, witness, etc.)
  2. Origin remote is steveyegge/gastown (maintainer should push directly)

Humans running outside Gas Town with a fork origin can still use PRs.

This command expects the hook's tool_input JSON on stdin. Run manually on
a terminal with no input piped in and it will hang reading stdin until
EOF (Ctrl-D) — same as the dangerous-command guard.`,
	RunE: runTapGuardPRWorkflow,
}

func init() {
	tapCmd.AddCommand(tapGuardCmd)
	tapGuardCmd.AddCommand(tapGuardPRWorkflowCmd)
}

// prWorkflowCommandPrefixes are the exact command shapes this guard blocks,
// as whitespace-separated tokens, mirroring the "if" glob patterns in
// DefaultBase() (Bash(gh pr create*), Bash(git checkout -b*),
// Bash(git switch -c*)). Checking them here too, against tool_input.command
// read from stdin, means a hooks-sync regression that drops the If field
// (formerly the guard's only filter) degrades to "blocks nothing unrelated"
// instead of "blocks every Bash call" — the shape behind gt-pjeh's 12:04
// outage, where the guard blocked every Bash call in agent context because
// it never looked at the command at all.
var prWorkflowCommandPrefixes = [][]string{
	{"gh", "pr", "create"},
	{"git", "checkout", "-b"},
	{"git", "switch", "-c"},
}

// shellCommandSeparators are shell operators that start a new command
// within a single line. Claude Code evaluates Bash permission rules
// per sub-command, so its "if" glob can fire on a later segment of a
// compound command (cd x && gh pr create ...) — this guard must inspect
// each ;/&&/||/| segment independently rather than only the start of the
// whole line, or a leading unrelated segment lets a real PR-workflow
// command later on the line slip through (gt-wisp-52y4).
var shellCommandSeparators = map[string]bool{
	"&&": true,
	"||": true,
	";":  true,
	"|":  true,
}

// matchesPRWorkflowCommand reports whether command is one of the PR-workflow
// shapes this guard exists to block. It tokenizes the command (so a quoted
// argument like a PR body is never mistaken for a shell operator or a
// command word), splits it into per-segment sub-commands on shell operators,
// and compares whole tokens rather than raw string prefixes — so e.g.
// "git checkout -branch" (a different flag) does not falsely match
// "git checkout -b".
func matchesPRWorkflowCommand(command string) bool {
	tokens := shellTokenize(strings.TrimSpace(command))

	segmentMatches := func(segment []string) bool {
		for _, prefix := range prWorkflowCommandPrefixes {
			if len(segment) < len(prefix) {
				continue
			}
			match := true
			for i, want := range prefix {
				if segment[i] != want {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	}

	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if segmentMatches(segment) {
				return true
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return segmentMatches(segment)
}

func runTapGuardPRWorkflow(cmd *cobra.Command, args []string) error {
	// The refinery's mandated merge-rehearsal checkout (mol-refinery-patrol
	// step 1, mol-polecat-conflict-resolve: "git checkout -b temp
	// origin/<branch>") legitimately needs to create a branch — the same
	// shape of command isGasTownAgentContext() otherwise blocks for every
	// role. Exempt only that shape, and only for the refinery role: "gh pr
	// create" stays blocked for refineries same as everyone else (gt-r2xm).
	// All three "if"-matched hook patterns (gh pr create*, git checkout
	// -b*, git switch -c*) invoke this same command with no argument
	// telling us which one fired, so the actual command must be read back
	// off stdin (Claude Code hook protocol) to tell them apart. Read once
	// and share it with the self-filter below.
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil
	}
	command := extractCommand(input)
	if isRefineryRole() && isFeatureBranchCommand(command) && !isPRCreateCommand(command) {
		return nil
	}
	// Self-filter only when a command was actually extracted. Some harness
	// templates (e.g. Copilot's INPUT=$(cat) wrapper, gt-wisp-52y4) drain
	// stdin before invoking this guard, so empty/unparsable input here means
	// "we don't know what command this is", not "nothing to block" — fall
	// back to the unconditional context/origin check below (the guard's only
	// behavior before this self-filter existed) instead of failing open.
	if command != "" && !matchesPRWorkflowCommand(command) {
		return nil
	}

	// Check if we're in a Gas Town agent context
	if isGasTownAgentContext() {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ PR WORKFLOW BLOCKED                                          ║")
		fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  Gas Town workers push directly to main. PRs are forbidden.     ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr create / git checkout -b / git switch -c    ║")
		fmt.Fprintln(os.Stderr, "║  Do this:     git add . && git commit && git push origin main   ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Why? PRs add friction that breaks autonomous execution.        ║")
		fmt.Fprintln(os.Stderr, "║  See: ~/gt/docs/PRIMING.md (GUPP principle)                     ║")
		fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// Check if origin is the maintainer's repo (steveyegge/gastown)
	if isMaintainerOrigin() {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
		fmt.Fprintln(os.Stderr, "║  ❌ PR BLOCKED - MAINTAINER ORIGIN                               ║")
		fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
		fmt.Fprintln(os.Stderr, "║  Your origin is steveyegge/gastown - push directly to main.     ║")
		fmt.Fprintln(os.Stderr, "║  PRs are for external contributors, not maintainers.            ║")
		fmt.Fprintln(os.Stderr, "║                                                                  ║")
		fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr create                                      ║")
		fmt.Fprintln(os.Stderr, "║  Do this:     git push origin main                              ║")
		fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
		fmt.Fprintln(os.Stderr, "")
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}

	// Not in Gas Town context and not maintainer origin - allow PRs
	return nil
}

// isGasTownAgentContext returns true if we're running as a Gas Town managed agent.
func isGasTownAgentContext() bool {
	// Check environment variables set by Gas Town session management
	envVars := []string{
		"GT_POLECAT",
		"GT_CREW",
		"GT_WITNESS",
		"GT_REFINERY",
		"GT_MAYOR",
		"GT_DEACON",
		"GT_DOG_NAME",
	}
	for _, env := range envVars {
		if os.Getenv(env) != "" {
			return true
		}
	}

	// Also check if we're in a crew or polecat worktree by path
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}

	agentPaths := []string{"/crew/", "/polecats/", "/deacon/dogs/"}
	for _, path := range agentPaths {
		if strings.Contains(cwd, path) {
			return true
		}
	}

	return false
}

// isMaintainerOrigin returns true if the origin remote points to the maintainer's repo.
// This prevents the maintainer from accidentally creating PRs in their own repo.
func isMaintainerOrigin() bool {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	url := strings.TrimSpace(string(output))
	// Match both HTTPS and SSH URL formats:
	// - https://github.com/steveyegge/gastown.git
	// - git@github.com:steveyegge/gastown.git
	return strings.Contains(url, "steveyegge/gastown")
}

// isRefineryRole reports whether the current process is running as the
// refinery role, via either signal a refinery session may carry:
// GT_REFINERY (set directly in tmux env — see gt-r2xm's mayor comment,
// since hooks-overrides could not subtract the compiled default rule) or
// GT_ROLE resolving to "<rig>/refinery".
func isRefineryRole() bool {
	if os.Getenv("GT_REFINERY") != "" {
		return true
	}
	role, _, _ := parseRoleString(os.Getenv("GT_ROLE"))
	return role == RoleRefinery
}

// isPRCreateCommand reports whether command invokes "gh pr create" — the
// one pr-workflow pattern that stays blocked for every role, refinery
// included. Matched as three consecutive tokens (case-insensitive) rather
// than a substring, so it isn't fooled by "gh" or "pr" appearing quoted
// elsewhere in the command.
func isPRCreateCommand(command string) bool {
	tokens := shellTokenize(command)
	for i := 0; i+2 < len(tokens); i++ {
		if strings.EqualFold(tokens[i], "gh") &&
			strings.EqualFold(tokens[i+1], "pr") &&
			strings.EqualFold(tokens[i+2], "create") {
			return true
		}
	}
	return false
}

// isFeatureBranchCommand reports whether command creates a new branch via
// "git checkout -b" or "git switch -c" — the two shapes mol-refinery-patrol
// step 1 and mol-polecat-conflict-resolve mandate ("git checkout -b temp
// origin/<branch>") for merge rehearsal. Uses containment rather than
// strict adjacency (like the dangerous-command guard's fragment matching)
// so an extra flag between the subcommand and "-b"/"-c" (e.g. "git checkout
// -q -b temp") still matches.
func isFeatureBranchCommand(command string) bool {
	tokens := shellTokenize(command)
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	if !containsToken(lower, "git") {
		return false
	}
	if containsToken(lower, "checkout") && containsToken(lower, "-b") {
		return true
	}
	if containsToken(lower, "switch") && containsToken(lower, "-c") {
		return true
	}
	return false
}

// containsToken reports whether want appears as an exact element of tokens.
func containsToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}
