package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardCmd = &cobra.Command{
	Use:   "guard",
	RunE:  requireSubcommand,
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
  container-suite    - Block unwrapped go test/make test on testcontainers-backed packages
  polecat-paths      - Block Edit/Write/Bash targets outside the polecat's own worktree
  bd-close-invariant - Block raw bd close of a bead whose branch carries unmerged work
  question-tool      - Deny AskUserQuestion in a session with nobody at the pane
  permission-request - Deny a permission prompt an unattended session cannot answer (PermissionRequest hook, exit 0)

External guards (standalone scripts, not compiled into gt):
  context-budget   - scripts/guards/context-budget-guard.sh

Example hook configuration (matcher is the TOOL NAME only, a shell guard's
being "Bash|Monitor" — a bare "Bash" lets a command run through the
Monitor tool reach the guard's target unguarded, gt-vx2mm; a command
pattern like "Bash(gh pr create*)" in "matcher" never fires, gt-5ihs —
and no "if" either: Claude Code's "if" evaluator matches ANY pattern for a
command it cannot statically resolve, so a deny guard must read
tool_input.command off stdin and self-filter, gt-3mp1):
  {
    "PreToolUse": [{
      "matcher": "Bash|Monitor",
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

One exemption narrows the first scenario:
  - a polecat creating a local session branch inside its own worktree
    (gt-6hg7) — the escape route for a polecat that resumed a branch whose
    push had already gone out, where gt done's recoverDivergedPush refuses
    the force-push and the alternative this message used to offer (push to
    main) is itself blocked for polecats (gt-ibt8).
"gh pr create" stays blocked for every role, the exemption included.

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

// shellCommandSeparators are shell operators that start a new command
// within a single line. Claude Code evaluates Bash permission rules
// per sub-command, so its "if" glob can fire on a later segment of a
// compound command (cd x && gh pr create ...) — this guard must inspect
// each ;/&/&&/||/| segment independently rather than only the start of the
// whole line, or a leading unrelated segment lets a real PR-workflow
// command later on the line slip through (gt-wisp-52y4). An unquoted
// newline separates commands the same way and arrives here as ";" — see
// spaceOutShellOperators (gt-3j8u).
//
// A background & belongs here with the others: it runs the following command
// just as ; does, so leaving it out let a text-only command in front of it
// speak for the real command behind — "echo hi & git clean -f" was judged a
// single echo call and the clean ran unchecked (gt-wwwht).
//
// A "}" closing a brace group belongs here too, because the ";" that ends the
// group's last command comes before it: the group's words would otherwise ride
// into the following segment as "}"-prefixed tokens, hiding that segment's
// command from every matcher keyed on its first words, and the walk would read
// the group's cd as continuing an argument list (gt-ajyw8). The "{" that opens
// the group is not a separator here — it must stay in the group's segment — so
// shellCommandStart names it instead.
var shellCommandSeparators = map[string]bool{
	"&&": true,
	"||": true,
	";":  true,
	"|":  true,
	"&":  true,
	"}":  true,
}

// maxPRWorkflowNestDepth bounds the payload recursion in
// prWorkflowInvocations, mirroring maxDangerousNestDepth: a payload can itself
// hold a payload, and without a cap that nesting could recurse as deep as the
// input text allows.
const maxPRWorkflowNestDepth = 3

// matchesPRWorkflowCommand reports whether command runs gh pr create,
// git checkout -b or git switch -c. The hook is registered without an "if"
// glob and self-filters here, so a command that runs none of the three is
// allowed and not every Bash call in agent context is blocked (gt-pjeh).
//
// Each shell segment is judged by the command it runs, not by its first words
// (prWorkflowInvocations), so an env assignment, a launcher, a group opener, a
// path-qualified binary, gh's -R flag or a bash -c or eval payload cannot
// hide it (gt-zf2vy). Words are compared whole, so "git checkout -branch"
// does not match "git checkout -b", and quoted text is never a command word.
//
// Heredoc bodies are data, not shell syntax, so they are stripped before
// tokenizing (gt-3j8u): a body line that spells "gh pr create" would
// otherwise block an ordinary file write. A body fed to a shell invoker is
// the exception, because that shell runs it (gt-9g0y, gt-q91m).
func matchesPRWorkflowCommand(command string) bool {
	invocations, tooDeep := prWorkflowInvocations(command, 0)
	if tooDeep {
		return true
	}
	for _, argv := range invocations {
		if isGhPRCreate(argv) || isBranchCreation(argv) {
			return true
		}
	}
	return false
}

// prWorkflowLauncher describes a command that runs the command behind it.
// valueFlags are its own flags whose value is the next word, and positionals
// counts the operands before the command (timeout's DURATION).
type prWorkflowLauncher struct {
	valueFlags  map[string]bool
	positionals int
}

// prWorkflowLaunchers are the commands prWorkflowCommand looks through:
// segmentCommandWord's launchers plus timeout.
var prWorkflowLaunchers = map[string]prWorkflowLauncher{
	"env":     {valueFlags: map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true}},
	"time":    {valueFlags: map[string]bool{"-f": true, "--format": true, "-o": true, "--output": true}},
	"command": {},
	"nohup":   {},
	"builtin": {},
	"exec":    {valueFlags: map[string]bool{"-a": true}},
	"nice":    {valueFlags: map[string]bool{"-n": true, "--adjustment": true}},
	"stdbuf":  {valueFlags: map[string]bool{"-i": true, "-o": true, "-e": true}},
	"sudo": {valueFlags: map[string]bool{
		"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-r": true,
		"-t": true, "-T": true, "-U": true, "-D": true,
	}},
	"timeout": {valueFlags: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}, positionals: 1},
}

// prWorkflowCommand returns the command a shell segment runs, from its command
// word on. It looks through the group openers and control keywords in front
// (trimShellKeywords), leading NAME=value assignments, and launchers with
// their flags. payloads holds the command line a launcher itself carries, as
// in env -S 'gh pr create'. argv is empty when nothing runs.
func prWorkflowCommand(segment []string) (argv, payloads []string) {
	seg := trimShellKeywords(segment)
	for len(seg) > 0 {
		word := seg[0]
		if isEnvAssignment(word) || word == "!" {
			seg = seg[1:]
			continue
		}
		name := strings.ToLower(filepath.Base(word))
		launcher, ok := prWorkflowLaunchers[name]
		if !ok {
			return seg, payloads
		}
		var payload string
		seg, payload = skipLauncherArgs(name, launcher, seg[1:])
		if payload != "" {
			payloads = append(payloads, payload)
		}
	}
	return nil, payloads
}

// skipLauncherArgs drops the flags and operands a launcher takes ahead of the
// command it runs and returns the words from that command on. "--" ends the
// flags. env -S takes a whole command line as its value, returned as payload.
func skipLauncherArgs(name string, launcher prWorkflowLauncher, rest []string) (seg []string, payload string) {
	i := 0
	for i < len(rest) {
		word := rest[i]
		if word == "--" {
			i++
			break
		}
		if len(word) < 2 || word[0] != '-' {
			break
		}
		if name == "env" {
			switch {
			case word == "-S" || word == "--split-string":
				if i+1 < len(rest) {
					payload = rest[i+1]
				}
				i += 2
				continue
			case strings.HasPrefix(word, "--split-string="):
				payload = strings.TrimPrefix(word, "--split-string=")
				i++
				continue
			case strings.HasPrefix(word, "-S"):
				payload = word[2:]
				i++
				continue
			}
		}
		if launcher.valueFlags[word] {
			i += 2
		} else {
			i++
		}
	}
	i += launcher.positionals
	if i > len(rest) {
		i = len(rest)
	}
	return rest[i:], payload
}

// prWorkflowInvocations returns the command every segment of command runs, as
// argv from the command word on, plus the commands inside every payload a
// shell runs: a shell invoker's -c string, an eval's joined arguments, an
// env -S string, the body of a heredoc fed to a shell, and the body of every
// $(...) or backtick substitution the shell expands. A payload is read only
// where a shell runs it, so "echo bash -c 'gh pr create'" is an echo, while
// PR_URL=$(gh pr create --fill) runs the gh inside it (gt-z9asr).
//
// A payload past maxPRWorkflowNestDepth is not read; tooDeep is set when it
// still names gh or git, since a script the guard cannot reach is not one that
// runs nothing.
func prWorkflowInvocations(command string, depth int) (invocations [][]string, tooDeep bool) {
	tokens := shellTokenize(stripHeredocBodies(strings.TrimSpace(command)))

	var payloads []string
	for _, segment := range splitShellSegments(tokens) {
		argv, launched := prWorkflowCommand(segment)
		payloads = append(payloads, launched...)
		if len(argv) == 0 {
			continue
		}
		invocations = append(invocations, argv)
		name := strings.ToLower(filepath.Base(argv[0]))
		switch {
		case shellInvokers[name]:
			if idx := shellCPayloadIndex(toLowerTokens(argv[1:])); idx >= 0 {
				payloads = append(payloads, argv[1+idx])
			}
		case name == "eval" && len(argv) > 1:
			payloads = append(payloads, strings.Join(argv[1:], " "))
		}
	}
	payloads = append(payloads, shellFedHeredocBodies(command)...)
	payloads = append(payloads, prWorkflowSubstitutions(command)...)

	for _, payload := range payloads {
		if depth >= maxPRWorkflowNestDepth {
			tooDeep = tooDeep || prWorkflowNamesGhOrGit.MatchString(payload)
			continue
		}
		nested, deep := prWorkflowInvocations(payload, depth+1)
		invocations = append(invocations, nested...)
		tooDeep = tooDeep || deep
	}
	return invocations, tooDeep
}

// prWorkflowSubstitutions returns the body of every command substitution the
// shell expands in command: the ones in the command text, and the ones in the
// body of an unquoted heredoc, which the shell expands while reading it. A
// "$(" the shell leaves literal is skipped, after a backslash and inside single
// quotes, so a note spelling '$(gh pr create)' stays data.
//
// A $(...) body runs to its balancing ")", so parentheses inside it, whether a
// nested $(date) or a quoted "fix (x)", do not end it early. A substitution
// nested in a body is not returned here: the recursion in prWorkflowInvocations
// scans the body for its own. commandSubstitutionPattern cannot do this, since
// it matches only bodies free of parentheses and would let
// $(gh pr create --title "$(date)") through.
func prWorkflowSubstitutions(command string) []string {
	text := stripHeredocBodies(command)
	var bodies []string
	add := func(body string) {
		if strings.TrimSpace(body) != "" {
			bodies = append(bodies, body)
		}
	}
	var quote byte
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case c == '$' && i+1 < len(text) && text[i+1] == '(':
			end := parenBodyEnd(text, i+2)
			add(text[i+2 : end])
			i = end
		case c == '`':
			end := backtickBodyEnd(text, i+1)
			add(strings.ReplaceAll(text[i+1:end], "\\`", "`"))
			i = end
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == '"' && c == '"':
			quote = 0
		}
	}
	return append(bodies, heredocBodySubstitutions(command)...)
}

// parenBodyEnd returns the index of the ")" that closes the $( whose body
// starts at start, or len(text) when none does, so an unterminated body is read
// to its end rather than dropped. Parentheses inside quotes are text.
func parenBodyEnd(text string, start int) int {
	depth := 1
	var quote byte
	for i := start; i < len(text); i++ {
		switch c := text[i]; {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\':
			i++
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == '"' && c == '"':
			quote = 0
		case quote != 0:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(text)
}

// backtickBodyEnd returns the index of the backtick that closes the one whose
// body starts at start, or len(text) when none does. A backslash escapes the
// next character, so an escaped backtick in the body does not end it; it opens
// a substitution nested in the body, which prWorkflowSubstitutions unescapes.
func backtickBodyEnd(text string, start int) int {
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '`':
			return i
		}
	}
	return len(text)
}

// prWorkflowNamesGhOrGit finds gh or git as a word of its own, path-qualified
// or not, so "github" and "gh-pages" are not mistaken for the commands.
var prWorkflowNamesGhOrGit = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])(gh|git)([^A-Za-z0-9_.-]|$)`)

// ghOptionsWithValue are the gh flags that take the next word as their value
// when given separately (-R o/r); the glued forms (-Ro/r, --repo=o/r) are one
// word.
var ghOptionsWithValue = map[string]bool{"-R": true, "--repo": true}

// ghPositionals returns the first two non-flag words after gh. Flags are
// skipped wherever they sit, as cobra reads them: gh -R o/r pr create and
// gh pr create -R o/r are the same command.
func ghPositionals(args []string) []string {
	var words []string
	for i := 0; i < len(args) && len(words) < 2; i++ {
		switch word := args[i]; {
		case ghOptionsWithValue[word]:
			i++
		case strings.HasPrefix(word, "-"):
		default:
			words = append(words, word)
		}
	}
	return words
}

// isGhPRCreate reports whether argv, a command from its command word on, is
// gh pr create. gh may be a path.
func isGhPRCreate(argv []string) bool {
	if len(argv) == 0 || !strings.EqualFold(filepath.Base(argv[0]), "gh") {
		return false
	}
	words := ghPositionals(argv[1:])
	return len(words) == 2 && strings.EqualFold(words[0], "pr") && strings.EqualFold(words[1], "create")
}

// isBranchCreation reports whether argv, a command from its command word on,
// is git checkout -b or git switch -c. git may be a path and may carry global
// options (-C dir, -c k=v) in front of the subcommand.
func isBranchCreation(argv []string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "git" {
		return false
	}
	rest := argv[1:]
	at := gitSubcommandIndex(rest)
	if at < 0 || at+1 >= len(rest) {
		return false
	}
	switch rest[at] {
	case "checkout":
		return rest[at+1] == "-b"
	case "switch":
		return rest[at+1] == "-c"
	}
	return false
}

func runTapGuardPRWorkflow(cmd *cobra.Command, args []string) error {
	// All three "if"-matched hook patterns (gh pr create*, git checkout -b*,
	// git switch -c*) invoke this same command with no argument telling us
	// which one fired, so the actual command must be read back off stdin
	// (Claude Code hook protocol).
	return tapGuardPRWorkflow(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardPRWorkflow is the pr-workflow guard: it reads the hook payload
// from stdin and the session from proc, and prints a block to stderr.
func tapGuardPRWorkflow(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
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

	switch evaluatePRWorkflowGuard(input, proc) {
	case prWorkflowBlockAgentContext:
		printPRWorkflowAgentContextBlock(stderr)
		return NewSilentExit(2) // Exit 2 = BLOCK in Claude Code hooks
	case prWorkflowBlockMaintainerOrigin:
		printPRWorkflowMaintainerOriginBlock(stderr)
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
func evaluatePRWorkflowGuard(input []byte, proc guardProcess) prWorkflowGuardDecision {
	command := extractCommand(input)

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
	//   - !isPRCreateCommand() — "git checkout -b x && gh pr create" stays
	//     blocked for a polecat: a polecat has no reason to pair the two, and
	//     the PR path is exactly what this guard is for.
	//   - isInOwnPolecatWorktree() — the worktree is the thing a session
	//     branch is being created for. A polecat that has wandered into a
	//     sibling's worktree (the gt-hmaf cross-worktree class) or the rig
	//     root stays blocked. This also keeps the doctor's live-fire probe
	//     honest: it runs with GT_POLECAT=live-fire, GT_POLECAT_PATH stripped
	//     and cwd in a /tmp sandbox, so a bare role check would let its
	//     blocked-shape 'git checkout -b' through and read that as broken
	//     matcher wiring (gt-xy4b). Last because it is the only condition
	//     that touches the filesystem.
	if isPolecatSession(proc) && isLeadingBranchCreation(command) &&
		!isPRCreateCommand(command) && isInOwnPolecatWorktree(input, proc) {
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
	if isGasTownAgentContextIn(proc.getenv, proc.getwd) {
		return prWorkflowBlockAgentContext
	}

	// Check if origin is the maintainer's repo (steveyegge/gastown)
	if isMaintainerOrigin(proc) {
		return prWorkflowBlockMaintainerOrigin
	}

	// Not in Gas Town context and not maintainer origin - allow PRs
	return prWorkflowAllow
}

func printPRWorkflowAgentContextBlock(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ PR WORKFLOW BLOCKED                                          ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(w, "║  Gas Town workers push directly to main. PRs are forbidden.     ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Instead of:  gh pr create / git checkout -b / git switch -c    ║")
	fmt.Fprintln(w, "║  Do this:     git add . && git commit && git push origin main   ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  In your own polecat worktree, creating a session branch is     ║")
	fmt.Fprintln(w, "║  allowed: git checkout -b <branch> / git switch -c <branch>.    ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Why? PRs add friction that breaks autonomous execution.        ║")
	fmt.Fprintln(w, "║  See: ~/gt/docs/PRIMING.md (GUPP principle)                     ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "")
}

func printPRWorkflowMaintainerOriginBlock(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ PR BLOCKED - MAINTAINER ORIGIN                               ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(w, "║  Your origin is steveyegge/gastown - push directly to main.     ║")
	fmt.Fprintln(w, "║  PRs are for external contributors, not maintainers.            ║")
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Instead of:  gh pr create                                      ║")
	fmt.Fprintln(w, "║  Do this:     git push origin main                              ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "")
}

// isGasTownAgentContext returns true if we're running as a Gas Town managed agent.
func isGasTownAgentContext() bool {
	return isGasTownAgentContextIn(os.Getenv, os.Getwd)
}

// isGasTownAgentContextIn is isGasTownAgentContext reading the environment
// through getenv and the working directory through getwd.
func isGasTownAgentContextIn(getenv func(string) string, getwd func() (string, error)) bool {
	// Check environment variables set by Gas Town session management
	envVars := []string{
		"GT_POLECAT",
		"GT_CREW",
		"GT_WITNESS",
		"GT_REFINERY",
		"GT_DEACON",
		"GT_DOG_NAME",
	}
	for _, env := range envVars {
		if getenv(env) != "" {
			return true
		}
	}

	// Also check if we're in a crew or polecat worktree by path
	cwd, err := getwd()
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
func isMaintainerOrigin(proc guardProcess) bool {
	url, err := proc.originURL()
	if err != nil {
		return false
	}
	// Match both HTTPS and SSH URL formats:
	// - https://github.com/steveyegge/gastown.git
	// - git@github.com:steveyegge/gastown.git
	return strings.Contains(url, "steveyegge/gastown")
}

// isPolecatSession reports whether the current process is running as a
// polecat, by the same GT_ROLE-first rule `gt sling`, `gt hook` and
// `gt handoff` use: a coordinator session may carry a stale GT_POLECAT in
// its environment from having spawned polecats, so GT_ROLE decides whenever
// it is set and GT_POLECAT is only the fallback.
func isPolecatSession(proc guardProcess) bool {
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		return isPolecatRole(role)
	}
	return proc.getenv("GT_POLECAT") != ""
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
func isInOwnPolecatWorktree(input []byte, proc guardProcess) bool {
	scope, ok := resolvePolecatPathScope(payloadCwd(input), proc)
	if !ok {
		return false
	}
	return scope.isWorktreePath(scope.cwd)
}

// isPRCreateCommand reports whether command runs "gh pr create" — the one
// pr-workflow pattern that stays blocked for every role. It reads the same
// invocations matchesPRWorkflowCommand does, so a gh pr create glued after an
// "&&" (gt-cyz8) or behind a wrapper (gt-zf2vy) cannot chain past the
// exemption that consults this check; a payload too deep to read counts.
func isPRCreateCommand(command string) bool {
	invocations, tooDeep := prWorkflowInvocations(command, 0)
	if tooDeep {
		return true
	}
	for _, argv := range invocations {
		if isGhPRCreate(argv) {
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
// The one exemption the pr-workflow guard grants is keyed on this shape: a
// polecat's fresh session branch (gt-6hg7), scoped to its own worktree.
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
	// The leading command is the one the first segment runs, so an env
	// assignment or launcher in front of git (the shapes the matcher now
	// reads, gt-zf2vy) keeps the exemption it had when they hid the command.
	seg, _ := prWorkflowCommand(segs[0])
	if len(seg) < 2 || !strings.EqualFold(filepath.Base(seg[0]), "git") {
		return false
	}
	lower := make([]string, len(seg))
	for i, tok := range seg {
		lower[i] = strings.ToLower(tok)
	}
	return (containsToken(lower, "checkout") || containsToken(lower, "switch")) &&
		(containsToken(lower, "-b") || containsToken(lower, "-c"))
}

func containsToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}
