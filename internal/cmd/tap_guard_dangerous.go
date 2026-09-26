package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemon"
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
  - git push to main/master from a polecat session (GT_ROLE naming a polecat
    role, or GT_POLECAT_PATH set when GT_ROLE is unset):
    HEAD:main, <sha>:main, :main, refs/heads/main, main, --all, --mirror.
    Polecat work lands through gt done -> MR -> Refinery (gt-ibt8). A release
    or a manual plugin run pushes main legitimately; both belong to a
    crew/mayor/refinery session, not a polecat one (gt-deff).
  - git reset --hard
  - git reset <remote-tracking-ref>  (--soft/--mixed/--hard/implicit: resetting
    onto origin/main etc. reverts everything merged since the checkout was cut
    and, for the tree-carrying modes, destroys uncommitted work — see gt-63sz)
  - git clean with a force flag: -f, -fd, -fdx
  - drop table/database
  - truncate table
  - find/bfs/fd/rg/grep -r/du/ls -R rooted at /, ~, $HOME, /Users, /System,
    /Library, or /opt (see gt-nqcy — an unbounded 'bfs /' froze a host)
  - the same walkers rooted at the town tree: the town root, any rig root,
    any path directly under the town root, a rig's worktree directories
    (polecats/crew/refinery/witness/mayor), or any .repo.git — see gt-6e2l,
    where a dog's 'grep -R ... /Users/sloan/gt' ran unblocked and walked
    every rig and every worktree on the host. A path inside a single repo or
    worktree (e.g. ~/gt/<rig>/polecats/<name>/<repo>) is still allowed, as is
    a search pattern that shares a name with a directory there: 'grep -rn
    polecats ./docs' searches ./docs, not the polecats/ directory at cwd
    (gt-yts7).
  - go clean -cache/-testcache/-modcache/-fuzzcache (wipes the Go build
    cache SHARED by every agent on the host — see gt-nqcy follow-up).
  - a loop or watcher around 'gt done' or 'gt slot': a for/while/until block
    whose body invokes either, an xargs/watch/seq beside either, or a heredoc
    body written to a file that contains either shape. 'gt done' waits for the
    container-gate slot itself and is bounded; the improvised polling loop it
    replaces held the gate everyone else was queued behind (gt-7dxw). Every
    role, every cwd.

This guard also HOLDS (rather than permanently blocks) a full-suite start —
'make test', 'go test ./...', 'make build', 'go build ./...' — when the host
is already busy (CPU idle below 25 percent, sampled via 'top'), unless the
command is wrapped in 'gt slot run'. Re-running the exact same command after
a short wait passes once the host frees up. See gt-nqcy follow-up.

The guard reads the tool input from stdin (Claude Code hook protocol)
and exits with code 2 to block or hold an operation.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED or HELD`,
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

	// The town root is resolved once per hook run and threaded down through
	// the recursion, so a nested payload (bash -c "grep -r x /Users/me/gt")
	// is judged against the same town tree as the top-level command
	// (gt-6e2l). "" means "not inside a town" — see currentTownRoot.
	if reason, alternative := evaluateDangerousCommand(command, 0, currentTownRoot()); reason != "" {
		if alternative != "" {
			printDangerousBlockWithAlternative(reason, command, alternative)
		} else {
			printDangerousBlock(reason, command)
		}
		return NewSilentExit(2)
	}

	if load1, held := evaluateIdleGate(command); held {
		printIdleGateHold(load1, command)
		return NewSilentExit(2)
	}

	return nil
}

// evaluateIdleGate reports whether command is a full-suite start
// (isIdleGatedSuiteStartCommand) that should be held because the sampled
// 1-minute load average is above idleGateLoad1Threshold. load1 is the
// sampled value (0 when the command isn't gated or the sample failed — a
// failed sample fails open, never holding the command).
func evaluateIdleGate(command string) (load1 float64, held bool) {
	if !isIdleGatedSuiteStartCommand(command) {
		return 0, false
	}
	load1, ok := hostLoad1()
	if !ok {
		return 0, false
	}
	return load1, load1 > idleGateLoad1Threshold
}

// maxDangerousNestDepth bounds nestedCommands recursion so a pathological
// input (e.g. deeply chained "eval eval eval ...") can't loop unboundedly.
const maxDangerousNestDepth = 3

// evaluateDangerousCommand runs every dangerous-pattern check against
// command and, up to maxDangerousNestDepth, recurses into any shell command
// it finds embedded as an argument (bash -c/sh -c/eval), as a command
// substitution ($(...) / `...`), or as a heredoc body fed to a shell
// (bash <<EOF). Without this, a wrapper like `bash -c "git reset --hard"`
// was invisible to every check: shlex collapses the quoted payload into a
// single token, so none of the fragment-based matchers (which require each
// fragment as its own token) ever fire on it. Quoted text that is NOT one of
// these shell-executing forms (a SQL string, a mail body, a jq/sed script)
// deliberately stays opaque — that is where this guard's real false
// positives have come from (mayor scope, gt-5ihs attempt 2, gt-wisp-db27
// finding 4).
func evaluateDangerousCommand(command string, depth int, townRoot string) (reason, alternative string) {
	// Read the shell-fed bodies off the untouched command: stripHeredocBodies
	// removes them from the text scanned below, and they come back in as
	// nested commands of their own (gt-9g0y).
	shellFedBodies := shellFedHeredocBodies(command)
	// Kept for the checks that need the pre-strip text: the done/slot loop
	// rule reads heredoc bodies the command writes to a FILE, which the
	// stripping below is exactly what removes from view (gt-7dxw).
	rawCommand := command
	command = stripHeredocBodies(command)
	tokens := shellTokenize(command)
	lowerTokens := make([]string, len(tokens))
	for i, t := range tokens {
		lowerTokens[i] = strings.ToLower(t)
	}

	// Check privilege escalation and package manager commands first
	if r := matchesSudo(lowerTokens); r != "" {
		return r, ""
	}
	if r := matchesPackageInstall(lowerTokens); r != "" {
		return r, ""
	}
	// pacman needs case-sensitive flag inspection (-S installs, -Ss/-Qs only
	// search) that lowerTokens has already destroyed — see matchesPacmanInstall.
	if matchesPacmanInstall(tokens, lowerTokens) {
		return "System package install (pacman) — use workspace tools instead", ""
	}

	// Check special patterns that need smarter matching
	if r := matchesDangerousRmRf(lowerTokens); r != "" {
		return r, ""
	}
	if r := matchesDangerousGitPush(lowerTokens); r != "" {
		return r, ""
	}
	if r, alt := matchesPolecatMainPush(lowerTokens, inPolecatSession()); r != "" {
		return r, alt
	}
	if r, alt := matchesWitnessGitPush(lowerTokens, inWitnessSession()); r != "" {
		return r, alt
	}
	if r, alt := matchesRefineryRawNotesPush(lowerTokens, isRefineryRole()); r != "" {
		return r, alt
	}
	if r, alt := matchesDangerousGitReset(lowerTokens); r != "" {
		return r, alt
	}
	if r := matchesGitClean(tokens); r != "" {
		return r, ""
	}
	if r, alt := matchesGoCleanSharedCache(lowerTokens); r != "" {
		return r, alt
	}
	// A loop or watcher around `gt done` / `gt slot` is the improvised retry
	// the slot ruling forbids (gt-7dxw). Checked against the unstripped
	// command so a heredoc-written retry script is visible.
	if r, alt := matchesDoneSlotLoop(rawCommand, tokens, lowerTokens); r != "" {
		return r, alt
	}
	// Unbounded scans need the original-case tokens: ls -R (recursive) and
	// ls -r (reverse sort) mean different things, and lowercasing would
	// collapse that distinction.
	if r, alt := matchesUnboundedScan(tokens, townRoot); r != "" {
		return r, alt
	}

	// Check simple fragment patterns
	for _, pattern := range fragmentPatterns {
		if matchesAllFragments(lowerTokens, pattern.contains) {
			return pattern.reason, ""
		}
	}

	if depth >= maxDangerousNestDepth {
		return "", ""
	}
	nested := nestedCommands(tokens, lowerTokens)
	nested = append(nested, commandSubstitutions(command)...)
	nested = append(nested, shellFedBodies...)
	for _, n := range nested {
		if r, alt := evaluateDangerousCommand(n, depth+1, townRoot); r != "" {
			return r, alt
		}
	}
	return "", ""
}

// shellInvokers are commands whose "-c" argument is itself a nested shell
// command string, not a plain argument.
var shellInvokers = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// nestedCommands extracts shell command strings embedded as arguments to
// shell-invoking wrappers (bash -c/sh -c/zsh -c/eval) so evaluateDangerousCommand
// can check their contents the same as a top-level command. tokens and
// lowerTokens must be the same length and index-aligned (see shellTokenize).
//
// "<shell> -c" takes exactly ONE command-string argument — any further
// tokens are positional parameters ($0, $1, ...) passed to that command,
// never concatenated onto it, so only tokens[i+2] is nested here. Joining
// the rest of the line (as this used to do) fabricates shell syntax that
// was never live: quoting is already gone by the time we have tokens, so
// two separately-quoted args like "echo a" and "rm -rf /" — real argv0
// and argv1 for the invoked command, not appended text — get glued into
// one string with a bare space, indistinguishable from an actual
// "echo a rm -rf /" command line (gt-mkrj). "eval", by contrast, really
// does concatenate all of its arguments with spaces before evaluating
// them as one shell command, so joining tokens[i+1:] there matches actual
// eval semantics rather than fabricating it.
func nestedCommands(tokens, lowerTokens []string) []string {
	var nested []string
	for i, lt := range lowerTokens {
		if shellInvokers[lt] && i+1 < len(lowerTokens) && lowerTokens[i+1] == "-c" && i+2 < len(tokens) {
			nested = append(nested, tokens[i+2])
		}
		if lt == "eval" && i+1 < len(tokens) {
			nested = append(nested, strings.Join(tokens[i+1:], " "))
		}
	}
	return nested
}

// heredocStartPattern matches a heredoc redirection operator: "<<TAG",
// "<<-TAG" (strip-leading-tabs form), or either with the tag quoted
// ("<<'TAG'", `<<"TAG"`).
var heredocStartPattern = regexp.MustCompile(`<<(-?)[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// heredocSpan is one heredoc redirection found by scanHeredocs: the reader
// line that owns it (the operator line with the operator removed), the body
// text, and the body's byte offsets. bodyStart is -1 when the operator line
// is the command's last line, so no body follows.
type heredocSpan struct {
	reader    string
	body      string
	bodyStart int
	bodyEnd   int
}

// scanHeredocs returns every heredoc in command, in source order. A "<<" that
// appears inside another heredoc's body is that body's data, not a second
// heredoc, so it is dropped (gt-mkrj).
func scanHeredocs(command string) []heredocSpan {
	matches := heredocStartPattern.FindAllStringSubmatchIndex(command, -1)
	if matches == nil {
		return nil
	}
	var spans []heredocSpan
	stripped := 0 // end of the last accepted body; nothing before it starts a heredoc
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < stripped {
			continue // inside a previously accepted heredoc body
		}
		allowIndent := command[m[2]:m[3]] == "-"
		tag := command[m[6]:m[7]]
		reader := heredocReaderLine(command, start, end)

		nl := strings.IndexByte(command[end:], '\n')
		if nl < 0 {
			// No body follows on a later line (e.g. the heredoc marker is
			// the last thing on the line with nothing after it).
			spans = append(spans, heredocSpan{reader: reader, bodyStart: -1, bodyEnd: end})
			stripped = end
			continue
		}
		bodyStart := end + nl + 1
		bodyEnd := heredocTerminatorEnd(command, bodyStart, tag, allowIndent)
		spans = append(spans, heredocSpan{
			reader:    reader,
			body:      command[bodyStart:bodyEnd],
			bodyStart: bodyStart,
			bodyEnd:   bodyEnd,
		})
		stripped = bodyEnd
	}
	return spans
}

// heredocReaderLine returns the text of the line the heredoc operator at
// [start,end) sits on, with the operator itself removed. The text after the
// operator stays: in "cat <<EOF | bash" the pipeline's command words, not the
// words before the operator, decide who consumes the body.
func heredocReaderLine(command string, start, end int) string {
	lineStart := strings.LastIndexByte(command[:start], '\n') + 1
	// "bash \" + newline + "<<EOF" still hands the body to bash, so walk back
	// over line continuations to keep the invoker on the reader line.
	for lineStart > 0 {
		prevEnd := lineStart - 1
		prevStart := strings.LastIndexByte(command[:prevEnd], '\n') + 1
		if !strings.HasSuffix(strings.TrimRight(command[prevStart:prevEnd], " \t"), `\`) {
			break
		}
		lineStart = prevStart
	}
	lineEnd := strings.IndexByte(command[start:], '\n')
	if lineEnd < 0 {
		lineEnd = len(command)
	} else {
		lineEnd += start
	}
	// Drop the continuations themselves: left in place they glue onto the
	// preceding word and hide it from the token matchers below.
	return strings.ReplaceAll(command[lineStart:start]+" "+command[end:lineEnd], "\\\n", " ")
}

// stripHeredocBodies removes heredoc body text from command before any
// tokenization or pattern matching runs. A heredoc body is DATA being
// written to a file or piped to a command's stdin — the same class of
// risk as a quoted SQL string or a mail body, not shell syntax to
// evaluate — so scanning it for dangerous fragments produces false
// positives: "cat > note.md <<'EOF' ... git push --force ... EOF" blocked
// an ordinary file write because the DATA it wrote happened to mention a
// dangerous phrase (gt-mkrj). Strips from just after the "<<TAG" line
// through the line that is exactly TAG (optionally tab-indented, for the
// "<<-TAG" form); text outside any heredoc span is left untouched. A body
// fed to a shell invoker is not data — see shellFedHeredocBodies.
func stripHeredocBodies(command string) string {
	spans := scanHeredocs(command)
	if len(spans) == 0 {
		return command
	}
	var b strings.Builder
	pos := 0
	for _, s := range spans {
		if s.bodyStart < 0 {
			continue
		}
		b.WriteString(command[pos:s.bodyStart])
		pos = s.bodyEnd
	}
	b.WriteString(command[pos:])
	return b.String()
}

// shellFedHeredocBodies returns the bodies of heredocs whose reader line
// names a shell invoker, because that shell runs the body as a script rather
// than reading it as data (gt-9g0y). Stripping those bodies — gt-mkrj's rule,
// which holds for every other reader — left "bash <<'EOF' ... git reset
// --hard ... EOF" inspected by nothing at all. The caller evaluates the
// returned bodies as nested commands; stripHeredocBodies still removes them
// from the surrounding text, so they are judged once, as code.
func shellFedHeredocBodies(command string) []string {
	var bodies []string
	for _, span := range scanHeredocs(command) {
		if span.bodyStart < 0 || strings.TrimSpace(span.body) == "" {
			continue
		}
		if heredocReaderIsShellInvoker(span.reader) {
			bodies = append(bodies, span.body)
		}
	}
	return bodies
}

// heredocReaderIsShellInvoker reports whether a shell invoker runs anywhere on
// the heredoc's line. "cat <<EOF | bash" feeds bash the body just as "bash
// <<EOF" does, so every pipeline segment counts — judged at command position,
// so an argument that merely spells "bash" ("echo bash <<EOF") does not.
func heredocReaderIsShellInvoker(reader string) bool {
	for _, segment := range splitShellSegments(shellTokenize(reader)) {
		word, _ := segmentCommandWord(segment)
		if word == "" {
			continue
		}
		if shellInvokers[strings.ToLower(filepath.Base(word))] {
			return true
		}
	}
	return false
}

// heredocTerminatorEnd scans command starting at bodyStart for a line
// whose content is exactly tag (leading tabs stripped first when
// allowIndent, matching the "<<-TAG" form) and returns the offset just
// past that terminator line (or len(command) if none is found, meaning
// the heredoc is unterminated and the rest of the command is body).
func heredocTerminatorEnd(command string, bodyStart int, tag string, allowIndent bool) int {
	offset := bodyStart
	for offset <= len(command) {
		lineEnd := strings.IndexByte(command[offset:], '\n')
		var line string
		var next int
		if lineEnd < 0 {
			line = command[offset:]
			next = len(command)
		} else {
			line = command[offset : offset+lineEnd]
			next = offset + lineEnd + 1
		}
		check := line
		if allowIndent {
			check = strings.TrimLeft(line, "\t")
		}
		if check == tag {
			return next
		}
		if lineEnd < 0 {
			break
		}
		offset = next
	}
	return len(command)
}

// shellVarAssignPattern matches a simple "NAME=VALUE" shell variable
// assignment token (e.g. "x=/", "FOO=bar") as produced by shellTokenize.
var shellVarAssignPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// shellVarAssignments scans tokens for leading "NAME=VALUE" assignments
// and returns a name->value map, so a later reference to "$NAME" in the
// same command line can be resolved to what it was just assigned — e.g.
// "x=/; bfs $x -name regex.h" assigns x=/ and then scans it. Without this,
// matchesUnboundedScan only ever compared literal tokens against the
// denylist, so routing the root path through a variable silently bypassed
// it (gt-mkrj).
func shellVarAssignments(tokens []string) map[string]string {
	vars := make(map[string]string)
	for _, tok := range tokens {
		if m := shellVarAssignPattern.FindStringSubmatch(tok); m != nil {
			vars[m[1]] = m[2]
		}
	}
	return vars
}

// resolveShellVar returns the value arg would expand to if it is a
// "$NAME" or "${NAME}" reference to a variable assigned earlier in the
// same command (per vars); otherwise it returns arg unchanged, preserving
// existing literal handling (e.g. the bare "$HOME" denylist entry).
func resolveShellVar(arg string, vars map[string]string) string {
	name := ""
	switch {
	case strings.HasPrefix(arg, "${") && strings.HasSuffix(arg, "}"):
		name = arg[2 : len(arg)-1]
	case strings.HasPrefix(arg, "$"):
		name = arg[1:]
	default:
		return arg
	}
	if v, ok := vars[name]; ok {
		return v
	}
	return arg
}

// commandSubstitutionPattern matches shell command substitution: $(...) or
// `...`. Scanned against the raw (untokenized) command text, since shlex
// has no notion of substitution grouping and would otherwise split
// "$(rm -rf /)" across unrelated tokens. Non-nested only (a $(...) or
// `...` containing its own nested substitution won't fully match) — good
// enough for a guard that only needs to catch realistic single-level
// wrapping, not parse arbitrary shell.
var commandSubstitutionPattern = regexp.MustCompile(`\$\(([^()]*)\)|` + "`" + `([^` + "`" + `]*)` + "`")

// commandSubstitutions extracts the inner command text of every $(...) or
// `...` command substitution in command, so evaluateDangerousCommand can
// recurse into it the same as a bash -c/eval payload (mayor scope, gt-5ihs
// attempt 2: "recurse into ... command substitution").
func commandSubstitutions(command string) []string {
	var out []string
	for _, m := range commandSubstitutionPattern.FindAllStringSubmatch(command, -1) {
		if m[1] != "" {
			out = append(out, m[1])
		} else if m[2] != "" {
			out = append(out, m[2])
		}
	}
	return out
}

// shellTokenize splits a shell command into argv-like tokens the way a real
// shell would: quoted text becomes a single opaque token instead of being
// re-split on whitespace, so a pattern living inside a quoted string (a sed
// script, a jq filter, a mail body passed to -m) is never mistaken for a
// standalone command-line argument (gt-mkrj). Falls back to naive whitespace
// splitting on malformed shell syntax (e.g. an unterminated quote) so a
// parse failure fails toward still checking real risks rather than silently
// allowing everything through.
func shellTokenize(command string) []string {
	tokens, err := shlex.Split(spaceOutShellOperators(command))
	if err != nil {
		return strings.Fields(command)
	}
	return tokens
}

// spaceOutShellOperators pads the command-chaining operators ;, &, &&, |,
// and || with spaces wherever they appear outside quotes, so shlex splits
// them into their own tokens even when glued directly to an adjacent word
// with no whitespace ("rm -rf /;echo done" has no space around ';').
// Without this, shlex — a generic word-splitter with no notion of shell
// control operators — folds the operator into whichever word touches it
// ("done;rm" as one token), hiding "rm" from every exact-token matcher
// (gt-mkrj). A doubled "&&" or "||" is emitted as a single spaced-out token
// rather than two adjacent single-character ones, so a matcher keyed on the
// whole operator (e.g. matchesPRWorkflowCommand's shellCommandSeparators)
// sees it as one token instead of two "&" or "|" tokens that never equal
// "&&"/"||" (gt-pjeh). Characters inside single/double quotes, or escaped
// with a backslash outside quotes, are left untouched so quoted content (a
// sed script containing '|', a jq filter) stays exactly as opaque as it was
// before this pass.
//
// An unquoted newline is a command separator too — the shell ends the
// command there exactly as if ';' had been typed — so it gets the same
// spaced-out ";" treatment. Without it a multi-line invocation tokenized as
// ONE segment: "cd /tmp" + newline + "gh pr create" read as a single "cd"
// call, so only the first line was ever compared against the
// command-prefix matchers and a blocked command on any later line failed
// open (gt-3j8u; the same hole hid a later line's command from
// checkBashCommand and the suite gates, which are keyed on
// shellCommandSeparators too).
//
// A backslash-newline is the opposite case — a LINE CONTINUATION, which the
// shell deletes outright so the two lines become one command — and is
// removed here, keeping the words on either side glued exactly as the shell
// would. Left raw, shlex buries the newline inside the following word and
// the joined command ("git \" + newline + "  checkout -b x") is invisible to
// those same matchers (gt-3j8u). Both rules are skipped inside single
// quotes, where a backslash is literal and a newline is just a character.
//
// A $(...) or `...` command substitution opens a quoting SCOPE of its own:
// the shell re-parses that body as a command in its own right, so a "'" that
// is literal inside a double-quoted word is a real quote again inside the
// body. With one flat quote flag the body's own '"' read as closing the
// outer word, and shlex — which has no notion of scopes either — split the
// rest of the body into standalone tokens, so a jq program's "//" alternative
// operator arrived as a bare token and read as a scan root (gt-n8ir). A
// scope opened from inside a double-quoted word therefore has its '"' and '\'
// backslashed, which keeps the whole substitution inside the one opaque token
// the enclosing quotes promise. Detection is not lost by that: quoted-text
// matchers that need the body get it from the raw text instead
// (commandSubstitutions).
func spaceOutShellOperators(command string) string {
	var b strings.Builder
	b.Grow(len(command) + 8)
	scopes := []shellQuoteScope{{}}
	escaped := false
	emit := func(r rune) {
		if scopes[len(scopes)-1].escapeForShlex && (r == '"' || r == '\\') {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	openScope := func(subst, tick bool) {
		top := scopes[len(scopes)-1]
		scopes = append(scopes, shellQuoteScope{
			subst:          subst,
			tick:           tick,
			depth:          1,
			escapeForShlex: top.escapeForShlex || top.quote == '"',
		})
	}
	closeScope := func() {
		scopes = scopes[:len(scopes)-1]
	}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		sc := scopes[len(scopes)-1]
		if r == '\\' && !escaped && sc.quote != '\'' && i+1 < len(runes) && runes[i+1] == '\n' {
			i++
			continue
		}
		if escaped {
			emit(r)
			escaped = false
			continue
		}
		// "(" and ")" nest inside a substitution body: the ')' that returns
		// the depth to zero is the one that ends it. Parens inside a quoted
		// stretch of the body are literal text, not nesting.
		paren := ""
		if sc.subst && sc.quote == 0 && (r == '(' || r == ')') {
			paren = string(r)
		}
		switch {
		case sc.quote == '\'':
			if r == '\'' {
				scopes[len(scopes)-1].quote = 0
			}
			emit(r)
		case r == '$' && sc.quote != '\'' && i+1 < len(runes) && runes[i+1] == '(':
			b.WriteString("$(")
			i++
			openScope(true, false)
		case r == '`' && sc.quote != '\'':
			if sc.tick && sc.quote == 0 {
				emit(r)
				closeScope()
			} else {
				emit(r)
				openScope(false, true)
			}
		case paren == "(":
			scopes[len(scopes)-1].depth++
			emit(r)
		case paren == ")":
			scopes[len(scopes)-1].depth--
			emit(r)
			if scopes[len(scopes)-1].depth == 0 {
				closeScope()
			}
		case sc.quote == '"':
			if r == '\\' {
				escaped = true
			} else if r == '"' {
				scopes[len(scopes)-1].quote = 0
			}
			emit(r)
		case r == '\\':
			escaped = true
			emit(r)
		case r == '\'' || r == '"':
			scopes[len(scopes)-1].quote = r
			emit(r)
		case r == '\n':
			b.WriteString(" ; ")
		case r == ';', r == '&', r == '|':
			if (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r {
				b.WriteRune(' ')
				b.WriteRune(r)
				b.WriteRune(r)
				b.WriteRune(' ')
				i++
			} else {
				b.WriteRune(' ')
				b.WriteRune(r)
				b.WriteRune(' ')
			}
		default:
			emit(r)
		}
	}
	return b.String()
}

// shellQuoteScope is one quoting scope in spaceOutShellOperators: the
// top-level command line, or the body of a command substitution, which the
// shell re-parses as a command with quoting of its own (gt-n8ir).
type shellQuoteScope struct {
	quote rune // the quote open in this scope: 0, '\'' or '"'
	subst bool // opened by "$(" — closed by the ")" matching its own depth
	depth int  // unmatched "(" seen inside a subst body
	tick  bool // opened by a backtick — closed by the next backtick
	// escapeForShlex backslashes every '"' and '\' this scope emits, so that
	// a substitution opened inside a double-quoted word stays the one opaque
	// token those quotes promise when shlex — which has no notion of scope
	// nesting — splits the result (gt-n8ir).
	escapeForShlex bool
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
	t := strings.ToLower(token)
	// A trailing slash ("~/", "/Users/", "$HOME/") names the same root as
	// the bare form but isn't in the denylist verbatim — strip it before
	// checking. The exact root "/" is left alone (nothing to strip to).
	if len(t) > 1 {
		t = strings.TrimSuffix(t, "/")
	}
	return scanRootDenylist[t]
}

// alwaysRecursiveScanTools walk a directory tree (or the whole index, for
// rg/ag) on every ordinary invocation — there's no non-recursive mode to
// distinguish, so any bare root argument is enough to flag them.
var alwaysRecursiveScanTools = map[string]bool{
	"find": true, "bfs": true, "fd": true, "du": true, "rg": true, "ag": true,
}

// scanOptionRole is the part a dash-led argument plays in locating a scan
// tool's search pattern (gt-yts7).
type scanOptionRole int

const (
	// roleBoolean is an option taking no value — including one no grammar
	// entry covers, so an unrecognized option never consumes the argument
	// after it.
	roleBoolean scanOptionRole = iota
	// rolePattern supplies the pattern as its value: `grep -e TODO ./docs`
	// searches ./docs, so no positional argument there is a pattern.
	rolePattern
	// rolePath supplies a directory to walk as its value (fd's
	// --search-path), which is a scan root like any other.
	rolePath
	// roleValue supplies a count, a file type, a glob, an encoding — a value
	// that is neither the pattern nor, in practice, a directory.
	roleValue
	// roleCount supplies a count only when the next argument is a number:
	// ag's -A/-B/-C keep any other token for the pattern (ag/src/options.c).
	roleCount
)

// scanArgGrammar is a scan tool's option grammar as it bears on which
// argument the tool reads as its search pattern (gt-yts7).
type scanArgGrammar struct {
	// flagRoles maps each option that takes a value to the role its value
	// plays. Options absent from it take no value.
	flagRoles map[string]scanOptionRole
	// listsPaths names options after which the tool has no pattern argument
	// at all (rg --files prints paths and searches for nothing).
	listsPaths []string
}

// scanGrammars is keyed by tool base name. Only the tools that take a search
// pattern appear: find, bfs, du and ls have none, so every non-flag argument
// of theirs is a path and stays judged as one.
var scanGrammars = map[string]scanArgGrammar{
	"grep": {flagRoles: mergeScanRoles(
		// The pattern options, including ugrep's additional-pattern family,
		// which also leave the positional arguments as files.
		scanRoles(rolePattern, "-e", "--regexp", "-f", "--file", "--and", "--andnot", "--not"),
		scanRoles(roleValue,
			"-A", "-B", "-C", "-m", "-d", "-D", "-J", "-K", "-g",
			"--after-context", "--before-context", "--context", "--max-count",
			"--directories", "--devices", "--jobs", "--range", "--min-line",
			"--max-line", "--glob", "--iglob", "--include", "--include-dir",
			"--include-from", "--exclude", "--exclude-dir", "--exclude-from",
			"--include-fs", "--exclude-fs", "--label", "--binary-files",
			"--encoding", "--depth"),
	)},
	"rg": {flagRoles: mergeScanRoles(
		scanRoles(rolePattern, "-e", "--regexp", "-f", "--file"),
		scanRoles(roleValue,
			"-A", "-B", "-C", "-m", "-M", "-j", "-g", "-t", "-T", "-d", "-r", "-E",
			"--after-context", "--before-context", "--context", "--max-count",
			"--max-columns", "--threads", "--glob", "--iglob", "--type",
			"--type-not", "--type-add", "--type-clear", "--max-depth",
			"--max-filesize", "--replace", "--encoding", "--engine", "--sort",
			"--sortr", "--ignore-file", "--pre", "--pre-glob", "--color",
			"--colors", "--context-separator", "--field-context-separator",
			"--field-match-separator", "--hostname-bin", "--hyperlink-format",
			"--path-separator", "--dfa-size-limit", "--regex-size-limit",
			"--generate"),
	), listsPaths: []string{"--files"}},
	"ag": {flagRoles: mergeScanRoles(
		// ag takes no pattern option: its pattern is always the first
		// positional argument, and -g/-G filter filenames instead.
		scanRoles(roleCount, "-A", "-B", "-C", "--after", "--before", "--context"),
		scanRoles(roleValue,
			"-m", "-g", "-G", "-p", "-W", "--depth", "--max-count",
			"--file-search-regex", "--ignore", "--ignore-dir", "--path-to-ignore",
			"--pager", "--workers", "--width", "--color-line-number",
			"--color-match", "--color-path"),
	)},
	"fd": {flagRoles: mergeScanRoles(
		// fd's pattern is optional but always first; -e is its extension
		// filter, and -x takes the rest of the line as another command's
		// arguments, which this grammar reads as ordinary arguments.
		scanRoles(rolePath, "--search-path", "-C", "--base-directory"),
		scanRoles(roleValue,
			"-d", "-t", "-e", "-E", "-c", "-j", "-S", "-o", "-x", "-X",
			"--max-depth", "--min-depth", "--exact-depth", "--type", "--extension",
			"--exclude", "--ignore-contain", "--ignore-file", "--color",
			"--threads", "--size", "--changed-within", "--changed-before",
			"--owner", "--path-separator", "--format", "--batch-size",
			"--max-results", "--and", "--exec", "--exec-batch",
			"--max-buffer-time"),
	)},
}

// scanRoles returns options mapped to the role their value plays.
func scanRoles(role scanOptionRole, options ...string) map[string]scanOptionRole {
	m := make(map[string]scanOptionRole, len(options))
	for _, o := range options {
		m[o] = role
	}
	return m
}

// mergeScanRoles combines option tables.
func mergeScanRoles(tables ...map[string]scanOptionRole) map[string]scanOptionRole {
	m := make(map[string]scanOptionRole)
	for _, t := range tables {
		for option, role := range t {
			m[option] = role
		}
	}
	return m
}

// isFlagToken reports whether an argument reads as an option rather than a
// positional argument: any dash-led token, a lone "-" included, which
// scanRootPath also declines to resolve as a path.
func isFlagToken(arg string) bool {
	return strings.HasPrefix(arg, "-")
}

// scanOption returns token's role for gram, and whether the argument after
// token is that option's value. A long option carries its value inline
// ("--type=go") and otherwise takes the next argument; a short option is read
// as a getopt cluster, where the letter taking a value ends the cluster
// ("-rn", "-A 3") or carries the value inline ("-A3").
func scanOption(gram scanArgGrammar, token string) (scanOptionRole, bool) {
	if !isFlagToken(token) {
		return roleBoolean, false
	}
	if strings.HasPrefix(token, "--") {
		name := token
		if eq := strings.IndexByte(token, '='); eq >= 0 {
			name = token[:eq]
		}
		role := gram.flagRoles[name]
		return role, role != roleBoolean && !strings.ContainsRune(token, '=')
	}
	letters := token[1:]
	for i := 0; i < len(letters); i++ {
		role := gram.flagRoles["-"+string(letters[i])]
		if role == roleBoolean {
			continue
		}
		return role, i == len(letters)-1
	}
	return roleBoolean, false
}

// scanPatternSlot returns the index in args of the argument tool reads as its
// search pattern, or -1 when the invocation has no pattern argument, and
// whether the invocation names a path for the scan to start from.
//
// Every other argument is left for the caller to judge as a scan root, a
// value-taking option's separate argument included. So a grammar that is
// wrong about an option re-blocks rather than spares: the pattern slot lands
// on that option's value and the pattern it displaced is judged as a path.
// The exception is a value that is itself a path the scan walks — fd's
// --search-path, -C and --base-directory — so those are all listed.
func scanPatternSlot(gram scanArgGrammar, args []string) (patternIdx int, hasRoot bool) {
	var positionals []int
	patternValue, pathValue := -1, false
	flagsDone, valueNext := false, false
	valueRole := roleBoolean
	for i, arg := range args {
		if valueNext {
			valueNext = false
			if valueRole == roleCount && !isCount(arg) {
				positionals = append(positionals, i) // ag keeps it for the pattern
				continue
			}
			switch valueRole {
			case rolePattern:
				if patternValue < 0 {
					patternValue = i
				}
			case rolePath:
				pathValue = true
			}
			continue
		}
		if !flagsDone {
			if arg == "--" {
				flagsDone = true
				continue
			}
			if isFlagToken(arg) {
				if role, next := scanOption(gram, arg); next {
					valueNext, valueRole = true, role
				}
				continue
			}
		}
		positionals = append(positionals, i)
	}

	switch {
	case patternValue >= 0:
		return patternValue, len(positionals) > 0 || pathValue
	case hasExactArg(args, gram.listsPaths...):
		return -1, len(positionals) > 0 || pathValue
	case len(positionals) > 0:
		return positionals[0], len(positionals) > 1 || pathValue
	}
	return -1, pathValue
}

// isCount reports whether token is a whole number: the shape ag's -A/-B/-C
// accept as their value.
func isCount(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// commandArgs returns the arguments of the command starting at i — its
// tokens up to the next shell command separator, the same delimiting the
// other per-command matchers use (see shellCommandSeparators, gt-wisp-52y4).
// A scan's flags and its root argument can only come from its own segment:
// run to the end of the line instead and an unrelated later command supplies
// them (gt-n8ir).
func commandArgs(tokens []string, i int) []string {
	for j := i + 1; j < len(tokens); j++ {
		if shellCommandSeparators[tokens[j]] {
			return tokens[i+1 : j]
		}
	}
	return tokens[i+1:]
}

// matchesUnboundedScan blocks find/bfs/fd/rg/ag/du, "grep -r", and "ls -R"
// invocations whose root argument is broad enough to scan the whole
// filesystem (see scanRootDenylist) or the town tree (see
// tap_guard_town_scan.go and townScanHazard). It returns a short reason for
// the fixed-width block banner and a longer alternative suggestion to print
// separately, or ("", "") if the command is fine. tokens must be
// original-case, shell-aware tokens (see shellTokenize) — quoted text (a
// sed/jq script, a mail body) must arrive as one opaque token so a "//"
// appearing inside it is never mistaken for a bare root-path argument
// (gt-mkrj).
//
// townRoot is the Gas Town root the guard resolved for this run, or "" when
// not inside a town; it is the only caller-supplied state here, so the
// host-root rule behaves identically whether or not a town exists.
func matchesUnboundedScan(tokens []string, townRoot string) (reason, alternative string) {
	fields := tokens
	vars := shellVarAssignments(tokens)
	for i, f := range fields {
		base := strings.ToLower(f)
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}

		rest := commandArgs(fields, i)
		isScan := alwaysRecursiveScanTools[base]
		if !isScan {
			switch base {
			case "grep":
				// grep's -r and -R are both "recursive" (they only differ on
				// symlinks), and either can be bundled with other short flags
				// in any order ("-rn", "-nr", "-rin"), unlike ls below where
				// case and position matter.
				isScan = hasExactArg(rest, "--recursive") || hasBundledFlagLetterFold(rest, 'r')
			case "ls":
				isScan = hasShortFlagLetter(rest, 'R')
			}
		}
		if !isScan {
			continue
		}

		// The pattern argument is not the directory that shares its spelling
		// (gt-yts7), but only where another argument names the path the walk
		// starts from. With no path argument the walker runs over cwd
		// (gt-3e6wa) and the pattern keeps its old reading, so this only ever
		// un-blocks a scan that names its root elsewhere.
		patternSkip := -1
		if gram, isPatternTool := scanGrammars[base]; isPatternTool {
			if pattern, hasRoot := scanPatternSlot(gram, rest); pattern >= 0 && hasRoot {
				patternSkip = pattern
			}
		}
		for j, arg := range rest {
			resolved := resolveShellVar(arg, vars)
			if isUnboundedScanRoot(resolved) {
				reason = fmt.Sprintf("Unbounded scan (%s rooted at %s)", base, arg)
				alternative = "Alternative: brew --prefix, pkg-config, 'go env GOROOT'/'go env GOMODCACHE', " +
					"or a search rooted inside the repo/rig instead of the whole filesystem."
				return reason, alternative
			}
			root := scanRootPath(resolved)
			if j == patternSkip {
				root = scanPatternPath(resolved)
			}
			// The expanded home directory names the same root as ~ / $HOME.
			if isHomeDirScanRoot(root) {
				reason = fmt.Sprintf("Unbounded scan (%s rooted at the home directory %s)", base, arg)
				alternative = "Alternative: search inside the repo/rig you are working in; the home " +
					"directory holds every checkout plus Documents/Desktop/Music and walking it " +
					"pegs the host and trips macOS privacy prompts."
				return reason, alternative
			}
			// The same walkers rooted at the town tree: the town root, a rig
			// root, a rig's worktree directory, or a .repo.git (gt-6e2l).
			if hazard := townScanHazard(root, townRoot); hazard != "" {
				reason = fmt.Sprintf("Unbounded scan (%s rooted at %s)", base, hazard)
				alternative = townScanAlternative
				return reason, alternative
			}
		}
	}
	return "", ""
}

// townScanAlternative is the allow-path suggestion printed under a town-tree
// block: the same shape as the host-root rule's (narrow the root until it
// names something bounded), specialised to the town's layout so the caller
// knows which part of the tree is safe to walk.
const townScanAlternative = "Alternative: scan one repo or worktree instead of the town tree — " +
	"cd into the rig or worktree you need (~/gt/<rig>/polecats/<name>/<repo>) and grep there, " +
	"or name the specific subdirectory. The town root, rig roots, rig worktree dirs, and .repo.git are off limits."

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

// hasBundledFlagLetterFold is hasShortFlagLetter but case-insensitive and
// order-independent, for tools like grep where -r and -R carry the same
// "recursive" meaning regardless of what else is bundled into the same
// short-option cluster ("-rn", "-nr", "-Rl").
func hasBundledFlagLetterFold(args []string, letter rune) bool {
	want := unicode.ToLower(letter)
	for _, a := range args {
		if len(a) < 2 || a[0] != '-' || a[1] == '-' {
			continue
		}
		for _, r := range a[1:] {
			if unicode.ToLower(r) == want {
				return true
			}
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

// matchesAllFragments returns true if every fragment is present somewhere in
// tokens (order- and position-independent). tokens must already be
// lowercased; fragments are lowercased here for callers that pass
// mixed-case literals. Word fragments (e.g. "apt", "table") must match an
// exact token — not a substring — so "apt" doesn't fire on "capture" or
// "adapt" (gt-mkrj), and shell-aware tokens (see shellTokenize) keep quoted
// text — a sed/jq script, a mail body — from being mistaken for standalone
// command words. A two-character short-flag fragment (e.g. "-f") also
// matches when bundled into a larger short-option cluster (e.g. "-fd",
// "git clean -fd"), since that's a real single-token flag combination, not
// quoted or embedded text.
func matchesAllFragments(tokens []string, fragments []string) bool {
	for _, f := range fragments {
		if !tokensContainFragment(tokens, strings.ToLower(f)) {
			return false
		}
	}
	return true
}

// tokensContainFragment deliberately does NOT look inside a quoted,
// multi-word token for word matches: mayor scope for gt-5ihs attempt 2 is
// explicit that quoted text stays opaque outside the shell-invoker
// recursion in evaluateDangerousCommand (nestedCommands) — "SQL DDL inside
// quotes is not a shell hazard; do not flag it." Every false positive this
// guard has hit (bead ids, mail bodies, a package-manager name inside an
// ordinary word) came from scanning inside quoted prose; the earlier
// word-boundary DDL fix reintroduced exactly that class of risk for SQL
// strings. Recursing into sh -c/bash -c/eval/command-substitution payloads
// is the correct, narrower fix — those really are shell commands.
func tokensContainFragment(tokens []string, want string) bool {
	for _, tok := range tokens {
		if tok == want {
			return true
		}
	}
	if len(want) == 2 && want[0] == '-' && want[1] != '-' {
		letter := rune(want[1])
		for _, tok := range tokens {
			if len(tok) > 2 && tok[0] == '-' && tok[1] != '-' && strings.ContainsRune(tok[1:], letter) {
				return true
			}
		}
	}
	return false
}

// matchesDangerousRmRf blocks "rm -rf /" targeting the root filesystem.
// Only blocks when the target is literally "/" or "/*". Normal cleanup
// commands like "rm -rf ./build/" are allowed. tokens must be lowercased,
// shell-aware tokens (see shellTokenize).
func matchesDangerousRmRf(tokens []string) string {
	hasRm := false
	hasRecursiveForce := false
	for _, f := range tokens {
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

// matchesSudo blocks any command whose argv contains a bare "sudo" token.
// Agents must never elevate privileges on the host system. tokens must be
// lowercased, shell-aware tokens (see shellTokenize).
func matchesSudo(tokens []string) string {
	for _, f := range tokens {
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
	// pacman is deliberately absent here — see matchesPacmanInstall. Its -S
	// (sync/install) and -s (search modifier, e.g. -Ss/-Qs) flags only
	// differ by case, which a lowercased fragment match can't distinguish;
	// the generic rule made "pacman -Ss foo" (a read-only search) block as
	// an install (finding 7, gt-wisp-db27).
	{[]string{"brew", "install"}, "Package install (brew) — use workspace tools instead"},
	{[]string{"gem", "install"}, "System gem install — use workspace tools instead"},
}

// matchesPacmanInstall reports whether tokens invoke pacman's sync/install
// operation (-S, capital — bare or bundled like -Sy/-Syu) rather than a
// read-only query or search. A generic case-insensitive bundled-flag match
// can't tell these apart: pacman -Ss (sync+search) and pacman -Qs
// (query+search) both contain the letter 's', but neither installs
// anything — only -S without a lowercase 's' modifier does. tokens must be
// original-case (for the flag) and lowerTokens lowercased and index-aligned
// (to find "pacman" case-insensitively), both from shellTokenize.
func matchesPacmanInstall(tokens, lowerTokens []string) bool {
	sawPacman := false
	for i, lt := range lowerTokens {
		if lt == "pacman" {
			sawPacman = true
			continue
		}
		if !sawPacman {
			continue
		}
		t := tokens[i]
		if len(t) < 2 || t[0] != '-' || t[1] == '-' {
			continue
		}
		if strings.ContainsRune(t, 'S') && !strings.ContainsRune(t, 's') {
			return true
		}
	}
	return false
}

// matchesPackageInstall blocks system package manager install commands.
// Also blocks "pip install" with --system flag and "npm install -g" (global
// installs). tokens must be lowercased, shell-aware tokens (see
// shellTokenize) — token-exact matching keeps a fragment like "apt" from
// firing on "capture"/"adapt" and keeps quoted text out of consideration
// (gt-mkrj).
func matchesPackageInstall(tokens []string) string {
	// Check simple token-based patterns (apt install, dnf install, etc.)
	for _, p := range packageManagerPatterns {
		if matchesAllFragments(tokens, p.tokens) {
			return p.reason
		}
	}

	hasToken := func(want string) bool {
		for _, t := range tokens {
			if t == want {
				return true
			}
		}
		return false
	}

	// pip install --system (but not regular pip install into a venv)
	if (hasToken("pip") || hasToken("pip3")) && hasToken("install") && hasToken("--system") {
		return "System-level pip install — use a virtualenv or workspace tools instead"
	}

	// npm install -g / npm install --global
	if hasToken("npm") && hasToken("install") && (hasToken("-g") || hasToken("--global")) {
		return "Global npm install — use workspace tools instead"
	}

	return ""
}

// matchesDangerousGitPush blocks "git push --force" while allowing safe
// variants like "--force-with-lease" and "--force-if-includes". tokens must
// be lowercased, shell-aware tokens (see shellTokenize).
func matchesDangerousGitPush(tokens []string) string {
	hasPush := false
	for i, f := range tokens {
		if f == "push" && i > 0 && tokens[i-1] == "git" {
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

// polecatMainPushReason and polecatMainPushAlternative are the block banner
// and its allow-path suggestion for a polecat pushing the default branch.
//
// The alternative names the two flows that legitimately push a default branch
// but get no env signal here (gt-deff): a release (beads-release /
// gastown-release) and a plugin script's own push when an agent runs its
// instructions by hand. Neither gets a signal because a signal an agent sets
// for itself is a user override, not a gate - only the Refinery's merge and
// `gt done`'s direct-merge convoy, which gt itself sets, are gates. So the
// allow path for both is a crew, mayor, or refinery session.
const (
	polecatMainPushReason      = "Polecats never push to main/master (use gt done)"
	polecatMainPushAlternative = "Alternative: `gt done` pushes your polecat/<name>/<bead> branch and the Refinery " +
		"merges it to the default branch after verification — a direct push to main skips " +
		"the MR, the Refinery gate run, and the om review (gt-ibt8). A release, or a plugin " +
		"script's own push, runs from a crew/mayor/refinery session instead (gt-deff)."
)

// polecatMainPushBranches are the destination branch names a polecat session
// may never push to. The rig's actual default branch is resolved dynamically
// by the pre-push hook, which has the remote in front of it; this guard has
// only the command text, so it protects both conventional names.
var polecatMainPushBranches = map[string]bool{"main": true, "master": true}

// inPolecatSession reports whether this guard run is inside a polecat's
// session. GT_ROLE decides first, exactly as it does for the guard family's
// other polecat check (isPolecatSession, tap_guard.go) and for `gt sling`,
// `gt hook` and `gt handoff`: one rule answers "is this a polecat", so a
// session the town names a polecat gets the rule whatever markers its
// environment happens to carry (gt-c38o). GT_POLECAT_PATH — exported to the
// session at polecat spawn (internal/polecat/session_manager.go) — is the
// fallback for a run with no role in its environment at all.
func inPolecatSession() bool {
	if role := strings.TrimSpace(os.Getenv("GT_ROLE")); role != "" {
		return isPolecatRole(role)
	}
	return os.Getenv("GT_POLECAT_PATH") != ""
}

// matchesPolecatMainPush blocks a `git push` from a polecat session whose
// refspec sends work to the default branch (main/master), plus the
// --all/--mirror forms that carry main along with every other branch.
//
// A polecat's work lands through the merge queue: `gt done` pushes
// polecat/<name>/<bead>, the Refinery gates the stack, and only the Refinery
// pushes the default branch. Nothing enforced that. On 09-17 a polecat
// (granite) ran `git push origin HEAD:main` from a detached HEAD after a
// rebase, landing three raw checkpoint commits on origin/main with no MR, no
// Refinery gates, and no om review (gt-ibt8) — the pre-push hook's branch
// allowlist let the default branch through for every caller, and the only
// main-specific check was integration-branch content. This is the second of
// three layers: the pre-push role check refuses the push itself, and this
// stops the attempt before git is even invoked.
//
// Deliberately scoped to explicit refspecs. A bare `git push` (no refspec)
// would also carry main if HEAD were on it, but deciding that needs HEAD's
// branch name, which a command-text guard does not have — the pre-push hook
// covers that case, where the repo and the refspec are both in hand.
//
// tokens must be lowercased, shell-aware tokens (see shellTokenize);
// polecatSession is passed in rather than read here so the matcher stays pure
// and table-testable, with the caller supplying it from the session
// (inPolecatSession).
func matchesPolecatMainPush(tokens []string, polecatSession bool) (reason, alternative string) {
	if !polecatSession {
		return "", ""
	}
	inPush := false
	for i, f := range tokens {
		if !inPush {
			// Same argv shape as matchesDangerousGitPush: a bare
			// "push" token directly after "git".
			if f == "push" && i > 0 && tokens[i-1] == "git" {
				inPush = true
			}
			continue
		}
		if f == "--all" || f == "--mirror" {
			return polecatMainPushReason, polecatMainPushAlternative
		}
		if strings.HasPrefix(f, "-") {
			// Flags carry no destination; --delete's target is the bare
			// branch argument that follows it, which this loop still sees.
			continue
		}
		if polecatMainPushTargetsDefault(f) {
			return polecatMainPushReason, polecatMainPushAlternative
		}
	}
	return "", ""
}

// polecatMainPushTargetsDefault reports whether one `git push` argument sends
// work to main/master. The refspec's DESTINATION decides: "HEAD:main" and
// ":main" (a delete) do, while "main:polecat/x" only reads from main and is
// left alone. An argument with no colon is the shorthand form, where the
// destination is the name itself ("git push origin main").
func polecatMainPushTargetsDefault(arg string) bool {
	arg = strings.TrimPrefix(arg, "+") // force prefix: same destination
	dst := arg
	if _, after, ok := strings.Cut(arg, ":"); ok {
		dst = after
	}
	return polecatMainPushBranches[strings.TrimPrefix(dst, "refs/heads/")]
}

// gitResetRemotePrefixes are token prefixes that name a remote-tracking ref:
// "origin/main", "upstream/main", and the long form of either.
var gitResetRemotePrefixes = []string{"origin/", "upstream/", "refs/remotes/"}

// matchesDangerousGitReset blocks `git reset <remote-tracking-ref>` in any of
// its modes — explicit --soft/--mixed/--hard/--keep, or the implicit --mixed of
// a bare `git reset origin/main`.
//
// The usual reason a branch is reset onto the remote is to squash local work
// onto a fresh base, and that is exactly what produces a revert of everything
// merged since the checkout was cut: reset moves HEAD while the index and
// working tree stay where the old checkout left them, so the next commit
// records (old tree) - (new tip). Two polecat MRs landed that way in one night
// (gt-wisp-hrau, gt-wisp-p7nl — gt-63sz), each reverting other people's merged
// work under a message describing unrelated work. The modes that take a working
// tree along (--hard, --keep) additionally destroy uncommitted work.
//
// The sanctioned integration is `git rebase <remote-ref>`, which replays the
// branch's own commits onto the new base and leaves the remote's content alone.
// Only a reset whose TARGET is a remote-tracking ref is blocked, so the
// ordinary `git reset --soft HEAD~1` squash — and every reset to a local ref or
// pathspec — still works. tokens must be lowercased, shell-aware tokens (see
// shellTokenize); the "git reset" adjacency check matches matchesDangerousGitPush,
// which treats `git -C <dir> push --force` as out of scope for the same reason:
// the guard scans argv shapes, and the branch content check in gt done is what
// actually fails closed.
func matchesDangerousGitReset(tokens []string) (reason, alternative string) {
	inReset := false
	for i, f := range tokens {
		if f == "reset" && i > 0 && tokens[i-1] == "git" {
			inReset = true
			continue
		}
		if !inReset {
			continue
		}
		// A bare "--" ends the revisions: everything after it is a pathspec,
		// so "git reset -- origin/main" is unstaging a path that happens to be
		// spelled like a ref, not resetting onto one.
		if f == "--" {
			return "", ""
		}
		for _, prefix := range gitResetRemotePrefixes {
			if strings.HasPrefix(f, prefix) {
				return "Reset onto a remote-tracking ref drops merged work",
					"Alternative: `git rebase " + f + "` — rebase your CHANGES onto the remote ref; never reset your tree onto it."
			}
		}
	}
	return "", ""
}

const gitCleanReason = "git clean -f deletes untracked files irreversibly"

// matchesGitClean blocks a `git clean` invocation that carries a force flag.
//
// The words are matched as one invocation — "git" running, "clean" as its
// subcommand, the flag among that invocation's own arguments — not as three
// fragments present anywhere in the token list. Containment matched the words
// wherever they landed, so a refinery merge step whose `test -f .git/MERGE_HEAD`
// supplied the "-f" and whose `echo "clean"` supplied the "clean" was rejected
// as a git clean (gt-775d). A mention behind a text-only command (echo, grep,
// gt mail) is not an invocation; anything else is, the fail-closed reading
// inCommandPosition states.
//
// tokens must be original-case, shell-aware tokens (see shellTokenize): the
// command and subcommand are compared case-insensitively, while the force flag
// is matched case-sensitively, so an unrelated "-x" or a long-form flag of
// another subcommand is not read as one.
func matchesGitClean(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if !strings.EqualFold(filepath.Base(tok), "git") || !inCommandPosition(segment, i) {
				continue
			}
			rest := segment[i+1:]
			sub := gitSubcommandIndex(rest)
			if sub < 0 || !strings.EqualFold(rest[sub], "clean") {
				continue
			}
			if hasShortFlagLetter(rest[sub+1:], 'f') {
				return gitCleanReason
			}
		}
	}
	return ""
}

// goCleanSharedCacheFlags are the 'go clean' flags that wipe caches shared
// by every agent on the host. Ported from the interim host-hygiene hook
// (~/gt/.claude/hooks/no-root-scan.sh) — see gt-nqcy follow-up.
var goCleanSharedCacheFlags = map[string]bool{
	"-cache":     true,
	"-testcache": true,
	"-modcache":  true,
	"-fuzzcache": true,
}

const goCleanSharedCacheReason = "'go clean' with -cache/-testcache/-modcache/-fuzzcache wipes the Go build cache shared by every agent on this host"
const goCleanSharedCacheAlternative = "Alternative: use 'go test -count=1' for a cold run, or rebuild a single package; " +
	"if you believe the cache is corrupt, mail the mayor with the evidence instead of clearing it."

// matchesGoCleanSharedCache blocks 'go clean' invocations carrying any of
// the shared-cache flags (goCleanSharedCacheFlags), in any order and
// combined with other clean flags (e.g. 'go clean -x -cache'). tokens must
// be lowercased, shell-aware tokens (see shellTokenize). inClean resets at
// every shell operator so a flag on an unrelated LATER command on the same
// line (e.g. "go clean -i; ls -la -cache") is never mistaken for one of
// 'go clean's own arguments.
func matchesGoCleanSharedCache(tokens []string) (reason, alternative string) {
	inClean := false
	for i, f := range tokens {
		if shellCommandSeparators[f] {
			inClean = false
			continue
		}
		if f == "clean" && i > 0 && tokens[i-1] == "go" {
			inClean = true
			continue
		}
		if !inClean {
			continue
		}
		if goCleanSharedCacheFlags[f] {
			return goCleanSharedCacheReason, goCleanSharedCacheAlternative
		}
	}
	return "", ""
}

// idleGateLoad1Threshold is the 1-minute load average above which a
// full-suite start is HELD (exit 2, retryable) rather than permitted. This
// mirrors the mayor standing rule mol-refinery-patrol's gate-load-check step
// already tells operators to follow by hand ("do NOT start or retry a
// full-suite gate while 1-min loadavg > 60"); the guard used to derive its
// own, stricter threshold from load1/NumCPU, which held gates on idle,
// many-core hosts because macOS loadavg counts uninterruptible-wait
// processes, not just CPU-runnable ones (gt-e6xh).
const idleGateLoad1Threshold = 60

// idleGateAlternative is the HOLD banner's suggested next step.
const idleGateAlternative = "Wait 2 minutes and re-run this exact command; it will pass once load1 <= 60. " +
	"Avoid bare 'go test ./...' at full parallelism — use GOFLAGS=-p=8 make test."

// isIdleGatedSuiteStartCommand reports whether command contains an
// unwrapped full-suite start: 'make test', 'go test ./...', 'make build',
// or 'go build ./...'. Heredoc bodies are stripped first (stripHeredocBodies)
// so prose written to a file — "cat > note.md <<'EOF'\nRun make test\nEOF" —
// is never misread as a live invocation, the same rule evaluateDangerousCommand
// applies. The remaining text is split into shell segments (on ;/&&/||/|,
// same as evaluateContainerSuiteCommand) and each is evaluated independently,
// so a "gt slot run -- make test" wrapper and an unrelated later "make test"
// on the same compound line are judged separately. Unlike
// evaluateDangerousCommand, this does NOT recurse into bash -c/eval payloads
// or command substitutions — the interim hook it replaces didn't either, and
// no incident has required it; scope stays narrow until one does.
func isIdleGatedSuiteStartCommand(command string) bool {
	tokens := shellTokenize(strings.TrimSpace(stripHeredocBodies(command)))
	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if isIdleGatedSuiteStartSegment(segment) {
				return true
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return isIdleGatedSuiteStartSegment(segment)
}

// idleGateSlotRunPrefix is the token sequence a shell segment must START
// WITH to count as already running through the townwide container-gate slot
// ('gt slot run --role <rig>/<name> -- <command>', see internal/slot). The
// match is anchored to the segment's own command, not a subsequence found
// anywhere in it — a subsequence match would let a trailing, non-executing
// mention of the same three words (e.g. inside a later argument) falsely
// exempt an actually-unwrapped invocation.
var idleGateSlotRunPrefix = containerSuiteSlotRunTokens

// isIdleGatedSuiteStartSegment judges a single shell segment (tokens
// between shell operators). tokens is original-case; matching is done on a
// lowercased copy so "Make Test" and "make test" are treated the same.
func isIdleGatedSuiteStartSegment(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	if hasPrefix(lower, idleGateSlotRunPrefix) {
		return false
	}
	if findInvocation(lower, "make", "test") >= 0 {
		return true
	}
	if findInvocation(lower, "make", "build") >= 0 {
		return true
	}
	if i := findInvocation(lower, "go", "test"); i >= 0 && wholeRepoArgFollows(tokens, i+2) {
		return true
	}
	if i := findInvocation(lower, "go", "build"); i >= 0 && wholeRepoArgFollows(tokens, i+2) {
		return true
	}
	return false
}

// hasPrefix reports whether tokens begins with prefix, element for element.
func hasPrefix(tokens, prefix []string) bool {
	if len(tokens) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if tokens[i] != p {
			return false
		}
	}
	return true
}

// wholeRepoArgFollows reports whether the token at idx names the whole-repo
// wildcard ("./...", "...", or the module-prefixed spelling). Mirrors the
// interim hook's exact "go test ./..."/"go build ./..." adjacency: only the
// token immediately after the subcommand is checked, not a flag further
// down the line — 'go test -v ./...' is intentionally NOT recognized here,
// same as the interim regex it replaces.
func wholeRepoArgFollows(tokens []string, idx int) bool {
	return idx < len(tokens) && isWholeRepoPackageArg(normalizeGoPackageArg(tokens[idx]))
}

// hostLoad1 returns the current 1-minute load average. Overridden in tests.
// Production reads internal/daemon's raw load-average sample (one instant
// sysctl/proc read, no subprocess sampling loop) rather than shelling out to
// `top` for several seconds on every suite-start command — the same
// mechanism that already powers the main_branch_test patrol's gate
// (gt-f57o), read unnormalized so a host whose load comes from non-CPU
// (uninterruptible-wait) contention isn't misjudged as CPU-saturated
// (gt-e6xh).
var hostLoad1 = actualHostLoad1

func actualHostLoad1() (load1 float64, ok bool) {
	return daemon.EstimateLoad1(), true
}

// printIdleGateHold prints the HOLD banner to stderr — distinct from
// printDangerousBlock's BLOCKED banner since this command is expected to
// succeed on retry, not to be avoided entirely.
func printIdleGateHold(load1 float64, command string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ⏸  SUITE START HELD (host busy)                                 ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Command:   %-51s ║\n", truncateStr(command, 51))
	fmt.Fprintf(os.Stderr, "║  Load avg:  %-51s ║\n", fmt.Sprintf("%.1f (need <= %d)", load1, idleGateLoad1Threshold))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Another suite is running on this shared host.                  ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "  "+idleGateAlternative)
	fmt.Fprintln(os.Stderr, "")
}

// inWitnessSession reports whether the hook runs inside a witness session.
// GT_ROLE is "<rig>/witness" for rig witnesses; the bare form covers a
// witness started outside a rig context.
func inWitnessSession() bool {
	role, _, _ := parseRoleString(os.Getenv("GT_ROLE"))
	return role == RoleWitness
}

const witnessGitPushReason = "git push from a witness session"
const witnessGitPushAlternative = "Alternative: a witness observes and reports. Pushing a polecat's branch is the " +
	"polecat's job (gt done) or the refinery's (with the mayor's authority); mail the mayor or the polecat " +
	"with what you found instead."

// matchesWitnessGitPush blocks every `git push` from a witness session,
// whatever the refspec. A witness never owns a branch: on 2026-09-18 a
// local-model witness, acting on a mail about a refinery fast-forward
// repair, composed `git push origin <branch>:<sha> --force-with-lease` in a
// polecat's worktree — a push to a branch named by a commit hash — and only
// the branch-policy pre-push hook stopped it. The rule turns "witnesses do
// not push" into a check the model cannot misread.
func matchesWitnessGitPush(tokens []string, witnessSession bool) (reason, alternative string) {
	if !witnessSession {
		return "", ""
	}
	for i, f := range tokens {
		if f == "git" && inCommandPosition(tokens, i) && gitSubcommand(tokens[i+1:]) == "push" {
			return witnessGitPushReason, witnessGitPushAlternative
		}
	}
	return "", ""
}

const refineryRawNotesPushReason = "raw git push of refs/notes/ from a refinery session"
const refineryRawNotesPushAlternative = "Alternative: `gt mq review` and `gt mq rekey-note` publish " +
	"refs/notes/om through a bounded timeout that kills git and any credential helper if the remote " +
	"hangs; let the gate publish the note, or bound a manual push yourself: " +
	"`GIT_TERMINAL_PROMPT=0 timeout 60 git push origin refs/notes/om` (gt-qhhlr)."

// matchesRefineryRawNotesPush blocks an unbounded `git push` targeting a
// refs/notes/ ref from a refinery session. The refinery's own note-publishing
// paths (gt mq review, gt mq rekey-note) already push notes through
// git.PushNotes, which runs the command with a timeout and kills its whole
// process group — including any credential helper — if the remote hangs
// (see pushTimeout in internal/git/git.go). A refinery agent typing the same
// push by hand as a raw shell command gets none of that: git can fork a
// credential helper (e.g. git-credential-osxkeychain) that blocks on a
// keychain prompt no headless session can answer, and that child can outlive
// git itself, holding open whatever pipe is reading the command's output
// (gt-qhhlr).
//
// --all and --mirror push every ref including refs/notes/om without ever
// spelling it out as an argument, the same implicit-destination gap
// matchesPolecatMainPush already guards for a main-branch push, so those
// flags match here too regardless of any refs/notes/ token being present.
//
// Known gap: inCommandPosition judges the whole command by tokens[0], so a
// compound command that runs git push after another program's own tokens
// (e.g. `echo x && git push origin refs/notes/om`) is not caught here. That
// is pre-existing shared behavior (matchesWitnessGitPush has the same gap),
// not something this guard alone can close.
func matchesRefineryRawNotesPush(tokens []string, refinerySession bool) (reason, alternative string) {
	if !refinerySession {
		return "", ""
	}
	for i, f := range tokens {
		if f != "git" || !inCommandPosition(tokens, i) || gitSubcommand(tokens[i+1:]) != "push" {
			continue
		}
		for _, arg := range tokens[i+1:] {
			if arg == "--all" || arg == "--mirror" || strings.Contains(arg, "refs/notes/") {
				return refineryRawNotesPushReason, refineryRawNotesPushAlternative
			}
		}
	}
	return "", ""
}

// inCommandPosition reports whether tokens[i] ("git") is being run rather
// than mentioned. It fails closed: git counts as a command unless the
// segment's own program is one that only carries text (echo, printf, gt
// mail, bd comments/create ...), so `timeout 60 git push`, `eval git push`
// and `xargs git push` are all refused while `echo do not git push` and a
// mail body that mentions pushing are not.
func inCommandPosition(tokens []string, i int) bool {
	if i == 0 {
		return true
	}
	switch tokens[0] {
	case "echo", "printf", "gt", "bd", "cat", "grep", "rg":
		return false
	}
	return true
}

// gitOptionsWithValue are git's global options that consume the next token
// (space-separated form), so `git -C dir push` still resolves to push.
var gitOptionsWithValue = map[string]bool{
	"-c": true, "-C": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
}

// gitSubcommand returns the first non-option token after "git" — the
// subcommand — skipping global options in both `-C dir` and `--git-dir=x`
// forms. Returns "" when the tokens end before a subcommand.
func gitSubcommand(rest []string) string {
	if i := gitSubcommandIndex(rest); i >= 0 {
		return rest[i]
	}
	return ""
}

// gitSubcommandIndex returns the position of git's subcommand in rest (the
// tokens after "git"), or -1 when rest ends before one. Callers that need the
// subcommand's own arguments — which start after that token, not after the
// global options in front of it — slice from here.
func gitSubcommandIndex(rest []string) int {
	for i := 0; i < len(rest); i++ {
		t := rest[i]
		if !strings.HasPrefix(t, "-") {
			return i
		}
		if gitOptionsWithValue[t] && !strings.Contains(t, "=") {
			i++ // skip the option's value
		}
	}
	return -1
}
