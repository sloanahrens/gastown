package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// tap_guard_bd_close_invariant.go implements `gt tap guard bd-close-invariant`
// (gt-arno), the PreToolUse half of the gt-6hmz close-time invariant.
//
// The gap: gt-6hmz made gt done re-verify, at close time, that a bead it is
// about to close is actually tracked (zero unmerged commits, or a live MR, or
// an operator supersede:/cancel: reason). But `bd` is an external binary in the
// sibling beads repo — every call in this repo shells out (internal/beads/
// beads.go) — so an agent running `bd close <id>` straight from a Bash tool
// call bypasses done.go entirely. Nothing inside gt's Go code runs at all.
//
// This repo's own PreToolUse guard machinery is the only interception point
// available, so this guard reads the Bash command off stdin, finds the bead IDs
// a `bd close` would close, and refuses the ones the invariant refuses.
//
// Design rules, in the order they matter:
//
//  1. One predicate, not two. The decision is delegated to
//     closeTimeInvariantSkipReason — the exact function gt done calls — with
//     the branch/target pair resolved by the same closeTimeBranchTarget helper
//     gt done uses. A guard that reimplemented the check would drift from it,
//     and a guard that refused closes its own gt done allows (or vice versa)
//     would be worse than no guard. The one deliberate difference is exit (c):
//     gt done passes "" as the close reason, so it can never take the operator
//     override; a raw `bd close --reason="supersede: ..."` is the path that
//     actually carries an operator reason, so the guard passes it through.
//
//  2. Scope by the branch, not by the hook. The invariant is about "a bead
//     that is the source_issue of polecat work", and a bare `bd close` gives no
//     signal for that on its own. The signal used here is the worktree's own
//     branch name: polecat branches are `polecat/<name>/<bead-id>+<suffix>`
//     (gt-6hmz's own branch format), so a bead is judged only when its ID
//     appears as a whole component of the current branch name. Everything else
//     — closing a filed bug bead, a molecule step, a convoy, an unrelated
//     task — is not this guard's business and passes untouched.
//
//     The rejected alternative was scoping by the agent bead's hook_bead.
//     That would block the legitimate conflict-resolution close, where the
//     hooked bead is the conflict *task* and the branch belongs to a different
//     source issue (the refinery waits on the task closing to retry the merge;
//     the branch-name rule exempts it correctly, a hook rule would not).
//
//  3. Fail open on anything unresolvable. No command on stdin, no cwd, no town
//     root, no git context, detached HEAD, or the default branch all mean "the
//     invariant is not evaluable here", never "refuse". This guard runs on
//     every Bash call in every rig, so a false refusal costs the whole town;
//     the invariants that must never fail open are already gated elsewhere in
//     gt done's own close path.
//
//  4. Compose with the other guards by saying nothing extra. tap_guard_
//     formula_allowlist.go's dog baseline permits "bd close" outright. Guards
//     are independent — exit 2 from any one blocks the call — so no exemption
//     is needed here: a dog runs its formula in the deacon kennel, not on a
//     polecat branch, so this guard is already a no-op for it. Hardcoding a
//     dog bypass would be wrong for the case where a dog really is closing a
//     source bead from a feature branch.
const (
	// bdCloseInvariantBeadPrefixMinLen is the shortest string accepted as a
	// bead ID when matching against a branch name. Bead IDs are
	// "<prefix>-<slug>" (gt-arno, hq-cv-vbvss), so a token without a dash —
	// "temp", a git ref component, a hex suffix — is never a bead ID and must
	// not be treated as one.
	bdCloseInvariantBeadPrefixMinLen = 3

	// bdCloseInvariantBranchSeparator separates the bead ID from the random
	// suffix in a polecat branch name ("polecat/<name>/<bead-id>+<suffix>"),
	// alongside "/". Both are component separators when matching a bead ID
	// against a branch name.
	bdCloseInvariantBranchSeparator = '+'
)

var tapGuardBdCloseInvariantCmd = &cobra.Command{
	Use:   "bd-close-invariant",
	Short: "Block raw bd close of a bead whose branch carries unmerged work",
	Long: `Enforce the gt-6hmz close-time invariant on raw bd close calls (PreToolUse hook).

gt-6hmz made 'gt done' verify, at close time, that a bead it closes is actually
tracked — its branch has no commits the target lacks, or an open MR tracks it,
or it carries an explicit supersede:/cancel: reason. A raw 'bd close <id>' from
an agent's Bash call never reaches that code, so this guard evaluates the same
invariant for the ids such a command would close.

Only ids that name the bead the current branch was cut for (the branch is
polecat/<name>/<bead-id>+<suffix>) are judged. Closing anything else — a filed
bug, a molecule step, another agent's bead — is untouched.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED (invariant would refuse this close)`,
	SilenceUsage: true,
	RunE:         runTapGuardBdCloseInvariant,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardBdCloseInvariantCmd)
}

// bdCloseInvocation is one shell segment's worth of `bd close`: the ids it
// would close and the close reason it carries.
//
// Ids is only populated when at least one non-flag argument was present, so an
// invocation with no recognizable id (a shell variable, a command substitution)
// is reported as an invocation with zero ids rather than being dropped — the
// caller can then tell "no bd close here" from "a bd close whose ids this
// parser could not read".
type bdCloseInvocation struct {
	IDs    []string
	Reason string
}

// payloadCwd reads the session cwd from the hook payload. The branch under
// judgment is the one checked out *there*, so the payload wins over this
// process's own cwd whenever the harness supplies it — the same rule
// tap_guard_polecat_paths.go follows for relative targets.
func payloadCwd(input []byte) string {
	var hook struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(input, &hook); err != nil {
		return ""
	}
	return hook.Cwd
}

func runTapGuardBdCloseInvariant(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		// Unreadable stdin is "we don't know what command this is", and this
		// guard's only behavior is command-dependent — there is no
		// unconditional context check to fall through to (unlike the
		// pr-workflow guard). Fail open, like every other input this guard
		// cannot resolve.
		return nil
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}

	invocations := parseBdCloseInvocations(command)
	if len(invocations) == 0 {
		return nil
	}

	scope, ok := resolveBdCloseInvariantScope(payloadCwd(input))
	if !ok {
		return nil
	}

	for _, invocation := range invocations {
		for _, id := range invocation.IDs {
			if !branchNamesBead(scope.branch, id) {
				continue
			}
			refusal := bdCloseInvariantRefusal(scope, id, invocation.Reason)
			if refusal == "" {
				continue
			}
			printBdCloseInvariantBlock(id, scope.branch, refusal)
			return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
		}
	}
	return nil
}

// bdCloseInvariantScope is everything the invariant needs that depends on the
// session rather than the command: which branch is being judged, what it is
// judged against, the rig beads client MR beads live in, and the MR this
// session last submitted.
//
// mrTracker is nil when the rig holds no beads database at all — see
// rigBeadsWorkspaceExists. The predicate skips exit (b) for a nil tracker, so
// such a session is judged on commits and operator reason alone.
type bdCloseInvariantScope struct {
	branch    string
	target    string
	mrTracker closeTimeMRTracker
	counter   closeTimeCommitCounter
	pendingMR string
}

// resolveBdCloseInvariantScope gathers the session context for the invariant,
// or reports ok=false when there is nothing meaningful to evaluate — not in a
// Gas Town agent context, no cwd, no town root, no rig, no branch to compare.
//
// payloadCwd is the session cwd from the hook payload; os.Getwd is the
// fallback, for harnesses whose payload omits it.
//
// The rig beads client is built from the rig path (not the worktree), matching
// gt done: MR beads always live in the rig DB, and cwd may be a checkout the
// rig path cannot be derived from once the worktree is gone.
func resolveBdCloseInvariantScope(payloadCwd string) (bdCloseInvariantScope, bool) {
	if !isGasTownAgentContext() {
		return bdCloseInvariantScope{}, false
	}
	cwd := payloadCwd
	if cwd == "" || !filepath.IsAbs(cwd) {
		wd, err := os.Getwd()
		if err != nil {
			return bdCloseInvariantScope{}, false
		}
		cwd = wd
	}
	townRoot, err := workspace.Find(cwd)
	if err != nil {
		return bdCloseInvariantScope{}, false
	}
	// An empty town root is Find's "not in a town" answer, and it is not
	// usable here: it would make the rig path relative and silently pick up
	// whichever directory named <rig> happened to sit under cwd.
	if strings.TrimSpace(townRoot) == "" {
		return bdCloseInvariantScope{}, false
	}

	// Rig identity comes from the role context, with GT_RIG as a backstop for
	// a session whose cwd-derived detection fails but whose env is intact.
	rigName := os.Getenv("GT_RIG")
	ctx, ctxErr := GetRoleWithContext(cwd, townRoot)
	if ctxErr == nil {
		if ctx.Rig != "" {
			rigName = ctx.Rig
		}
		if ctx.TownRoot != "" {
			townRoot = ctx.TownRoot
		}
	}
	if rigName == "" {
		return bdCloseInvariantScope{}, false
	}

	g := git.NewGit(cwd)
	branch, target, ok := closeTimeBranchTarget(g, townRoot, rigName)
	if !ok {
		return bdCloseInvariantScope{}, false
	}

	scope := bdCloseInvariantScope{branch: branch, target: target, counter: g}
	rigPath := filepath.Join(townRoot, rigName)
	if rigBeadsWorkspaceExists(rigPath) {
		rigBd := beads.New(rigPath)
		scope.mrTracker = rigBd
		scope.pendingMR = resolveBdCloseInvariantPendingMR(ctx, ctxErr, rigBd)
	}
	return scope, true
}

// rigBeadsWorkspaceExists reports whether rigPath actually holds a beads
// database — the precondition for asking bd anything at all.
//
// Without it the guard would shell out to bd from a directory that has no
// database, which answers nothing and (on a host with a shared Dolt server)
// risks resolving to, or creating, the wrong one. A missing database therefore
// means "no MR can be looked up here", which the predicate handles as a nil
// tracker rather than as an error.
func rigBeadsWorkspaceExists(rigPath string) bool {
	info, err := os.Stat(beads.ResolveBeadsDir(rigPath))
	return err == nil && info.IsDir()
}

// resolveBdCloseInvariantPendingMR returns the MR this session last submitted,
// read off the agent bead's active_mr field — the same field gt done consults
// for the identical purpose.
//
// This is the field gt-6hmz objected to trusting *unverified*: the exported
// predicate re-verifies it with a live Show and requires a non-terminal
// status, so a stale pointer to a closed MR does not count as tracking. Every
// failure here (no agent bead, unreadable, no active_mr) yields "", which
// sends exit (b) to its by-issueID fallback instead of disabling it
// (gt-h8ld) — the field being unreadable isn't evidence no MR exists.
func resolveBdCloseInvariantPendingMR(ctx RoleContext, ctxErr error, rigBd *beads.Beads) string {
	if ctxErr != nil {
		// No resolved role means no agent bead to identify, and ctx is the
		// zero value getAgentBeadID would read as "unknown role".
		return ""
	}
	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		return ""
	}
	agentIssue, err := rigBd.ForAgentBead().Show(agentBeadID)
	if err != nil || agentIssue == nil {
		return ""
	}
	fields := beads.ParseAgentFields(agentIssue.Description)
	if fields == nil {
		return ""
	}
	return strings.TrimSpace(fields.ActiveMR)
}

// bdCloseInvariantRefusal evaluates the gt-6hmz invariant for one bead id and
// returns the refusal message, or "" when the close may proceed.
func bdCloseInvariantRefusal(scope bdCloseInvariantScope, issueID, closeReason string) string {
	return closeTimeInvariantSkipReason(
		scope.mrTracker, scope.counter,
		issueID, scope.pendingMR,
		scope.branch, scope.target,
		closeReason,
	)
}

// branchNamesBead reports whether branch embeds issueID as one of its own
// name components.
//
// Polecat branches are `polecat/<name>/<bead-id>+<suffix>`, so splitting on the
// component separators ("/" and "+") and comparing whole components is what
// makes this exact: `polecat/malachite/gt-arno+muck73gu` names gt-arno and
// nothing else, while `polecat/malachite/gt-arnold+muck73gu` does not name
// gt-arno. A malformed or renamed branch simply names no bead, which is the
// fail-open direction this guard wants.
func branchNamesBead(branch, issueID string) bool {
	id := strings.ToLower(strings.TrimSpace(issueID))
	if !isBeadIDShaped(id) {
		return false
	}
	for _, component := range strings.FieldsFunc(strings.ToLower(branch), func(r rune) bool {
		return r == '/' || r == bdCloseInvariantBranchSeparator
	}) {
		if component == id {
			return true
		}
	}
	return false
}

// isBeadIDShaped reports whether s has the town's "<prefix>-<slug>" bead ID
// shape. The dash requirement is what keeps a branch component that happens to
// equal a plain word ("temp", "main", the hex suffix of another polecat's
// branch) from being read as a bead ID.
func isBeadIDShaped(s string) bool {
	return len(s) >= bdCloseInvariantBeadPrefixMinLen && strings.Contains(s, "-")
}

// parseBdCloseInvocations finds every `bd close` in command and extracts the
// ids and reason each one carries.
//
// The command is tokenized (quoted text stays one opaque token, so an id
// mentioned inside a quoted argument is never mistaken for an argument of the
// command), split into per-segment sub-commands on shell operators, and each
// segment is inspected on its own — `cd /tmp && bd close gt-x` must be found
// on its second segment, and a heredoc body that merely mentions "bd close" is
// data, not a command.
//
// Only a segment whose command word is literally `bd` (or a path ending in
// /bd) followed by `close` is an invocation. Everything a close can carry is
// accounted for: environment assignments are skipped, flags are skipped, and
// the value of -r/--reason is consumed as the reason rather than mistaken for
// an id.
func parseBdCloseInvocations(command string) []bdCloseInvocation {
	tokens := shellTokenize(stripHeredocBodies(strings.TrimSpace(command)))

	var invocations []bdCloseInvocation
	var segment []string
	flush := func() {
		if invocation, ok := parseBdCloseSegment(segment); ok {
			invocations = append(invocations, invocation)
		}
		segment = nil
	}
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			flush()
			continue
		}
		segment = append(segment, tok)
	}
	flush()
	return invocations
}

// parseBdCloseSegment parses one shell segment as a possible `bd close`.
func parseBdCloseSegment(segment []string) (bdCloseInvocation, bool) {
	tokens := stripLeadingEnvAssignments(segment)
	if len(tokens) < 2 {
		return bdCloseInvocation{}, false
	}
	if !isBdCommandWord(tokens[0]) || tokens[1] != "close" {
		return bdCloseInvocation{}, false
	}

	var invocation bdCloseInvocation
	rest := tokens[2:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "-r" || tok == "--reason":
			// The reason is the next token; a trailing -r with no value is a
			// malformed command that the shell will reject, so there is
			// nothing to read and nothing to skip.
			if i+1 < len(rest) {
				invocation.Reason = rest[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--reason="):
			invocation.Reason = strings.TrimPrefix(tok, "--reason=")
		case strings.HasPrefix(tok, "-r") && len(tok) > 2:
			// Glued short form: "-rsupersede: dup".
			invocation.Reason = tok[2:]
		case strings.HasPrefix(tok, "-") && tok != "-":
			// Any other flag (-f/--force, --json, ...) has no bearing on the
			// invariant and is not an id.
		default:
			invocation.IDs = append(invocation.IDs, tok)
		}
	}
	return invocation, true
}

// isBdCommandWord reports whether tok invokes the bd binary — either the bare
// name or a path to it (/usr/local/bin/bd). A token whose last path component
// is "bd" is the same program; "subd" or "bd2" is not.
func isBdCommandWord(tok string) bool {
	if tok == "bd" {
		return true
	}
	return strings.HasSuffix(tok, "/bd")
}

// stripLeadingEnvAssignments drops leading VAR=value tokens (and the
// "env"/"command" prefixes that can precede them) so `FOO=1 bd close gt-x` is
// recognized. An assignment is only stripped from the front, so an argument
// later in the command that merely contains "=" is left alone. Reuses
// isEnvAssignment from tap_guard_polecat_paths.go, which draws the same
// NAME=value distinction for the same reason.
func stripLeadingEnvAssignments(tokens []string) []string {
	for len(tokens) > 0 {
		tok := tokens[0]
		if tok == "env" || tok == "command" {
			tokens = tokens[1:]
			continue
		}
		if isEnvAssignment(tok) {
			tokens = tokens[1:]
			continue
		}
		break
	}
	return tokens
}

// printBdCloseInvariantBlock prints the refusal banner naming the bead, the
// branch it belongs to, and the predicate's own message (which carries the
// unmerged commit count).
func printBdCloseInvariantBlock(issueID, branch, refusal string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ BD CLOSE BLOCKED — UNMERGED WORK                             ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Bead:   %-55s ║\n", truncateStr(issueID, 55))
	fmt.Fprintf(os.Stderr, "║  Branch: %-55s ║\n", truncateStr(branch, 55))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintf(os.Stderr, "║  %-63s ║\n", truncateStr(refusal, 63))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  A bead with no commits on this branch closes normally (that    ║")
	fmt.Fprintln(os.Stderr, "║  is exit (a)); so does one whose MR is still live (exit (b)).   ║")
	fmt.Fprintln(os.Stderr, "║  Otherwise submit with 'gt done', or record an operator reason: ║")
	fmt.Fprintln(os.Stderr, "║    bd close <id> --reason=\"supersede: <what replaced it>\"        ║")
	fmt.Fprintln(os.Stderr, "║    bd close <id> --reason=\"cancel: <why it is abandoned>\"       ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  See: gt-6hmz (close-time invariant), gt-arno (this guard)       ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
