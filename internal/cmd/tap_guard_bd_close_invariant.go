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
	"github.com/steveyegge/gastown/internal/cmdtree"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// tap_guard_bd_close_invariant.go implements `gt tap guard bd-close-invariant`
// (gt-arno), the PreToolUse half of the gt-6hmz close-time invariant.
//
// The gap: gt-6hmz made gt done re-verify, at close time, that a bead it is
// about to close carries no unlanded work (zero unmerged commits, or an
// operator supersede:/cancel: reason). But `bd` is an external binary in the
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
//     — closing a filed bug bead, a molecule step, an unrelated task — is not
//     this guard's business and passes untouched.
//
//     The rejected alternative was scoping by the agent bead's hook_bead,
//     which would also judge a hooked task whose branch belongs to a
//     different source issue.
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

	// bdCloseReasonFileMaxBytes caps how much of a --reason-file is read. A
	// supersede:/cancel: reason is a sentence, so the cap is generous.
	bdCloseReasonFileMaxBytes = 64 << 10
)

var tapGuardBdCloseInvariantCmd = &cobra.Command{
	Use:   "bd-close-invariant",
	Short: "Block raw bd close of a bead whose branch carries unmerged work",
	Long: `Enforce the gt-6hmz close-time invariant on raw bd close calls (PreToolUse hook).

gt-6hmz made 'gt done' verify, at close time, that a bead it closes is actually
unlanded work — its branch has no commits the target lacks, or it carries an
explicit supersede:/cancel: reason. A raw 'bd close <id>' from
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
//
// Reason is the last --reason given. bd maps repeated --reason flags to ids by
// position (the first reason to the first id), so Reasons keeps every one when
// the flag was repeated and is nil otherwise; reasonFor reads the pair. A
// --reason-file is carried as ReasonFile and read by the guard, which knows the
// session's cwd; the parser never touches the filesystem.
//
// IDsUnreadable marks an `xargs bd close` whose ids come from a producer the
// parser cannot read (a file, a query): the close is real but names ids unknown
// here, so the guard judges the bead its branch was cut for.
type bdCloseInvocation struct {
	IDs           []string
	Reason        string
	Reasons       []string
	ReasonFile    string
	IDsUnreadable bool
}

// reasonFor returns the close reason that applies to the idx'th id. Repeated
// reasons pair with ids by position only when there is one per id; any other
// count is a command bd rejects, so the last reason stands, as it did before
// the flags were tracked per id.
func (inv bdCloseInvocation) reasonFor(idx int) string {
	if len(inv.Reasons) == len(inv.IDs) && idx < len(inv.Reasons) {
		return inv.Reasons[idx]
	}
	return inv.Reason
}

// withReasonFile fills in the reason a --reason-file names. An explicit
// --reason wins, and a file that cannot be read leaves the reason empty, which
// the invariant refuses exactly as it did when the flag was not understood.
// A relative path is read against cwd, the directory bd runs in. The path comes
// from the command, so only a regular file is read, and only its first
// bdCloseReasonFileMaxBytes: /dev/zero, a FIFO or a multi-gigabyte file named
// there must not stall or exhaust a guard that runs on every Bash call.
func (inv bdCloseInvocation) withReasonFile(cwd string) bdCloseInvocation {
	if inv.ReasonFile == "" || inv.Reason != "" || inv.ReasonFile == "-" {
		return inv
	}
	path := inv.ReasonFile
	if !filepath.IsAbs(path) && filepath.IsAbs(cwd) {
		path = filepath.Join(cwd, path)
	}
	reason, ok := readBdCloseReasonFile(path)
	if !ok {
		return inv
	}
	inv.Reason = reason
	return inv
}

// readBdCloseReasonFile returns the trimmed head of the regular file at path.
// A device, FIFO, directory or unreadable path reports ok=false without being
// opened, so the read cannot block; a regular file is cut at
// bdCloseReasonFileMaxBytes, far past any close reason.
func readBdCloseReasonFile(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, bdCloseReasonFileMaxBytes))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
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
	return tapGuardBdCloseInvariant(os.Stdin, realGuardProcess())
}

// tapGuardBdCloseInvariant is the guard: it reads the hook payload from
// stdin and the session from proc.
func tapGuardBdCloseInvariant(stdin io.Reader, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
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

	scope, ok := resolveBdCloseInvariantScope(payloadCwd(input), proc)
	if !ok {
		return nil
	}

	if id, refusal := firstBdCloseRefusal(scope, invocations, payloadCwd(input)); refusal != "" {
		printBdCloseInvariantBlock(id, scope.branch, refusal)
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	}
	return nil
}

// firstBdCloseRefusal returns the first id among invocations that the
// invariant refuses to close, with the refusal message, or "" when every close
// may proceed. cwd is the directory a --reason-file path is read against.
func firstBdCloseRefusal(scope bdCloseInvariantScope, invocations []bdCloseInvocation, cwd string) (id, refusal string) {
	for _, invocation := range invocations {
		invocation = invocation.withReasonFile(cwd)
		if invocation.IDsUnreadable {
			// The ids are unknown, so this close may name the bead the branch
			// was cut for; judge it as if it did, under the reason the command
			// carries (a supersede:/cancel: reason still clears it).
			if id := branchBeadID(scope.branch); id != "" {
				if refusal := bdCloseInvariantRefusal(scope, id, invocation.Reason); refusal != "" {
					return id, refusal
				}
			}
		}
		for i, id := range invocation.IDs {
			if !branchNamesBead(scope.branch, id) {
				continue
			}
			if refusal := bdCloseInvariantRefusal(scope, id, invocation.reasonFor(i)); refusal != "" {
				return id, refusal
			}
		}
	}
	return "", ""
}

// bdCloseInvariantScope is everything the invariant needs that depends on the
// session rather than the command: which branch is being judged and what it
// is judged against.
type bdCloseInvariantScope struct {
	branch  string
	target  string
	counter closeTimeCommitCounter
}

// resolveBdCloseInvariantScope gathers the session context for the invariant,
// or reports ok=false when there is nothing meaningful to evaluate — not in a
// Gas Town agent context, no cwd, no town root, no rig, no branch to compare.
//
// payloadCwd is the session cwd from the hook payload; os.Getwd is the
// fallback, for harnesses whose payload omits it.
func resolveBdCloseInvariantScope(payloadCwd string, proc guardProcess) (bdCloseInvariantScope, bool) {
	if !isGasTownAgentContextIn(proc.getenv, proc.getwd) {
		return bdCloseInvariantScope{}, false
	}
	cwd := payloadCwd
	if cwd == "" || !filepath.IsAbs(cwd) {
		wd, err := proc.getwd()
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
	rigName := proc.getenv("GT_RIG")
	ctx, ctxErr := getRoleWithContextEnv(cwd, townRoot, proc.getenv)
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
	// The branch's own bead carries the base_branch its work lands on.
	var bead *beads.Issue
	if current, err := g.CurrentBranch(); err == nil {
		bead = closeInvariantBranchBead(current, beads.New(cwd).Show)
	}
	branch, target, ok := closeTimeBranchTarget(g, townRoot, rigName, bead)
	if !ok {
		return bdCloseInvariantScope{}, false
	}

	return bdCloseInvariantScope{branch: branch, target: target, counter: g}, true
}

// closeInvariantBranchBead returns the bead branch was cut for, read through
// show. Only that bead is looked up: it is the only bead the guard judges
// (branchNamesBead). It returns nil for a branch that names no bead, or when
// show cannot read the bead, and the target then falls back to the rig
// default, as a bead with no base_branch does.
func closeInvariantBranchBead(branch string, show func(id string) (*beads.Issue, error)) *beads.Issue {
	id := branchBeadID(branch)
	if id == "" {
		return nil
	}
	issue, err := show(id)
	if err != nil {
		return nil
	}
	return issue
}

// bdCloseInvariantRefusal evaluates the gt-6hmz invariant for one bead id and
// returns the refusal message, or "" when the close may proceed.
func bdCloseInvariantRefusal(scope bdCloseInvariantScope, issueID, closeReason string) string {
	return closeTimeInvariantSkipReason(scope.counter, issueID, scope.branch, scope.target, closeReason)
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

// branchBeadID returns the bead id a polecat branch
// (`polecat/<name>/<bead-id>+<suffix>`) was cut for, or "" for any other branch
// shape, which names no bead.
func branchBeadID(branch string) string {
	parts := strings.Split(branch, "/")
	if len(parts) != 3 || parts[0] != "polecat" {
		return ""
	}
	id, _, _ := strings.Cut(parts[2], string(bdCloseInvariantBranchSeparator))
	id = strings.ToLower(strings.TrimSpace(id))
	if !isBeadIDShaped(id) {
		return ""
	}
	return id
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
	stripped := stripHeredocBodies(strings.TrimSpace(command))
	tokens := shellTokenize(stripped)

	var invocations []bdCloseInvocation
	var segment []string
	// pipedFrom is the segment feeding the current one through a pipe, which is
	// where `xargs bd close` gets the ids it closes.
	var pipedFrom []string
	caseDepth := 0
	flush := func() {
		caseDepth += bdCloseCaseDepthChange(segment)
		if caseDepth > 0 {
			segment = bdCloseUnglueArmLabel(segment)
		}
		if invocation, ok := parseBdCloseSegment(segment); ok {
			if bdCloseSegmentRunsXargs(segment) {
				ids, readable := bdCloseEchoedIDs(pipedFrom)
				invocation.IDs = append(invocation.IDs, ids...)
				invocation.IDsUnreadable = !readable
			}
			invocations = append(invocations, invocation)
		}
		invocations = append(invocations, parseBdCloseShellPayload(segment)...)
	}
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			flush()
			pipedFrom = nil
			if tok == "|" {
				pipedFrom = segment
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	flush()
	// A command substitution runs its body as a command of its own, wherever
	// the surrounding word sits (`echo $(bd close gt-x)`), so each body is
	// parsed the way a whole command is. A nested substitution is found when
	// its enclosing body is parsed.
	for _, body := range commandSubstitutions(stripped) {
		invocations = append(invocations, parseBdCloseInvocations(body)...)
	}
	return invocations
}

// bdCloseSegmentRunsXargs reports whether xargs stands in front of the bd
// command word of segment, so the ids piped into the segment are bd's operands.
func bdCloseSegmentRunsXargs(segment []string) bool {
	for _, tok := range segment {
		if isBdCommandWord(tok) {
			return false
		}
		if filepath.Base(tok) == "xargs" {
			return true
		}
	}
	return false
}

// bdCloseEchoedIDs returns the words an `echo` or `printf` segment writes, the
// ids that `echo gt-x | xargs bd close` hands to bd. Any other producer (a
// file, a query), or none at all, is not readable: readable is false and the
// caller judges the branch's own bead rather than letting the close through
// unexamined.
func bdCloseEchoedIDs(segment []string) (ids []string, readable bool) {
	tokens := bdCloseCommandTokens(segment)
	if len(tokens) < 2 {
		return nil, false
	}
	if word := filepath.Base(tokens[0]); word != "echo" && word != "printf" {
		return nil, false
	}
	for _, tok := range tokens[1:] {
		if strings.HasPrefix(tok, "-") || strings.Contains(tok, "%") {
			continue // an echo flag or a printf format directive
		}
		ids = append(ids, strings.Fields(strings.ReplaceAll(tok, `\n`, " "))...)
	}
	return ids, true
}

// parseBdCloseSegment parses one shell segment as a possible `bd close`.
func parseBdCloseSegment(segment []string) (bdCloseInvocation, bool) {
	tokens := bdCloseCommandTokens(segment)
	if len(tokens) < 2 {
		return bdCloseInvocation{}, false
	}
	if !isBdCommandWord(tokens[0]) {
		return bdCloseInvocation{}, false
	}
	sub := bdSubcommandIndex(tokens[1:])
	if sub < 0 || !isBdCloseSubcommand(tokens[1+sub]) {
		return bdCloseInvocation{}, false
	}

	var invocation bdCloseInvocation
	rest := tokens[2+sub:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "-r" || tok == "--reason":
			// The reason is the next token; a trailing -r with no value is a
			// malformed command that the shell will reject, so there is
			// nothing to read and nothing to skip.
			if i+1 < len(rest) {
				invocation.addReason(rest[i+1])
				i++
			}
		case strings.HasPrefix(tok, "--reason="):
			invocation.addReason(strings.TrimPrefix(tok, "--reason="))
		case strings.HasPrefix(tok, "-r") && len(tok) > 2:
			// Glued short form: "-rsupersede: dup".
			invocation.addReason(tok[2:])
		case tok == "--reason-file":
			// The path is the flag's value, not an id; the guard reads it.
			if i+1 < len(rest) {
				invocation.ReasonFile = rest[i+1]
				i++
			}
		case strings.HasPrefix(tok, "--reason-file="):
			invocation.ReasonFile = strings.TrimPrefix(tok, "--reason-file=")
		case strings.HasPrefix(tok, "-") && tok != "-":
			// Any other flag (-f/--force, --json, ...) has no bearing on the
			// invariant and is not an id.
		default:
			invocation.IDs = append(invocation.IDs, tok)
		}
	}
	// Reasons only carries a repeated flag; one reason is Reason alone.
	if len(invocation.Reasons) < 2 {
		invocation.Reasons = nil
	}
	return invocation, true
}

// addReason records one --reason value: Reason keeps the last, Reasons all.
func (inv *bdCloseInvocation) addReason(reason string) {
	inv.Reason = reason
	inv.Reasons = append(inv.Reasons, reason)
}

// isBdCloseSubcommand reports whether word is a name `bd close` answers to.
// Matching only "close" let `bd done` close a bead past the invariant (gt-ltua8).
func isBdCloseSubcommand(word string) bool {
	return word == "close" || word == "done"
}

// bdSubcommandIndex returns the position of bd's subcommand in rest (the
// tokens after "bd"), skipping global flags in both `--db x` and `--db=x`
// forms, or -1 when rest ends before one (gt-kocid). The value-taking flags
// come from cmdtree's list, so `bd --db x close gt-1` and `bd --format json
// close gt-1` both resolve to close (gt-m8efc).
func bdSubcommandIndex(rest []string) int {
	for i := 0; i < len(rest); i++ {
		t := rest[i]
		if !strings.HasPrefix(t, "-") {
			return i
		}
		if cmdtree.IsBdGlobalValueFlag(t) {
			i++ // skip the flag's value
		}
	}
	return -1
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

// bdCloseLeadWords open a clause or group and are followed by a command, as in
// `then bd close X`, `if bd close X; then ...; fi` and `( bd close X )`; bdCloseLaunchers run the command that
// follows them. Both are looked through so the close behind them is judged
// (deep-review gt-dilqk).
var (
	bdCloseLeadWords = map[string]bool{"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "{": true, "(": true, "!": true}
	bdCloseLaunchers = map[string]bool{"time": true, "nohup": true, "exec": true, "sudo": true, "nice": true, "stdbuf": true, "timeout": true, "xargs": true}
)

// bdCloseLauncherValueFlags names, per launcher, the flags that consume the
// next token as their value. Without this table the value is left behind as the
// command word and the close behind it is never parsed: `sudo -u alice bd close
// gt-x` stopped at "alice" (gt-2bvyy). The table is per launcher because the
// same short letter means different things to different programs — nice's -n is
// a priority, sudo's -h a host — and naming a flag the launcher does not take
// would swallow a token that is really the next argument. A launcher flag not
// listed here is dropped on its own, which is what every launcher did before, so
// the table can only ever reveal more closes (gt-gzz4j's review flagged the
// residual this closes).
var bdCloseLauncherValueFlags = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true, "--chdir": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true},
	"timeout": {"-s": true, "-k": true},
	"nice":    {"-n": true},
	"xargs":   {"-I": true, "-n": true, "-P": true, "-L": true, "-s": true, "-d": true, "-E": true, "-a": true},
}

// bdCloseCommandTokens returns the segment from its command word on: clause and
// group openers, env assignments, launchers (with their own flags and the
// values those flags take) and case-arm labels are dropped from the front, and
// a group's closing ) or } from the back.
func bdCloseCommandTokens(segment []string) []string {
	tokens := segment
	for len(tokens) > 0 {
		before := len(tokens)
		tokens = stripLeadingEnvAssignments(tokens)
		if len(tokens) > 0 && bdCloseLeadWords[tokens[0]] {
			tokens = tokens[1:]
		}
		if end := bdCloseCaseHeaderEnd(tokens); end >= 0 {
			tokens = tokens[end:]
		}
		if end := bdCloseCaseArmLabelEnd(tokens); end >= 0 {
			tokens = tokens[end+1:]
		}
		if len(tokens) > 0 && bdCloseLaunchers[filepath.Base(tokens[0])] {
			launcher := filepath.Base(tokens[0])
			tokens = tokens[1:]
			valueFlags := bdCloseLauncherValueFlags[launcher]
			for len(tokens) > 0 && strings.HasPrefix(tokens[0], "-") {
				flag := tokens[0]
				tokens = tokens[1:]
				if valueFlags[flag] {
					if len(tokens) > 0 {
						tokens = tokens[1:]
					}
				}
			}
			if launcher == "timeout" && len(tokens) > 0 {
				tokens = tokens[1:] // the duration
			}
		}
		if len(tokens) == before {
			break
		}
	}
	for len(tokens) > 0 && (tokens[len(tokens)-1] == ")" || tokens[len(tokens)-1] == "}") {
		tokens = tokens[:len(tokens)-1]
	}
	return tokens
}

// bdCloseCaseHeaderEnd returns the index just past the `in` that ends a
// `case WORD in` header at the front of tokens, or -1 when tokens opens with
// no such header.
//
// A case arm is `case WORD in PATTERN) LIST`, so the header shares its segment
// with the first arm's label and the LIST's command is never the segment's
// first word. Without dropping the header, a `bd close` in the first arm is
// invisible to the invariant where the same command outside a case is judged
// (gt-37k43). A `case` whose `in` this scan does not reach leaves the segment
// alone, which is the same "not a bd close" answer as before.
func bdCloseCaseHeaderEnd(tokens []string) int {
	if len(tokens) == 0 || tokens[0] != "case" {
		return -1
	}
	for i := 1; i < len(tokens); i++ {
		if tokens[i] == "in" {
			return i + 1
		}
	}
	return -1
}

// bdCloseCaseArmLabelEnd returns the index of the token ending a case-arm label
// at the front of tokens — the `)` of `a)` or of `( a|b )` — or -1 when no
// label is there.
//
// `;;` ends an arm and its two `;` tokens reach the caller as separators, so
// after the first arm every segment opens with its own label. A glued label
// (`a)`) arrives as one token; the tokenizer spaces a parenthesised one
// (`(a)`), so its `)` arrives alone after the `(` opener has already been
// dropped. Both are found by scanning to the first token that ends in `)` —
// the shape test doneSlotLoopIsCaseArmPattern applies to the same labels
// (gt-vjrpj).
//
// Two shapes must not read as a label. A group's own `)` is the last token of
// its segment — everything behind a group is a separator, which splits the
// segment — so only a `)` with words after it closes a label. And a bd command
// word already in command position means the segment is a subshell that HOLDS
// the close (`( bd close gt-x ) 2>/dev/null`), not an arm label in front of it
// — stripping to that `)` there would hide the close. A token carrying an
// opening paren is a function definition or a substitution, so it opens no
// label either.
func bdCloseCaseArmLabelEnd(tokens []string) int {
	for i, tok := range tokens {
		if isBdCommandWord(tok) {
			return -1
		}
		if !strings.HasSuffix(tok, ")") || strings.Contains(tok, "(") {
			continue
		}
		if i == len(tokens)-1 {
			return -1
		}
		return i
	}
	return -1
}

// bdCloseUnglueArmLabel splits a case-arm label glued to its command word
// (`a)bd`, `*)bd`) at the front of a segment inside a case statement, so the
// command word stands alone for the matchers that follow. Bash accepts `case
// a in a)echo hit;; esac`, and spaceOutShellOperators leaves a case pattern's
// `)` attached to the word behind it, so the segment's command word arrived as
// `a)bd` and the close behind it went unjudged (gt-5toy9). Only the first `)`
// ends the label, and a token with `(` before it is a substitution or function
// definition, not a label. A label with nothing behind it (`a)`) is
// bdCloseCaseArmLabelEnd's. The caller passes only segments inside a case, so
// a word like `a)bd` in an ordinary command is never split.
func bdCloseUnglueArmLabel(segment []string) []string {
	start := 0
	for start < len(segment) {
		tok := segment[start]
		switch {
		case bdCloseLeadWords[tok]:
			start++
			continue
		case tok == "case":
			// Skip the `case WORD in` header; no `in` leaves the segment alone.
			end := bdCloseCaseHeaderEnd(segment[start:])
			if end < 0 {
				return segment
			}
			start += end
			continue
		}
		break
	}
	if start >= len(segment) {
		return segment
	}
	i := strings.Index(segment[start], ")")
	if i <= 0 || i == len(segment[start])-1 || strings.ContainsAny(segment[start][:i], "(`") {
		return segment
	}
	out := append([]string{}, segment[:start]...)
	out = append(out, segment[start][:i+1], segment[start][i+1:])
	return append(out, segment[start+1:]...)
}

// bdCloseCaseDepthChange returns how far a segment moves the case-statement
// depth: +1 when it opens with a `case` header, -1 when its command word is
// `esac`. Only the command position counts, so `echo case` and `echo esac`
// leave the depth alone.
func bdCloseCaseDepthChange(segment []string) int {
	tokens := stripLeadingEnvAssignments(segment)
	for len(tokens) > 0 && bdCloseLeadWords[tokens[0]] {
		tokens = tokens[1:]
	}
	switch {
	case bdCloseCaseHeaderEnd(tokens) >= 0:
		return 1
	case len(tokens) > 0 && tokens[0] == "esac":
		return -1
	}
	return 0
}

// parseBdCloseShellPayload reads the closes inside a `bash -c '...'` payload.
func parseBdCloseShellPayload(segment []string) []bdCloseInvocation {
	tokens := bdCloseCommandTokens(segment)
	// eval joins its arguments with spaces and runs the result as a command
	// line, so `eval 'bd close gt-x'` is a close.
	if len(tokens) >= 2 && tokens[0] == "eval" {
		return parseBdCloseInvocations(strings.Join(tokens[1:], " "))
	}
	if len(tokens) < 3 || !shellInvokers[filepath.Base(tokens[0])] {
		return nil
	}
	i := shellCPayloadIndex(tokens[1:])
	if i < 0 {
		return nil
	}
	return parseBdCloseInvocations(tokens[1+i])
}

// stripLeadingEnvAssignments drops leading VAR=value tokens (and the
// "env"/"command" prefixes that can precede them) so `FOO=1 bd close gt-x` is
// recognized. env may be a path (/usr/bin/env) and carry its own flags, so
// `env -i bd close gt-x` and `env -u FOO bd close gt-x` are recognized too;
// env's flags are only skipped, never read. An assignment is only stripped from
// the front, so an argument later in the command that merely contains "=" is
// left alone. Reuses isEnvAssignment from tap_guard_polecat_paths.go, which
// draws the same NAME=value distinction for the same reason.
func stripLeadingEnvAssignments(tokens []string) []string {
	for len(tokens) > 0 {
		tok := tokens[0]
		if filepath.Base(tok) == "env" {
			tokens = bdCloseSkipEnvFlags(tokens[1:])
			continue
		}
		if tok == "command" {
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

// bdCloseEnvValueFlags are env's flags that consume the next token, so the
// value is not left behind as the command word (`env -u FOO bd close gt-x`).
var bdCloseEnvValueFlags = map[string]bool{
	"-u": true, "--unset": true, "-C": true, "--chdir": true, "-P": true,
	"-S": true, "--split-string": true,
}

// bdCloseSkipEnvFlags drops env's own flags (and the values they take) from the
// front of tokens, the part of the command line after the word env.
func bdCloseSkipEnvFlags(tokens []string) []string {
	for len(tokens) > 0 && strings.HasPrefix(tokens[0], "-") {
		flag := tokens[0]
		tokens = tokens[1:]
		if flag == "--" {
			break
		}
		if bdCloseEnvValueFlags[flag] && len(tokens) > 0 {
			tokens = tokens[1:]
		}
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
	fmt.Fprintln(os.Stderr, "║  A bead with no commits on this branch closes normally; the     ║")
	fmt.Fprintln(os.Stderr, "║  landing worker closes submitted work when it lands.            ║")
	fmt.Fprintln(os.Stderr, "║  Otherwise submit with 'gt done', or record an operator reason: ║")
	fmt.Fprintln(os.Stderr, "║    gt bead close <id> --reason=\"supersede: <what replaced it>\"   ║")
	fmt.Fprintln(os.Stderr, "║    gt bead close <id> --reason=\"cancel: <why it is abandoned>\"  ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  See: gt-6hmz (close-time invariant), gt-arno (this guard)       ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
