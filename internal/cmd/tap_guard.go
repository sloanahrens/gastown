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
  boot-sendkeys      - Block raw tmux send-keys in the boot watchdog
  bd-init            - Block bd init in wrong directories
  mol-patrol         - Block mol patrol from agent contexts
  dangerous-command  - Block rm -rf, force push, hard reset, git clean
  formula-allowlist  - Constrain dog sessions to their formula's declared commands
  container-suite    - Block unwrapped go test/make test on testcontainers-backed packages
  polecat-paths      - Block Edit/Write/Bash targets outside the polecat's own worktree
  bd-close-invariant - Block raw bd close of a bead whose branch carries unmerged work
  permission-request - Deny a permission prompt an unattended session cannot answer (PermissionRequest hook, exit 0)

External guards (standalone scripts, not compiled into gt):
  context-budget   - scripts/guards/context-budget-guard.sh

Example hook configuration (matcher is the TOOL NAME only — a command
pattern like "Bash(gh pr create*)" in "matcher" never fires, gt-5ihs — and
no "if" either: Claude Code's "if" evaluator matches ANY pattern for a
command it cannot statically resolve, so a deny guard must read
tool_input.command off stdin and self-filter, gt-3mp1):
  {
    "PreToolUse": [{
      "matcher": "Bash",
      "hooks": [{"command": "gt tap guard pr-workflow"}]
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

Two exemptions narrow the first scenario, both keyed on the same leading
branch-creation shape and each scoped to its role:
  - the refinery's mandated merge rehearsal, "git checkout -b temp
    origin/<branch>" (gt-r2xm);
  - a polecat creating a local session branch inside its own worktree
    (gt-6hg7) — the escape route for a polecat that resumed a branch whose
    push had already gone out, where gt done's recoverDivergedPush refuses
    the force-push and the alternative this message used to offer (push to
    main) is itself blocked for polecats (gt-ibt8).
"gh pr create" stays blocked for every role, including both of the above.

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
// command later on the line slip through (gt-wisp-52y4). An unquoted
// newline separates commands the same way and arrives here as ";" — see
// spaceOutShellOperators (gt-3j8u).
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
//
// Heredoc bodies are data being written or composed, not shell syntax —
// the rule evaluateDangerousCommand, checkBashCommand and
// commandInvokesRawTmuxSendKeys all state — so they are stripped before
// tokenizing. Now that an unquoted newline ends a command (gt-3j8u), a body
// line that happens to spell "gh pr create" would otherwise be read as a
// live invocation and block an ordinary file write.
func matchesPRWorkflowCommand(command string) bool {
	tokens := shellTokenize(stripHeredocBodies(strings.TrimSpace(command)))

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
	// All three "if"-matched hook patterns (gh pr create*, git checkout -b*,
	// git switch -c*) invoke this same command with no argument telling us
	// which one fired, so the actual command must be read back off stdin
	// (Claude Code hook protocol).
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		// Unreadable stdin is "we don't know what command this is", not
		// "nothing to block" — the same distinction evaluatePRWorkflowGuard
		// draws for an empty/unparsable payload (gt-wisp-52y4). Drop the
		// partial bytes io.ReadAll returns alongside the error (a truncated
		// command is not a command) and fall through to the unconditional
		// context/origin check. Returning early here failed the guard open,
		// letting the very command it exists to block through (gt-hift).
		input = nil
	}

	switch evaluatePRWorkflowGuard(input) {
	case prWorkflowBlockAgentContext:
		printPRWorkflowAgentContextBlock()
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	case prWorkflowBlockMaintainerOrigin:
		printPRWorkflowMaintainerOriginBlock()
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}
	return nil
}

// prWorkflowGuardDecision is the pr-workflow guard's verdict on one hook
// payload: allow, or block with one of the two messages below.
type prWorkflowGuardDecision int

const (
	prWorkflowAllow prWorkflowGuardDecision = iota
	prWorkflowBlockAgentContext
	prWorkflowBlockMaintainerOrigin
)

// evaluatePRWorkflowGuard decides whether a hook payload may proceed.
//
// input is the raw hook JSON, or nil when stdin could not be read — the two
// cases collapse deliberately: a payload we could not read and a payload
// holding no command both mean "we don't know what command this is", and
// self-filtering is only safe when the command is actually known
// (gt-wisp-52y4). Unknown input therefore skips the role exemptions and the
// self-filter below and falls through to the unconditional context/origin
// check — the guard's only behavior before the self-filter existed — rather
// than allowing everything.
func evaluatePRWorkflowGuard(input []byte) prWorkflowGuardDecision {
	command := extractCommand(input)

	// The refinery's mandated merge-rehearsal checkout (mol-refinery-patrol
	// step 1, mol-polecat-conflict-resolve: "git checkout -b temp
	// origin/<branch>") legitimately needs to create a branch — the same
	// shape of command isGasTownAgentContext() otherwise blocks for every
	// role. Exempt only that shape, and only for the refinery role: "gh pr
	// create" stays blocked for refineries same as everyone else (gt-r2xm).
	if isRefineryRole() && isLeadingBranchCreation(command) {
		return prWorkflowAllow
	}

	// gt-6hg7: a polecat that resumes a branch whose push already went out
	// (the gt-i0z3 class — mol-polecat-work's branch reuse rebases AND adds
	// fix commits, so the patch-ids differ and gt done's recoverDivergedPush
	// refuses the force-push by design) has no sanctioned escape. The only
	// remaining route is a fresh LOCAL session branch, and every spelling of
	// it was blocked here while the guard's own advice (push to main) is
	// blocked for polecats by gt-ibt8 — leaving the polecat fully stuck
	// pending a human force-push. Creating a local branch is not the PR path
	// this guard exists to block, so it is allowed for a polecat inside its
	// OWN worktree.
	//
	// Four conditions, each load-bearing:
	//
	//   - isPolecatSession() — the role, not merely GT_POLECAT, so a
	//     coordinator carrying a stale GT_POLECAT from spawning polecats
	//     cannot inherit the exemption.
	//   - isLeadingBranchCreation() — the command's FIRST word, so a branch
	//     creation glued after an && cannot carry a later segment past the
	//     guard (the gt-cyz8 escape).
	//   - !isPRCreateCommand() — deliberately stricter than the refinery
	//     exemption: "git checkout -b x && gh pr create" stays blocked for a
	//     polecat. Unlike a merge rehearsal, a polecat has no reason to pair
	//     the two, and the PR path is exactly what this guard is for.
	//   - isInOwnPolecatWorktree() — the worktree is the thing a session
	//     branch is being created for. A polecat that has wandered into a
	//     sibling's worktree (the gt-hmaf cross-worktree class) or the rig
	//     root stays blocked. This also keeps the doctor's live-fire probe
	//     honest: it runs with GT_POLECAT=live-fire, GT_POLECAT_PATH stripped
	//     and cwd in a /tmp sandbox, so a bare role check would let its
	//     blocked-shape 'git checkout -b' through and read that as broken
	//     matcher wiring (gt-xy4b). Last because it is the only condition
	//     that touches the filesystem.
	if isPolecatSession() && isLeadingBranchCreation(command) &&
		!isPRCreateCommand(command) && isInOwnPolecatWorktree(input) {
		return prWorkflowAllow
	}

	// Self-filter only when a command was actually extracted. Some harness
	// templates (e.g. Copilot's INPUT=$(cat) wrapper, gt-wisp-52y4) drain
	// stdin before invoking this guard, so empty/unparsable input here means
	// "we don't know what command this is", not "nothing to block".
	if command != "" && !matchesPRWorkflowCommand(command) {
		return prWorkflowAllow
	}

	// Check if we're in a Gas Town agent context
	if isGasTownAgentContext() {
		return prWorkflowBlockAgentContext
	}

	// Check if origin is the maintainer's repo (steveyegge/gastown)
	if isMaintainerOrigin() {
		return prWorkflowBlockMaintainerOrigin
	}

	// Not in Gas Town context and not maintainer origin - allow PRs
	return prWorkflowAllow
}

func printPRWorkflowAgentContextBlock() {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ PR WORKFLOW BLOCKED                                          ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(os.Stderr, "║  Gas Town workers push directly to main. PRs are forbidden.     ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Instead of:  gh pr create / git checkout -b / git switch -c    ║")
	fmt.Fprintln(os.Stderr, "║  Do this:     git add . && git commit && git push origin main   ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  In your own polecat worktree, creating a session branch is     ║")
	fmt.Fprintln(os.Stderr, "║  allowed: git checkout -b <branch> / git switch -c <branch>.    ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Why? PRs add friction that breaks autonomous execution.        ║")
	fmt.Fprintln(os.Stderr, "║  See: ~/gt/docs/PRIMING.md (GUPP principle)                     ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}

func printPRWorkflowMaintainerOriginBlock() {
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

// isPolecatSession reports whether the current process is running as a
// polecat, by the same GT_ROLE-first rule `gt sling`, `gt hook` and
// `gt handoff` use: a coordinator (mayor, witness, refinery) may carry a
// stale GT_POLECAT in its environment from having spawned polecats, so
// GT_ROLE decides whenever it is set and GT_POLECAT is only the fallback.
func isPolecatSession() bool {
	if role := strings.TrimSpace(os.Getenv("GT_ROLE")); role != "" {
		return isPolecatRole(role)
	}
	return os.Getenv("GT_POLECAT") != ""
}

// isPolecatRole reports whether a GT_ROLE value names a polecat — the one
// place the polecat check is written down, so the guards that each carry
// their own fallback marker (isPolecatSession above, inPolecatSession in
// tap_guard_dangerous.go) cannot drift on what the role itself means.
func isPolecatRole(role string) bool {
	parsed, _, _ := parseRoleString(role)
	return parsed == RolePolecat
}

// isInOwnPolecatWorktree reports whether the hook payload's session cwd sits
// inside the worktree that owns this session — "a polecat's own worktree",
// as opposed to a sibling's (gt-hmaf's cross-worktree class), the rig root,
// or a sandbox.
//
// The boundary is resolvePolecatPathScope's, reused rather than re-derived:
// that helper already answers exactly this question for the polecat-paths
// guard (identity from GT_POLECAT, layout from GT_POLECAT_PATH with the
// session cwd as fallback, symlinks resolved, rig mismatch reconciled), and
// two guards disagreeing about which tree is the polecat's would be a bug
// of its own. An unresolvable scope is not a polecat session as far as this
// question goes, so the caller stays on the blocking side — the same
// fail-closed direction resolvePolecatPathScope's callers rely on.
func isInOwnPolecatWorktree(input []byte) bool {
	scope, ok := resolvePolecatPathScope(payloadCwd(input))
	if !ok {
		return false
	}
	return scope.isWorktreePath(scope.cwd)
}

// isPRCreateCommand reports whether command invokes "gh pr create" — the
// one pr-workflow pattern that stays blocked for every role, refinery
// included. Matched as three consecutive tokens (case-insensitive) rather
// than a substring, so it isn't fooled by "gh" or "pr" appearing quoted
// elsewhere in the command. The scan is per-segment, not per-token-position:
// the exemption that consults this guard fires when the command's FIRST
// token is gh, and the exemption's own test is segment-first, so a "gh pr
// create" glued after an "&&" on one line must match here too or the
// refinery exemption chains past it (gt-cyz8).
func isPRCreateCommand(command string) bool {
	for _, seg := range splitShellSegments(shellTokenize(strings.TrimSpace(command))) {
		if len(seg) < 3 {
			continue
		}
		if strings.EqualFold(seg[0], "gh") &&
			strings.EqualFold(seg[1], "pr") &&
			strings.EqualFold(seg[2], "create") {
			return true
		}
	}
	return false
}

// isLeadingBranchCreation reports whether command's leading segment is a
// local branch creation — "git checkout -b" or "git switch -c" — with
// containment for the creation flag so an extra flag between the subcommand
// and "-b"/"-c" (e.g. "git checkout -q -b temp") still matches.
//
// Both exemptions the pr-workflow guard grants are keyed on this one shape:
// the refinery's mandated merge rehearsal (gt-r2xm) and a polecat's fresh
// session branch (gt-6hg7). Each adds its own scope — role, and for a
// polecat the worktree — but the command shape is shared, so the two cannot
// drift apart on what "branch creation" means.
//
// Leading-command by design: the hook "if" globs that route a command here
// (Bash(git checkout -b*), Bash(gh pr create*)) are anchored to the
// command's first word, so the exemption must be too. Keying the old
// isFeatureBranchCommand's line-wide containment here instead would let a
// "gh pr create && git checkout -b" chain its PR create past the exemption
// (gt-cyz8) — so a checkout glued after an && on the same line is NOT branch
// creation here and exempts nothing.
func isLeadingBranchCreation(command string) bool {
	segs := splitShellSegments(shellTokenize(strings.TrimSpace(command)))
	if len(segs) == 0 {
		return false
	}
	seg := segs[0]
	if len(seg) < 2 || !strings.EqualFold(seg[0], "git") {
		return false
	}
	lower := make([]string, len(seg))
	for i, tok := range seg {
		lower[i] = strings.ToLower(tok)
	}
	return (containsToken(lower, "checkout") || containsToken(lower, "switch")) &&
		(containsToken(lower, "-b") || containsToken(lower, "-c"))
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
