package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/shellparse"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

var tapGuardContainerSuiteCmd = &cobra.Command{
	Use:   "container-suite",
	Short: "Block bare go test/make test on testcontainers-backed packages",
	Long: `Block unwrapped container-backed test suites via Claude Code PreToolUse hooks.

Some packages spin real Dolt/testcontainers containers in their tests (see
internal/slot's package doc). The host's Docker VM has a fixed CPU/memory
bound shared by the whole town, so two container-backed suites running
concurrently starve each other even when the host itself shows idle CPU.
'gt slot run' serializes access to that VM, but only for commands actually
run through it — an ad-hoc "go test ./internal/beads/..." run straight from
a polecat or refinery session is invisible to the slot and still collides
with anything else using the VM (gt-e2rs: two such collisions in one night,
one starving the refinery for 29 minutes).

This guard blocks, when running as a polecat or refinery:
  - go test <pkg...>   where <pkg...> intersects the testcontainers-backed
                        package list (containerSuitePackages) or is a
                        whole-repo wildcard (./..., ...). A package argument
                        spelled as a path under the directory the
                        invocation runs in ("./beads/...") is judged there,
                        which is where go resolves it, as well as at the
                        module root. Go's global -C dir flag between the
                        binary and "test", in either spelling ("go -C <dir>
                        test ..."), does not hide the invocation, and the
                        toolchain's own binary names (go, go1.22.0, gotip)
                        own it too (gt-saorq, gt-g6yi7)
  - go test .          (or a bare "go test") when the directory it runs in
                        is one of those packages — the cwd is judged like
                        any other target, not exempted, and a -C dir flag
                        moves it (gt-tzdpe)
  - make test           in a Go tree, where it runs the module's whole
                        "go test ./..."; a non-Go rig's own make target is
                        left alone. The tree is the one make itself runs in,
                        so "make -C <go tree> test" is refused wherever the
                        hook's own cwd happens to be, and a named makefile
                        (-f) is refused outright — the guard cannot read what
                        that target does (gt-dieu9)

The directory a segment is judged in is the one it runs in: a cd, pushd or
popd earlier on the same shell line carries into the segments after it
("&&"/";" only, as the shell does), so changing into a Go tree and running the
suite there is judged in that tree rather than in the rig the hook was invoked
from (gt-5mc21, gt-n7ksl), and a heredoc body fed to a shell is judged in the
directory its reader line left the shell in (gt-1cvqj).

...unless the command is already wrapped in 'gt slot run -- <command>', in
which case it is allowed through untouched. Bare go test runs that only
target non-Docker packages are also allowed through.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardContainerSuite,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardContainerSuiteCmd)
}

// containerSuitePackages is the list the guard judges a command against.
// specdispatch.ContainerSuitePackages is its one source, shared with the spec
// lint's Gate check so the two cannot drift.
var containerSuitePackages = specdispatch.ContainerSuitePackages()

func runTapGuardContainerSuite(cmd *cobra.Command, args []string) error {
	return tapGuardContainerSuite(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardContainerSuite is the container-suite guard: it reads the hook
// payload from stdin and the session from proc, and prints a block to stderr.
func tapGuardContainerSuite(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
	if err != nil {
		return nil // fail open
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}
	if !isPolecatOrRefineryContext(proc) {
		return nil
	}

	// The scope rule answers first for a polecat: its advice is "iterate with
	// -run", and it must win over the container-suite rule's "run it wrapped"
	// line, which for a whole-suite command is exactly the run the scope rule
	// forbids (gt-v6se: three polecats followed that line into full suites
	// beside the refinery's gate).
	if isPolecatContext(proc) {
		if reason, matched := evaluatePolecatTestScope(proc, command); reason != "" {
			printPolecatTestScopeBlock(stderr, reason, command, matched)
			return NewSilentExit(2)
		}
	}
	if reason, matched := evaluateContainerSuiteCommand(proc, command, proc.getenv(dockerTestsEnv)); reason != "" {
		printContainerSuiteBlock(stderr, reason, command, matched, proc.getenv("GT_ROLE"))
		return NewSilentExit(2)
	}
	return nil
}

// isPolecatOrRefineryContext reports whether the current process is running
// as a polecat or refinery — the two roles whose ad-hoc scoped go test/make
// test invocations have repeatedly spun Dolt/testcontainers containers
// unwrapped next to the refinery's own gt-slot-run-wrapped gate (gt-e2rs:
// two collisions in one night, one starving the refinery for 29 minutes).
// Other agent contexts (crew, witness, deacon) are left unguarded — they
// don't run scoped test suites as part of their normal work.
func isPolecatOrRefineryContext(proc guardProcess) bool {
	// GT_REFINERY is the marker a refinery spawn sets, and it decides first: a
	// refinery's GT_ROLE ("gastown/refinery") parses as an unknown role because
	// the refinery role was deleted (gt-v4ssj.6), so the role-first reading
	// below answers false for the session this gate exists to cover
	// (gt-j2n8y1).
	if proc.getenv("GT_REFINERY") != "" {
		return true
	}
	// GT_ROLE decides next (see isPolecatContext): a stale GT_POLECAT does not
	// make a refinery or crew session a polecat (gt-pb77k).
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		return isPolecatContext(proc)
	}
	if proc.getenv("GT_POLECAT") != "" {
		return true
	}
	cwd, err := proc.getwd()
	if err == nil && strings.Contains(cwd, "/polecats/") {
		return true
	}
	return false
}

// containerSuiteSlotRunTokens is the token sequence that marks a shell
// segment as already running through the townwide container-gate slot (see
// internal/slot and 'gt slot run --role <rig>/<name> -- <command>').
var containerSuiteSlotRunTokens = []string{"gt", "slot", "run"}

// slotRunWrapped reports whether a segment runs through the gt slot run
// wrapper: its own command — the words behind the leading NAME=value
// assignments and launchers a shell looks through (prWorkflowCommand) — starts
// with the three wrapper words. The match is anchored to that command, not a
// subsequence found anywhere in the segment: a subsequence match exempted
// "GT_TEST_DOCKER=1 go test ./internal/beads -args gt slot run", where the
// three words are go's own test arguments and no slot is involved. It is the
// reading idleGateSlotRunPrefix gives the same wrapper (gt-jj3or, and
// gt-sveq8's "timeout 5 gt slot run -- ..." must stay wrapped, so the
// launchers count).
func slotRunWrapped(segment []string) bool {
	argv, _, _ := prWorkflowCommand(segment)
	return hasPrefix(toLowerTokens(argv), containerSuiteSlotRunTokens)
}

// maxTestGuardNestDepth bounds the shell-fed-heredoc, -c payload, eval and
// command-substitution recursion in
// evaluateContainerSuiteCommand and evaluatePolecatTestScope, so a
// pathological command nesting heredocs inside heredocs still terminates.
// Same shape and value as maxPRWorkflowNestDepth, which bounds the identical
// recursion in matchesPRWorkflowCommand.
const maxTestGuardNestDepth = 3

// nestedTooDeepRefusal is the verdict at maxTestGuardNestDepth for a command
// that still holds a shell it would have to read: a script the guard cannot
// reach is refused, not read as one that runs nothing. A level with no nested
// shell has nothing left to inspect and is allowed (gt-rws47).
func nestedTooDeepRefusal(holdsNestedShell bool) (string, []string) {
	if !holdsNestedShell {
		return "", nil
	}
	return fmt.Sprintf("shells nested more than %d deep cannot be inspected for a test run", maxTestGuardNestDepth), nil
}

// shellCPayloads returns the command strings the shell invokers in segment run
// with -c. A segment judge reads a quoted payload as one opaque word, so
// "bash -c 'make gate'" showed it a bash call and nothing else; the callers
// judge each payload as the command line it is, in the segment's directory
// (gt-y1zks).
func shellCPayloads(segment []string) []string {
	lower := toLowerTokens(segment)
	var payloads []string
	for i, word := range lower {
		if !shellInvokers[filepath.Base(word)] {
			continue
		}
		if idx := shellCPayloadIndex(lower[i+1:]); idx >= 0 {
			payloads = append(payloads, segment[i+1+idx])
		}
	}
	return payloads
}

// evalPayloads returns the command string the eval a segment opens runs. eval
// joins its arguments with spaces and runs the result as one command line, so
// `eval "go test ./..."` is a go test the segment judge saw as a quoted word
// after eval. Only an eval at command position counts: a segment that merely
// spells the word ("grep eval", "echo eval make test") runs nothing
// (gt-t1pct).
func evalPayloads(segment []string) []string {
	word, rest := segmentCommandWordPastRedirections(trimShellKeywords(segment))
	if strings.ToLower(word) != "eval" || len(rest) == 0 {
		return nil
	}
	return []string{strings.Join(rest, " ")}
}

// shellCommandWord returns a segment's command word with the redirections
// ahead of it stepped over, so "2>/dev/null sh <<< 'make gate'" is read as the
// sh it runs (gt-8rxxy). The stepped-over tokens stay in args: a here-string
// ahead of a shell is the script it reads, so the payload readers must see it.
func shellCommandWord(segment []string) (string, []string) {
	word, args, lead := segmentCommandWordPastRedirectionsKeepingLead(trimShellKeywords(segment))
	return word, append(lead, args...)
}

// splitHereStrings returns args without their "<<<" here-string redirections,
// and the text each one feeds its command's stdin. A here-string on another
// descriptor ("3<<<text", "{fd}<<<text") feeds that descriptor, not the
// script, so it is dropped without its text (gt-hizij).
//
// A bare "<<<" takes the words after it as its text: the shell word-splits an
// unquoted body, so the run of words up to the next redirection is the script.
// The first word is read on its own as well, so a body of several words adds a
// reading rather than replacing the one a single word drew (gt-norvx).
func splitHereStrings(args []string) (rest, texts []string) {
	for i := 0; i < len(args); i++ {
		fd, tail, ok := redirectionFD(args[i])
		if !ok || !strings.HasPrefix(tail, "<<<") {
			rest = append(rest, args[i])
			continue
		}
		text := strings.TrimPrefix(tail, "<<<")
		var words []string
		if text == "" && i+1 < len(args) {
			end := i + 1
			for end < len(args) {
				if redirected, _, _ := shellRedirection(args[end]); redirected {
					break
				}
				end++
			}
			// A redirection right behind the operator leaves the body empty; it
			// stays a word of its own for the shell, read by the next pass.
			if words = args[i+1 : end]; len(words) > 0 {
				text, i = words[0], end-1
			}
		}
		if fd == "" || fd == "0" {
			texts = append(texts, text)
			if len(words) > 1 {
				texts = append(texts, strings.Join(words, " "))
			}
		}
	}
	return rest, texts
}

// shellReadsStdin reports whether a shell invoked with args (lowercased, the
// words after the invoker) reads its script from stdin: it has no -c payload
// and no script file operand, or -s asks for stdin outright; dashS says it was
// the -s, which leaves every operand to the script as a positional parameter.
// Options are walked as shellCPayloadIndex walks them.
func shellReadsStdin(args []string) (reads, dashS bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-":
			return true, false
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && a[0] == '+':
			i += shellClusterValues(a)
		case len(a) > 1 && a[0] == '-':
			i += shellClusterValues(a)
			if strings.Contains(a[1:], "c") {
				return false, false
			}
			if strings.Contains(a[1:], "s") {
				return true, true
			}
		default:
			redirected, skip, replacesStdin := shellRedirection(a)
			if !redirected || replacesStdin {
				return false, false
			}
			i += skip
		}
	}
	return true, false
}

// shellRedirection reports whether word is a redirection (2>&1, >out, &>log,
// 2>, 3<in) rather than an operand, how many words after it are its target
// (one when the target is a separate word: "> out", "2>"), and whether it
// replaces the shell's stdin. A redirection other than stdin's leaves the
// shell reading its script from stdin, so shellReadsStdin walks past it; one
// that replaces stdin ("< run.sh", "<&3", "0<in") feeds the shell a file the
// guards cannot read, so the producer upstream is not its script. A heredoc
// ("<<EOF") replaces stdin the same way and is judged where its body is
// scanned, not here (gt-sveq8); a heredoc or here-string on another descriptor
// ("3<<EOF", "3<<<text") leaves stdin alone (gt-hizij). A word no shape here
// matches is an operand.
func shellRedirection(word string) (redirected bool, skip int, replacesStdin bool) {
	fd, rest, ok := redirectionFD(word)
	if !ok {
		return false, 0, false
	}
	if strings.HasPrefix(rest, "<<") {
		op := "<<"
		for _, candidate := range []string{"<<<", "<<-"} {
			if strings.HasPrefix(rest, candidate) {
				op = candidate
			}
		}
		if rest[len(op):] == "" {
			skip = 1
		}
		return true, skip, fd == "" || fd == "0"
	}
	var op string
	for _, candidate := range []string{"&>>", "&>", ">>", ">&", ">|", ">", "<>", "<&", "<"} {
		if strings.HasPrefix(rest, candidate) {
			op = candidate
			break
		}
	}
	if op == "" || (fd != "" && strings.HasPrefix(op, "&")) {
		return false, 0, false
	}
	if rest[len(op):] == "" {
		skip = 1
	}
	replacesStdin = (fd == "" || fd == "0") && strings.HasPrefix(op, "<")
	return true, skip, replacesStdin
}

// redirectionFD splits the file descriptor a redirection word names off its
// front: digits ("2>&1"), a variable in braces ("{fd}>log") or nothing. ok is
// false for a "{" that closes nowhere or holds no name, which is no
// descriptor.
func redirectionFD(word string) (fd, rest string, ok bool) {
	if strings.HasPrefix(word, "{") {
		end := strings.IndexByte(word, '}')
		if end < 2 {
			return "", word, false
		}
		return "{}", word[end+1:], true
	}
	digits := len(word) - len(strings.TrimLeft(word, "0123456789"))
	return word[:digits], word[digits:], true
}

// substitutionSpan locates one command substitution in a shell line: open is
// where its "$(" or backtick starts and text[bodyStart:bodyEnd] is the command
// line inside it.
type substitutionSpan struct {
	open, bodyStart, bodyEnd int
}

// commandSubstitutionSpans finds the outermost command substitutions the shell
// expands in text, in order. It balances parentheses and steps over quoted
// text, so a body holding "(ok|FAIL)", "$(NF)" or a nested "$(nproc)" is cut
// whole (gt-wrr5c). A "$(" or backtick is live outside quotes and inside double
// quotes; a backslash escapes the next character, and single quotes hide
// everything until the closing quote. A substitution that never closes runs to
// the end of text, so a line the shell would reject is still judged rather than
// passed unread (gt-6v07e).
func commandSubstitutionSpans(text string) []substitutionSpan {
	return substitutionSpansIn(text, true)
}

// heredocSubstitutionSpans is commandSubstitutionSpans for the text of an
// unquoted heredoc body. There a quote mark is an ordinary character, so the
// apostrophe in "don't" hides nothing and a "$(make gate)" after it is live;
// only a backslash still keeps the next character literal (gt-6fkng).
func heredocSubstitutionSpans(body string) []substitutionSpan {
	return substitutionSpansIn(body, false)
}

// substitutionSpansIn is the one command-substitution scan: the
// commandSubstitutionSpans and heredocSubstitutionSpans wrappers serve the
// container-suite and dangerous guards, and the pr-workflow guard reads it
// directly for the payloads it recurses into (gt-ejn41). quotes says whether a
// quote mark in text opens quoted text; inside a substitution's own body
// closingParen honors quotes either way, as the shell does.
//
// With quotes, a "#" that starts a word opens a comment to the end of the line:
// the shell reads an apostrophe there as plain text, so "# don't" must not hide
// the "$(...)" on the next line. The comment body is still scanned for
// substitutions, so a "$(" in it is judged as before (over-blocked, never
// passed). A "$'" opens an ANSI-C string, where "\'" does not close the quote.
// The comment read ends a quoted span earlier than the bare scan did, so it only
// adds spans. The ANSI-C read ends one later, so a "$(...)" after a "\'" inside
// $'...' is now literal text and no longer a span, as the shell reads it
// (gt-2yc0u).
//
// A process substitution ("<(...)" / ">(...)") is a span too: the shell runs
// the command inside it and hands its output to the word, so "cat <(gh pr
// create --fill)" creates the PR and "cat <(git reset --hard)" resets the tree
// though the segment judge sees only a cat (gt-jj3or). It is read only where
// the shell performs one — not inside double quotes, single quotes or a
// comment, where the angle bracket is plain text, and not in a heredoc body
// (quotes is false there), where the shell's expansion of the body does not
// include process substitution: "cat > f <<EOF" holding "cat <(ls)" writes
// that line as data. A heredoc body fed to a shell is a script, so the
// recursion judges its process substitutions in the body's own reading.
func substitutionSpansIn(text string, quotes bool) []substitutionSpan {
	var spans []substitutionSpan
	// Found once for the whole text: closingParen skips the bodies, and the
	// substitutions inside them share this scan rather than each rescanning
	// (gt-lx8i8).
	bodies := heredocBodiesFrom(text, 0)
	inDouble := false
	inComment := false
	escapedAt := -1
	subClose := -1
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case inComment && c == '\n':
			inComment = false
		case c == '\\' && !inComment:
			escapedAt = i + 1
			i++
		case quotes && !inComment && !inDouble && c == '\'':
			end := strings.IndexByte(text[i+1:], '\'')
			if end < 0 {
				return spans
			}
			i += end + 1
		case quotes && !inComment && !inDouble && c == '$' && ansiQuoteAt(text, i) >= 0:
			// An unterminated $'...' is left to the plain single-quote scan
			// on the next character, which is how it read before.
			if end := closingANSIQuote(text, ansiQuoteAt(text, i)+1); end >= 0 {
				i = end
			}
		case quotes && !inComment && !inDouble && c == '#' && startsWordAt(text, i, escapedAt, subClose):
			inComment = true
		case quotes && !inComment && c == '"':
			inDouble = !inDouble
		case quotes && !inComment && !inDouble && (c == '<' || c == '>') && i+1 < len(text) && text[i+1] == '(':
			end := closingParen(text, i+2, bodies)
			if end < 0 {
				return append(spans, substitutionSpan{open: i, bodyStart: i + 2, bodyEnd: len(text)})
			}
			spans = append(spans, substitutionSpan{open: i, bodyStart: i + 2, bodyEnd: end})
			subClose = end
			i = end
		case c == '$' && i+1 < len(text) && text[i+1] == '(':
			end := closingParen(text, i+2, bodies)
			if end < 0 {
				return append(spans, substitutionSpan{open: i, bodyStart: i + 2, bodyEnd: len(text)})
			}
			spans = append(spans, substitutionSpan{open: i, bodyStart: i + 2, bodyEnd: end})
			subClose = end
			i = end
		case c == '`':
			end := backtickBodyEnd(text, i+1)
			if end == len(text) {
				return append(spans, substitutionSpan{open: i, bodyStart: i + 1, bodyEnd: len(text)})
			}
			spans = append(spans, substitutionSpan{open: i, bodyStart: i + 1, bodyEnd: end})
			i = end
		}
	}
	return spans
}

// substitutionBody is the body of one command substitution as the shell parses
// it: a backtick span's body drops the backslash of each \\, \` and \$ pair, the
// only three the shell unescapes there. An escaped nested backtick and a
// "\$(...)" are literal to the outer scan, yet live substitutions to the body's
// own parse, so the nested command the caller must judge is judged (gt-tder1).
// A "$(...)" or process-substitution body is returned as written.
func substitutionBody(text string, span substitutionSpan) string {
	body := text[span.bodyStart:span.bodyEnd]
	if text[span.open] != '`' {
		return body
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' && i+1 < len(body) && strings.IndexByte("\\`$", body[i+1]) >= 0 {
			i++
		}
		b.WriteByte(body[i])
	}
	return b.String()
}

// backtickBodyEnd returns the index of the backtick that closes the one whose
// body starts at start, or len(text) when none does. A backslash keeps the next
// character literal, so the shell reads "\`" as a literal backtick and the body
// runs on past it; the shell drops that backslash, so a caller judging the body
// as a command line drops it too (gt-ejn41).
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

// startsWordAt reports whether text[i] begins a shell word, the only place a
// "#" opens a comment: at the start of text or after a blank, newline or
// command separator. A "#" inside a word (a#b, $#, ${#x}), after a ")" that
// closes one ("$(date)#x") or right after a backslash-escaped character
// (escapedAt is that character's index) is literal.
//
// subClose is the index of the ")" that closed a substitution the scan has
// just stepped over, or -1. That ")" is a part of the word in front of it, so
// a "#" right after it is literal: "echo $(date)#tail" prints "date#tail". Any
// other ")" is an operator that ends a word, as the one in "(true)#c", and a
// "#" after it opens a comment (gt-5wtmz).
func startsWordAt(text string, i, escapedAt, subClose int) bool {
	if i == 0 {
		return true
	}
	if escapedAt == i-1 {
		return false
	}
	switch text[i-1] {
	case ' ', '\t', '\n', ';', '&', '|', '(':
		return true
	case ')':
		return i-1 != subClose
	}
	return false
}

// opensSubstitution reports whether the "(" at i opens a substitution — a
// "$(...)", "$((...))" or "<(...)" / ">(...)" group — rather than a subshell.
// The shell reads a substitution as a part of the word around it, so the ")"
// that closes one ends no word (gt-5wtmz).
func opensSubstitution(text string, i int) bool {
	if i == 0 {
		return false
	}
	switch text[i-1] {
	case '$', '<', '>':
		return true
	case '(':
		return i > 1 && text[i-2] == '$'
	}
	return false
}

// ansiQuoteAt returns the index of the "'" that the "$" at dollar opens an
// ANSI-C string with, or -1. It reads the opener as isANSICDollar does
// (ansiCDollarQuote), so an escaped "$" and the second "$" of a PID "$$" open
// nothing here either, and a backslash-newline between the "$" and the quote is
// stepped over (gt-javvg, gt-22glo, gt-h4go2).
func ansiQuoteAt(text string, dollar int) int {
	return ansiCDollarQuote(len(text), bytesAt(text), dollar)
}

// closingANSIQuote returns the index of the "'" that closes a $'...' string
// whose text starts at from, or -1 when none does. A backslash escapes the next
// character, so \' stays inside the string.
func closingANSIQuote(text string, from int) int {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '\'':
			return i
		}
	}
	return -1
}

// closingParen returns the index of the ")" that closes a "(" whose body starts
// at from, or -1 when none does. Every "(" in the body, a nested "$(" or a
// subshell, opens a level of its own; single-quoted, double-quoted and backtick
// text is stepped over, so a parenthesis inside one counts for nothing.
//
// A "#" that starts a word opens a comment to the end of its line, so a ")"
// inside it closes nothing and the body runs on; a comment that reaches the end
// of the text leaves nothing to close the "(", and -1 is right there too
// (gt-5wtmz).
//
// A heredoc body in the substitution is stepped over whole, as heredocSpans
// places it, so the quote marks and parentheses in it count for nothing
// (gt-lx8i8). bodies is the heredoc spans found in text (heredocBodiesFrom,
// found once per caller so the substitutions closingDoubleQuote steps into
// share the scan rather than each rescanning the text); the slice is read, not
// consumed, by the caller.
func closingParen(text string, from int, bodies []heredocSpan) int {
	depth := 1
	escapedAt := -1
	subClose := -1
	// One entry per "(" the body has open, saying whether it opens a
	// substitution: the ")" that closes one is a word part, so the "#" after
	// it is literal (gt-5wtmz).
	var subOpens []bool
	for i := from; i < len(text); i++ {
		// Skip a body only where this walk arrives at its first byte in plain
		// text. A span heredocSpans placed somewhere this walk reads as quoted
		// or commented text — the two reads disagreeing — is dropped instead, so
		// the skip can never hide text this walk would otherwise have read.
		for len(bodies) > 0 && i >= bodies[0].bodyStart {
			body := bodies[0]
			bodies = bodies[1:]
			if i == body.bodyStart && body.bodyEnd > i {
				i = body.bodyEnd - 1
				break
			}
		}
		switch c := text[i]; {
		case c == '\\':
			escapedAt = i + 1
			i++
		case c == '#' && startsWordAt(text, i, escapedAt, subClose):
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				return -1
			}
			i += end
		case c == '\'':
			end := strings.IndexByte(text[i+1:], '\'')
			if end < 0 {
				return -1
			}
			i += end + 1
		case c == '"':
			end := closingDoubleQuote(text, i+1, bodies)
			if end < 0 {
				return -1
			}
			i = end
		case c == '`':
			end := strings.IndexByte(text[i+1:], '`')
			if end < 0 {
				return -1
			}
			i += end + 1
		case c == '(':
			subOpens = append(subOpens, opensSubstitution(text, i))
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
			subClose = -1
			if subOpens[len(subOpens)-1] {
				subClose = i
			}
			subOpens = subOpens[:len(subOpens)-1]
		}
	}
	return -1
}

// heredocBodiesFrom returns the heredoc bodies that start at or after from, in
// order, as heredocSpans places the whole text. An unterminated body is left
// out: it runs to the end of the text, which would hide the ")" that closes the
// substitution (gt-lx8i8).
func heredocBodiesFrom(text string, from int) []heredocSpan {
	if !strings.Contains(text[from:], "<<") {
		return nil
	}
	var bodies []heredocSpan
	for _, span := range heredocSpans(text) {
		if span.bodyStart >= from && !span.unterminated {
			bodies = append(bodies, span)
		}
	}
	return bodies
}

// closingDoubleQuote returns the index of the unescaped double quote that ends
// a string whose text starts at from, or -1 when none does. A "$(" inside the
// string is a substitution with quoting of its own, so closingParen skips it
// whole; a backtick pair is skipped as one. bodies carries the heredoc bodies
// closingParen already found, so the substitution is read without rescanning
// the text (gt-lx8i8).
func closingDoubleQuote(text string, from int, bodies []heredocSpan) int {
	for i := from; i < len(text); i++ {
		switch {
		case text[i] == '\\':
			i++
		case text[i] == '"':
			return i
		case text[i] == '$' && i+1 < len(text) && text[i+1] == '(':
			end := closingParen(text, i+2, bodies)
			if end < 0 {
				// The substitution never closes, so the line is one the shell
				// rejects. For a quoted dollar-paren — the "$(" shape — the
				// quote after the "(" still ends this double-quoted stretch:
				// read that way, the ")" of the substitution around it stays in
				// view and the command inside it is judged (gt-qg832).
				if i+2 < len(text) && quotedDollarParen(rune(text[i]), rune(text[i+1]), rune(text[i+2])) {
					return i + 2
				}
				return -1
			}
			i = end
		case text[i] == '`':
			end := strings.IndexByte(text[i+1:], '`')
			if end < 0 {
				return -1
			}
			i += end + 1
		}
	}
	return -1
}

func toLowerTokens(tokens []string) []string {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	return lower
}

// commandSegment cuts a segment at the ")" that closes the subshell it sits
// in. A ")" left in the argument list reads as a package argument, which hid
// "(cd internal/cmd && go test)" from the rule for a bare go test (gt-y1zks).
// The "(" tokens that open that subshell lead the segment and are kept; a
// group opened after the command word has its own ")", which is not the cut,
// so the words after it stay in the segment (gt-rws47).
func commandSegment(segment []string) []string {
	lead := 0
	for lead < len(segment) && segment[lead] == "(" {
		lead++
	}
	open := 0
	for i := lead; i < len(segment); i++ {
		switch segment[i] {
		case "(":
			open++
		case ")":
			if open == 0 {
				return segment[:i]
			}
			open--
		}
	}
	return segment
}

// evaluateContainerSuiteCommand splits command into shell segments (on
// ;/&&/||/|, same as matchesPRWorkflowCommand) and evaluates each
// independently, so a "gt slot run -- go test ./..." wrapper and an
// unrelated later "go test ./internal/beads/..." on the same compound line
// are judged separately rather than one exemption covering the whole line.
// Returns the reason for the first blocked segment found, or ("", nil) if
// none is blocked. envOptIn is the hook environment's own GT_TEST_DOCKER.
//
// Segment directories come from segmentWalkRoot, so a polecat in a non-Go rig
// cannot reach the whole suite by changing into a Go tree first (gt-5mc21). A
// directory the walk cannot place is refused rather than read as a non-Go one,
// the reading an unplaceable make -C already gets (gt-ofj05).
//
// Heredoc bodies are stripped before tokenizing (stripHeredocBodies), so a
// body that merely spells "make test" — a bead description, a doc, a formula
// — is data, not a live invocation. A body fed to a shell invoker is the
// exception and is judged as the nested script it is, in the directory the
// shell had reached when it read the body (heredocBodyCwd), the same
// distinction matchesPRWorkflowCommand and evaluateDangerousCommand draw
// (gt-ohe8n, gt-1cvqj, gt-v02wh).
//
// A command substitution ($(...) or backticks) is a command line of its own,
// judged like a -c payload even when it sits under a slot-run wrapper, and so
// is a substitution in an unquoted heredoc body: "out=$(go test ./...)" and
// "cat > f <<EOF" holding "$(make gate)" run the suite though no segment word
// is a go or make binary (gt-t1pct, gt-6fkng).
func evaluateContainerSuiteCommand(proc guardProcess, command, envOptIn string) (reason string, matched []string) {
	cwd, _ := proc.getwd()
	return evaluateContainerSuiteCommandDepth(proc, command, cwd, true, envOptIn, 0)
}

// evaluateContainerSuiteCommandDepth judges command as a shell starting in cwd
// — the invocation's directory, or the directory a shell-fed heredoc's body
// runs in, one level down. cwdKnown is false when cwd is only standing in for
// a directory the walk above could not place, as for the -c payload of a
// segment whose cd the guard cannot read: nothing in command is then judged
// against cwd, which is no evidence of where the shell is (gt-jj3or, the
// reading evaluatePolecatTestScopeDepth gives the same handoff, gt-rws47).
func evaluateContainerSuiteCommandDepth(proc guardProcess, command, cwd string, cwdKnown bool, envOptIn string, depth int) (reason string, matched []string) {
	script, err := shellparse.Parse(command)
	if err != nil {
		return containerSuiteUnreadable(proc, err)
	}
	return evaluateContainerSuiteScript(proc, script, cwd, cwdKnown, envOptIn, depth)
}

// containerSuiteUnreadable is the verdict on a command the parser could not
// read. A polecat or refinery session is refused — a line the shell cannot
// parse might still run something, and the guard cannot say what, so it fails
// closed — with a reason naming the construct. Crew and the other agent
// contexts are left unguarded, as the hook leaves every command they run: the
// old lenient tokenizer never refused a line, so they keep that reading.
//
// construct is the name shellparse gives the unreadable construct. The parser
// names what it rejected; a failure the walk itself meets (a $'...' escape
// that decodes to a NUL byte, which mvdan parses but the walk refuses) arrives
// the same way. An error that is not an *shellparse.UnparseableError keeps the
// generic name rather than being echoed, so parser text cannot become the
// refusal.
func containerSuiteUnreadable(proc guardProcess, err error) (string, []string) {
	if !isPolecatOrRefineryContext(proc) {
		return "", nil
	}
	construct := "shell syntax"
	var unparseable *shellparse.UnparseableError
	if errors.As(err, &unparseable) && unparseable.Construct != "" {
		construct = unparseable.Construct
	}
	return fmt.Sprintf("command cannot be read (%s), so it cannot be inspected for a test run", construct), nil
}

// containerSuiteDir is the directory one segment runs in, and whether the walk
// could place it.
type containerSuiteDir struct {
	dir   string
	known bool
}

// containerSuiteScript judges one parsed command: every segment in its
// directory, the shells it starts, the substitutions the shell expands and the
// here-document bodies a shell reads as a script. The line's container opt-in
// (testutil.DockerTestsEnv) is read from its top-level segments.
func evaluateContainerSuiteScript(proc guardProcess, script shellparse.Script, cwd string, cwdKnown bool, envOptIn string, depth int) (reason string, matched []string) {
	dockerOn := containerSuiteDockerOptIn(script) || envOptIn == "1"
	// nestedOptIn is what every script this command runs as a separate shell
	// inherits: the line's own opt-in when it carries one, the hook
	// environment's otherwise. A -c payload, a command substitution and a
	// shell-fed heredoc body are all that same handoff, so a GT_TEST_DOCKER=1
	// before the reader word reaches the body the shell reads (gt-zyccn).
	nestedOptIn := envOptIn
	if dockerOn {
		nestedOptIn = "1"
	}

	tokens, starts := containerSuiteWalkLine(script)
	// A relative cd is placed only against a directory the walk knows.
	base := cwd
	if !cwdKnown {
		base = ""
	}
	tokens, walkFrom, unplacedFrom := containerSuiteCarry(proc, tokens, starts, base)
	vars := shellVarAssignments(tokens)
	dirs := make([]containerSuiteDir, len(script.Segments))
	for i := range script.Segments {
		from := starts[i]
		if at, ok := walkFrom[from]; ok {
			from = at
		}
		dir, known := segmentWalkRoot(proc, tokens, from, cwd, vars)
		// A segment after a directory change that may not run is judged in a
		// directory the walk cannot name, whatever the walk says (gt-zz1t7g).
		known = known && starts[i] <= unplacedFrom
		dirs[i] = containerSuiteDir{dir: dir, known: known && cwdKnown}
	}
	payloadDir := func(i int) (string, bool) {
		dir := dirs[i].dir
		if !dirs[i].known {
			dir = cwd
		}
		return dir, dirs[i].known
	}

	for i, seg := range script.Segments {
		words := containerSuiteWords(seg)
		if r, m := evaluateContainerSuiteSegment(words, dockerOn, dirs[i].dir, dirs[i].known); r != "" {
			return r, m
		}
		if slotRunWrapped(words) {
			continue
		}
		// A command word or nested-shell payload the shell does not pass
		// through as text cannot be read for a test run, so it is refused
		// wherever the guard binds. The container opt-in does not exempt it: a
		// literal `make test` is refused without the opt-in too, and a
		// non-literal word is the same unseen command (gt-j2n8y1).
		if isPolecatOrRefineryContext(proc) {
			if r := containerSuiteNonLiteralRefusal(seg); r != "" {
				return r, nil
			}
		}
		payloads := containerSuiteGuardPayloads(script, i)
		// A trap action, a sourced process substitution and an env -S string
		// are nested shells this bead adds. They are read on the same gate as
		// the non-literal refusal, so crew and the other agent contexts keep
		// the reading they had: the hook never runs the rule for them
		// (gt-j2n8y1).
		if isPolecatOrRefineryContext(proc) {
			payloads = append(payloads, containerSuiteGuardNestedShellPayloads(seg)...)
		}
		if depth >= maxTestGuardNestDepth {
			if len(payloads) > 0 {
				return nestedTooDeepRefusal(true)
			}
			continue
		}
		dir, known := payloadDir(i)
		for _, payload := range payloads {
			if payload.refusal != "" {
				return payload.refusal, nil
			}
			if r, m := evaluateContainerSuiteCommandDepth(proc, payload.text, dir, known, nestedOptIn, depth+1); r != "" {
				return containerSuitePayloadRefusal(payload, r), m
			}
		}
	}

	// A command substitution runs outside any slot-run wrapper that precedes it
	// on the line (`gt slot run -- echo $(go test ./...)`), so it is judged
	// whatever its segment is, in the directory its segment runs in, with the
	// container opt-in the line carries. A substitution whose directory the
	// walk could not place is judged as unplaced, the handoff the payloads get
	// (gt-jj3or). A substitution written in a construct header (a for list, a
	// case pattern) runs in the line's own directory.
	var subs []containerSuiteNested
	for i, seg := range script.Segments {
		for _, sub := range seg.Subs {
			if containerSuiteScriptEmpty(sub) {
				continue
			}
			dir, known := payloadDir(i)
			subs = append(subs, containerSuiteNested{script: sub, dir: dir, known: known})
		}
	}
	for _, sub := range script.Subs {
		if containerSuiteScriptEmpty(sub) {
			continue
		}
		subs = append(subs, containerSuiteNested{script: sub, dir: cwd, known: cwdKnown})
	}

	// A here-document body is data unless a shell reads it as a script: the
	// segment's own command, or a shell anywhere on the reader line the body
	// is fed to ("cat <<EOF | bash"). Only stdin's body feeds a shell its
	// script (gt-ohe8n, gt-hizij). The body runs in the directory the reader
	// segment runs in, so "cd <Go tree> && bash <<EOF" is judged there and a
	// cd after the body is read is not (gt-1cvqj, gt-v02wh).
	var bodies []containerSuiteNested
	for i, seg := range script.Segments {
		if !containerSuiteHeredocReadAsScript(script, i) {
			continue
		}
		dir, known := payloadDir(i)
		for _, h := range seg.Heredocs {
			if h.N != "" && h.N != "0" {
				continue
			}
			if strings.TrimSpace(h.Body) == "" {
				continue
			}
			bodies = append(bodies, containerSuiteNested{dir: dir, known: known, body: h.Body})
		}
	}

	if depth >= maxTestGuardNestDepth {
		return nestedTooDeepRefusal(len(subs) > 0 || len(bodies) > 0)
	}
	for _, sub := range subs {
		if r, m := evaluateContainerSuiteScript(proc, sub.script, sub.dir, sub.known, nestedOptIn, depth+1); r != "" {
			return r, m
		}
	}
	for _, body := range bodies {
		if r, m := evaluateContainerSuiteCommandDepth(proc, body.body, body.dir, body.known, nestedOptIn, depth+1); r != "" {
			return r, m
		}
	}
	return "", nil
}

// dockerTestsEnv mirrors testutil.DockerTestsEnv. It is spelled out here
// rather than imported because internal/testutil pulls testcontainers and
// the Docker client into whatever links it, and this is the gt binary;
// TestDockerTestsEnvMatchesTestutil keeps the two in step.
const dockerTestsEnv = "GT_TEST_DOCKER"

// containerSuiteNested is a command line the guard judges as a separate shell:
// either a parsed substitution (script set) or a here-document body (body set),
// with the directory the shell runs it in. known is false when that directory
// is only standing in for one the walk could not place.
type containerSuiteNested struct {
	script shellparse.Script
	body   string
	dir    string
	known  bool
}

// containerSuiteScriptEmpty reports whether a substitution holds no command,
// the shape "$( )" produces. There is nothing to judge in it.
func containerSuiteScriptEmpty(script shellparse.Script) bool {
	return len(script.Segments) == 0 && len(script.Subs) == 0
}

// containerSuiteDockerOptIn reports whether the command's own top-level
// segments set the container opt-in switch: a leading GT_TEST_DOCKER=1, an
// env(1) assignment or an earlier `export GT_TEST_DOCKER=1` segment. An
// assignment written inside a command substitution is not one of them — it
// sets the variable in that subshell, never on this line — so the outer line
// stays without the switch and the substitution is judged with its own.
func containerSuiteDockerOptIn(script shellparse.Script) bool {
	var tokens []string
	for _, seg := range script.Segments {
		for _, a := range seg.Assigns {
			tokens = append(tokens, a.Text)
		}
		for _, w := range seg.Words {
			tokens = append(tokens, w.Text)
		}
	}
	return commandSetsDockerTests(tokens, "1")
}

// commandSetsDockerTests reports whether any token assigns the container
// opt-in switch the given value (bare, quoted, or as an export/env value).
func commandSetsDockerTests(tokens []string, value string) bool {
	prefix := dockerTestsEnv + "="
	for _, t := range tokens {
		if i := strings.Index(t, prefix); i >= 0 && (i == 0 || t[i-1] == ' ') {
			v := strings.Trim(t[i+len(prefix):], `"'`)
			if v == value {
				return true
			}
		}
	}
	return false
}

// containerSuiteWords is a segment's command and arguments as the tokens the
// policy readers take: quoting resolved, with no assignment or redirection
// among them. A word with a substitution keeps the substitution's source text,
// which is what the package-argument checks read as an expansion.
func containerSuiteWords(seg shellparse.Segment) []string {
	words := make([]string, len(seg.Words))
	for i, w := range seg.Words {
		words[i] = w.Text
	}
	return words
}

// containerSuiteWalkLine rebuilds the shell line the parsed segments came from,
// as the token line segmentWalkRoot reads for directory changes: each segment's
// assignments and words in source order, the operator that ends it
// (Segment.After), and a "(" / ")" pair around the segments a subshell
// encloses so a cd inside one cannot carry. starts[i] is the token index the
// segment begins at, which is the point the walk stops reading at.
func containerSuiteWalkLine(script shellparse.Script) (tokens []string, starts []int) {
	starts = make([]int, len(script.Segments))
	var shells []int
	for i, seg := range script.Segments {
		common := containerSuiteCommonShells(shells, seg.Shells)
		for len(shells) > common {
			tokens = append(tokens, ")")
			shells = shells[:len(shells)-1]
		}
		for len(shells) < len(seg.Shells) {
			tokens = append(tokens, "(")
			shells = append(shells, seg.Shells[len(shells)])
		}
		starts[i] = len(tokens)
		for _, a := range seg.Assigns {
			tokens = append(tokens, a.Text)
		}
		for _, w := range seg.Words {
			tokens = append(tokens, w.Text)
		}
		if seg.After != "" {
			tokens = append(tokens, seg.After)
		}
	}
	for len(shells) > 0 {
		tokens = append(tokens, ")")
		shells = shells[:len(shells)-1]
	}
	return tokens, starts
}

// containerSuiteCarry rewrites the walk line for the directory changes the walk
// (segmentWalkRoot) does not read the way the shell makes them (gt-zz1t7g). It
// returns the rewritten line, walkFrom (a segment's start token mapped to the
// token its directory is walked up to) and unplacedFrom, the index of the first
// directory change a chain element may skip: the segments after it are unknown,
// and the guard refuses them (fail closed). Every token keeps its index, so the
// segment starts still index the line.
//
// Why: "cd A || cd B; go test" runs go in A when the first cd succeeds, and the
// shell skips the right-hand side. A cd that follows "||" or "|", one that "||"
// follows, and a builtin or command cd after a chain element are changes the
// shell may or may not make, so they are not carried.
//
// ACCEPTED RESIDUAL (overseer decision, gt-zz1t7g): a plain cd after "&&" with
// no "||" after it is read as the walk reads any "&&" list. In a pure "&&"
// chain the command after the cd runs only when every cd ran, which the walk
// reads right. The one shape that differs, "X && cd A; go test" with X failing,
// runs go in the old directory; it is accepted because this is an opt-in check
// for the polecat and refinery contexts and saying "skipped" needs the dead-list
// analysis the walk does not have.
func containerSuiteCarry(proc guardProcess, tokens []string, starts []int, base string) (carried []string, walkFrom map[int]int, unplacedFrom int) {
	carried = append([]string(nil), tokens...)
	vars := shellVarAssignments(tokens)
	walkFrom = map[int]int{}
	unplacedFrom = len(carried)
	// One pass in line order: a skipped right-hand side is rewritten before the
	// walk reaches it, so only a directory change that is still a change here
	// is one the shell may run after a chain element.
	for i := 0; i < len(carried); i++ {
		if containerSuiteChainBefore(carried, i) {
			cd := i
			builtin := false
			if (carried[i] == "builtin" || carried[i] == "command") && i+1 < len(carried) && carried[i+1] == "cd" {
				cd = i + 1
				builtin = true
			} else if !dirChangeCommands[carried[i]] {
				continue
			}
			sep := cd + 1 + len(commandArgs(carried, cd))
			orFollows := sep < len(carried) && carried[sep] == "||"
			orBefore := carried[i-1] == "||" || carried[i-1] == "|"
			if !builtin && !orFollows && !orBefore {
				continue
			}
			root, _ := cdWalkRoot(proc, carried, cd, base, vars)
			if _, status := dirChangeTarget(proc, carried[cd], commandArgs(carried, cd), root, vars); status != cdRefused && i < unplacedFrom {
				unplacedFrom = i
			}
			continue
		}
		if (carried[i] == "builtin" || carried[i] == "command") && i+1 < len(carried) && carried[i+1] == "cd" && shellCommandStart(carried, i) {
			carried[i] = ";"
			continue
		}
		if !dirChangeCommands[carried[i]] || !shellCommandStart(carried, i) {
			continue
		}
		args := commandArgs(carried, i)
		sep := i + 1 + len(args)
		if sep >= len(carried) || carried[sep] != "||" {
			continue
		}
		// An unplaced prefix gives an empty root: a relative cd does not resolve
		// here and is left for the walk, which refuses it as unplaced. An
		// absolute cd resolves whatever the prefix, so it carries.
		root, _ := cdWalkRoot(proc, carried, i, base, vars)
		if _, status := dirChangeTarget(proc, carried[i], args, root, vars); status != cdResolved {
			continue
		}
		end := containerSuiteChainEnd(carried, sep+1)
		if end < len(carried) && carried[end] == "&" {
			continue
		}
		carried[sep] = "&&"
		for j := sep + 1; j < end; j++ {
			if carried[j] != "||" && carried[j] != "|" && carried[j] != "(" {
				carried[j] = "true"
			}
		}
		for _, start := range starts {
			if start > sep && start < end {
				walkFrom[start] = i
			}
		}
	}
	return carried, walkFrom, unplacedFrom
}

// containerSuiteChainBefore reports whether the token at i follows "&&", "||"
// or "|": a command there may not run, or runs in a pipeline's subshell.
func containerSuiteChainBefore(tokens []string, i int) bool {
	return i > 0 && (tokens[i-1] == "&&" || tokens[i-1] == "||" || tokens[i-1] == "|")
}

// containerSuiteChainEnd returns the index of the token that ends the
// right-hand side of a "||" whose right-hand side starts at start: the next
// ";", "&&", "&" or ")", or len(tokens). A "|" or a further "||" stays inside
// the right-hand side.
func containerSuiteChainEnd(tokens []string, start int) int {
	for i := start; i < len(tokens); i++ {
		switch tokens[i] {
		case ";", "&&", "&", ")":
			return i
		}
	}
	return len(tokens)
}

// containerSuiteNonLiteralRefusal refuses a segment whose command word, or
// whose -c or eval payload, is a word the shell does not pass through as the
// text the guard reads (shellparse's Literal is false). The command the segment
// runs is then one the guard cannot name, so it cannot be read for a test run.
// The reason names the word as written. The caller applies the polecat or
// refinery gate alone: the container opt-in does not exempt a non-literal
// word, because the word names a command the guard cannot read whatever the
// opt-in (gt-j2n8y1).
//
// The command word is the first word after the shell keywords trimShellKeywords
// steps over, and the -c payload is the word shellCPayloadIndex picks, the same
// words the payload readers read. Redirections are not among seg.Words, so a
// redirection ahead of the command does not hide a non-literal word.
func containerSuiteNonLiteralRefusal(seg shellparse.Segment) string {
	words := containerSuiteWords(seg)
	skip := len(words) - len(trimShellKeywords(words))
	if skip < len(seg.Words) && !seg.Words[skip].Literal {
		return fmt.Sprintf("command word %s is not a literal, so the command it runs cannot be inspected for a test run", seg.Words[skip].Raw)
	}
	lower := toLowerTokens(words)
	for i, word := range lower {
		if !shellInvokers[filepath.Base(word)] {
			continue
		}
		if idx := containerSuiteCPayloadIndex(lower[i+1:]); idx >= 0 && !seg.Words[i+1+idx].Literal {
			return containerSuiteNonLiteralPayload(seg.Words[i+1+idx])
		}
	}
	if skip >= len(seg.Words) {
		return ""
	}
	switch strings.ToLower(filepath.Base(seg.Words[skip].Text)) {
	case "eval":
		for _, w := range seg.Words[skip+1:] {
			if !w.Literal {
				return containerSuiteNonLiteralPayload(w)
			}
		}
	case "trap":
		if idx := containerSuiteTrapActionIndex(seg, skip); idx >= 0 && !seg.Words[idx].Literal {
			return fmt.Sprintf("trap action %s is not a literal, so the command it runs cannot be inspected for a test run", seg.Words[idx].Raw)
		}
	case "source", ".":
		for i := skip + 1; i < len(seg.Words); i++ {
			if _, ok := containerSuiteSoleProcessSubstitution(seg.Words[i]); ok && !seg.Words[i].Literal {
				return fmt.Sprintf("source payload %s is not a literal, so the file it reads cannot be inspected for a test run", seg.Words[i].Raw)
			}
		}
	case "env":
		if idx := containerSuiteEnvSplitIndex(seg, skip); idx >= 0 && !seg.Words[idx].Literal {
			return fmt.Sprintf("env -S payload %s is not a literal, so the command it runs cannot be inspected for a test run", seg.Words[idx].Raw)
		}
	}
	return ""
}

// containerSuiteNonLiteralPayload is the refusal for a -c or eval payload word
// the shell expands before it runs.
func containerSuiteNonLiteralPayload(w shellparse.Word) string {
	return fmt.Sprintf("shell payload %s is not a literal, so the command it runs cannot be inspected for a test run", w.Raw)
}

// containerSuiteGuardPayload is one command line the container rule judges from
// a construct it reads itself, or the refusal for a payload it knows the shell
// runs but cannot expand into text.
type containerSuiteGuardPayload struct {
	text string

	// refusal is non-empty when the shell is known to run a payload whose text
	// the guard cannot produce (a printf escape it cannot expand): the rule
	// refuses rather than judging a payload it does not have.
	refusal string

	// construct names where the payload came from, when a refusal of the
	// payload should say so ("trap 'make test' EXIT" is a trap refusal, not a
	// bare make test).
	construct string
}

// containerSuitePayloadRefusal names the construct a payload came from ahead of
// the refusal its command drew.
func containerSuitePayloadRefusal(p containerSuiteGuardPayload, reason string) string {
	if p.construct == "" {
		return reason
	}
	return fmt.Sprintf("%s payload: %s", p.construct, reason)
}

// containerSuiteGuardPayloads returns the command lines the segment at index
// runs through another shell, in the container rule's reading: a -c payload, an
// eval string and a script the shell reads on stdin or from a script operand
// naming stdin. The dangerous and test-scope guards keep containerSuitePayloads
// and its readers (gt-j2n8y1).
func containerSuiteGuardPayloads(script shellparse.Script, index int) []containerSuiteGuardPayload {
	words := containerSuiteWords(script.Segments[index])
	payloads := containerSuiteGuardCPayloads(words)
	payloads = append(payloads, containerSuiteGuardEvalPayloads(words)...)
	return append(payloads, containerSuiteGuardStdinPayloads(script, index)...)
}

// containerSuiteGuardCPayloads returns the command strings the shell invokers
// in segment run with -c, read through containerSuiteCPayloadIndex.
func containerSuiteGuardCPayloads(segment []string) []containerSuiteGuardPayload {
	lower := toLowerTokens(segment)
	var payloads []containerSuiteGuardPayload
	for i, word := range lower {
		if !shellInvokers[filepath.Base(word)] {
			continue
		}
		if idx := containerSuiteCPayloadIndex(lower[i+1:]); idx >= 0 {
			payloads = append(payloads, containerSuiteGuardPayload{text: segment[i+1+idx]})
		}
	}
	return payloads
}

// containerSuiteGuardEvalPayloads returns the command string the eval a segment
// opens runs.
func containerSuiteGuardEvalPayloads(segment []string) []containerSuiteGuardPayload {
	var payloads []containerSuiteGuardPayload
	for _, text := range evalPayloads(segment) {
		payloads = append(payloads, containerSuiteGuardPayload{text: text})
	}
	return payloads
}

// containerSuiteCPayloadIndex is shellCPayloadIndex's reading for the container
// rule, which knows the option terminator: bash takes the command string its -c
// flag carries from the first operand after "--", so "bash -c -- 'make test'"
// runs make test, not the word "--" (gt-j2n8y1). Every other shape matches
// shellCPayloadIndex, which the other guards keep reading.
func containerSuiteCPayloadIndex(args []string) int {
	wantC := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if wantC && i+1 < len(args) {
				return i + 1
			}
			return -1
		case a == "--rcfile" || a == "--init-file":
			i++ // the option's separate value
		case strings.HasPrefix(a, "--"):
			// a long option such as --norc or --login
		case len(a) > 1 && a[0] == '+':
			i += shellClusterValues(a)
		case len(a) > 1 && a[0] == '-':
			values := shellClusterValues(a)
			payload := i + 1 + values
			i += values
			if strings.Contains(a[1:], "c") {
				if payload < len(args) && args[payload] != "--" {
					return payload
				}
				wantC = true
			}
		default:
			// The first operand is the script file; anything after it belongs
			// to that script, not to the shell.
			return -1
		}
	}
	return -1
}

// containerSuiteGuardNestedShellPayloads returns the command lines the segment
// runs through a construct the guard reads only for a polecat or refinery: a
// trap action, the file a source (or .) reads from a process substitution, and
// an env(1) -S/--split-string string. The caller applies the context gate, so
// crew and the other agent contexts keep the reading they had (gt-j2n8y1).
func containerSuiteGuardNestedShellPayloads(seg shellparse.Segment) []containerSuiteGuardPayload {
	words := containerSuiteWords(seg)
	skip := len(words) - len(trimShellKeywords(words))
	if skip >= len(seg.Words) {
		return nil
	}
	switch strings.ToLower(filepath.Base(seg.Words[skip].Text)) {
	case "trap":
		if idx := containerSuiteTrapActionIndex(seg, skip); idx >= 0 {
			return []containerSuiteGuardPayload{{text: seg.Words[idx].Text, construct: "trap"}}
		}
	case "source", ".":
		return containerSuiteSourcePayloads(seg, skip)
	case "env":
		if idx := containerSuiteEnvSplitIndex(seg, skip); idx >= 0 {
			text := containerSuiteEnvSplitText(seg.Words[idx].Text)
			expanded, ok := containerSuiteEnvSplitEscapes(text)
			if !ok {
				return []containerSuiteGuardPayload{{
					refusal: fmt.Sprintf("env -S payload %s holds a sequence the guard cannot expand, so the command it runs cannot be inspected for a test run", text),
				}}
			}
			return []containerSuiteGuardPayload{{text: expanded, construct: "env -S"}}
		}
	}
	return nil
}

// containerSuiteEnvSplitEscapes expands the escapes env(1) reads in a
// -S/--split-string value, so the command line env builds is the one judged.
// ok is false for a sequence this guard does not expand: env accepts a fixed
// vocabulary and refuses the rest, but a form the guard cannot expand leaves
// what env would run unread, so it refuses rather than guessing.
func containerSuiteEnvSplitEscapes(text string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' || i+1 >= len(text) {
			b.WriteByte(text[i])
			continue
		}
		i++
		switch text[i] {
		case 'c':
			return b.String(), true // env writes nothing after \c
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		case '_':
			b.WriteByte(' ')
		case '"':
			b.WriteByte('"')
		case '\'':
			b.WriteByte('\'')
		case '$':
			b.WriteByte('$')
		default:
			return "", false
		}
	}
	return b.String(), true
}

// containerSuiteTrapActionIndex returns the index in seg.Words of the action a
// trap sets, or -1. The action is the first operand after the options; "-" and
// an empty action reset the trap, which runs nothing.
func containerSuiteTrapActionIndex(seg shellparse.Segment, skip int) int {
	for i := skip + 1; i < len(seg.Words); i++ {
		text := seg.Words[i].Text
		if strings.HasPrefix(text, "-") && len(text) > 1 {
			continue // -p prints, -l lists, -- ends the options
		}
		if text == "-" || text == "" {
			return -1
		}
		return i
	}
	return -1
}

// containerSuiteSourcePayloads returns the script a source (or .) builtin runs
// from each process substitution it names: the substitution is a file the shell
// fills with the producers' output, and the builtin runs that output as its
// script.
func containerSuiteSourcePayloads(seg shellparse.Segment, skip int) []containerSuiteGuardPayload {
	var payloads []containerSuiteGuardPayload
	for i := skip + 1; i < len(seg.Words); i++ {
		sub, ok := containerSuiteSoleProcessSubstitution(seg.Words[i])
		if !ok {
			continue
		}
		for _, p := range containerSuiteGuardProducerPayloadsIn(sub) {
			p.construct = "source"
			payloads = append(payloads, p)
		}
	}
	return payloads
}

// containerSuiteEnvSplitIndex returns the index in seg.Words of the word the
// env(1) -S/--split-string option carries, or -1. A value glued to its option
// ("-S'cmd'", "--split-string=cmd") reports that option word.
func containerSuiteEnvSplitIndex(seg shellparse.Segment, skip int) int {
	for i := skip + 1; i < len(seg.Words); i++ {
		text := seg.Words[i].Text
		switch {
		case text == "--":
			return -1
		case text == "-u" || text == "--unset" || text == "-C" || text == "--chdir":
			i++ // the option's value
		case strings.HasPrefix(text, "--split-string="):
			return i
		case text == "-S" || text == "--split-string":
			if i+1 < len(seg.Words) {
				return i + 1
			}
			return -1
		case strings.HasPrefix(text, "-S") && !strings.HasPrefix(text, "--"):
			return i
		case strings.HasPrefix(text, "-"):
			// another option: -i, -0, -v, --ignore-environment, --null, ...
		case strings.Contains(text, "="):
			// a NAME=value assignment env applies before it runs the command
		default:
			return -1
		}
	}
	return -1
}

// containerSuiteEnvSplitText returns the split string an env -S option word
// carries, whether the value is glued to the option or followed it.
func containerSuiteEnvSplitText(text string) string {
	if value, ok := strings.CutPrefix(text, "--split-string="); ok {
		return value
	}
	if value, ok := strings.CutPrefix(text, "-S"); ok && !strings.HasPrefix(text, "--") {
		return value
	}
	return text
}

// containerSuiteGuardStdinPayloads returns the script text fed to a shell that
// reads stdin, the container rule's reading of containerSuiteStdinPayloads. It
// differs where the shell's own reading differs: a script operand naming stdin
// ("bash /dev/stdin") is stdin, and the last stdin redirection is the one the
// shell uses (gt-j2n8y1).
func containerSuiteGuardStdinPayloads(script shellparse.Script, index int) []containerSuiteGuardPayload {
	seg := script.Segments[index]
	word, args := containerSuiteStdinArgs(seg)
	if !shellInvokers[filepath.Base(strings.ToLower(word))] {
		return nil
	}
	hereStrings, replaced := containerSuiteGuardStdinSources(seg)
	if replaced {
		return nil
	}
	var payloads []containerSuiteGuardPayload
	for _, text := range hereStrings {
		payloads = append(payloads, containerSuiteGuardPayload{text: text})
	}

	// A word that is one process substitution is the script; one with text
	// around it is an operand path.
	var before []string
	var rest []string
	var procSubs []containerSuiteProcSub
	seenSub := false
	for _, arg := range args {
		if sub, ok := containerSuiteSoleProcessSubstitution(arg); ok {
			if !seenSub {
				before = append([]string(nil), rest...)
				seenSub = true
			}
			procSubs = append(procSubs, containerSuiteProcSub{script: sub})
			continue
		}
		rest = append(rest, arg.Text)
	}
	if !seenSub {
		before = rest
	}
	for _, r := range seg.Redirs {
		if r.N != "" && r.N != "0" || r.Op != "<" {
			continue
		}
		if sub, ok := containerSuiteSoleProcessSubstitution(r.Word); ok {
			procSubs = append(procSubs, containerSuiteProcSub{script: sub, onStdin: true})
		}
	}
	_, dashS := containerSuiteGuardReadsStdin(toLowerTokens(before))
	if reads, _ := containerSuiteGuardReadsStdin(toLowerTokens(rest)); !reads {
		return nil
	}
	scripted := false
	for _, sub := range procSubs {
		if dashS && !sub.onStdin {
			continue
		}
		scripted = true
		payloads = append(payloads, containerSuiteGuardProducerPayloadsIn(sub.script)...)
	}
	if scripted {
		return payloads
	}
	for j := index - 1; j >= 0 && script.Segments[j].After == "|"; j-- {
		payloads = append(payloads, containerSuiteGuardProducerPayloads(script.Segments[j])...)
	}
	return payloads
}

// containerSuiteGuardStdinSources returns the text of every here-string on the
// segment's stdin, and whether the shell reads something else there instead. A
// here-string, a here-document and a "< file" all replace stdin, and the shell
// uses the last one written: a "< file" after the here-strings leaves them
// unused, so their text is not the script. A here-document carries no source
// offset, so it cannot be ordered against a here-string; when both are on
// stdin the guard reads the here-string too and the body walk judges the
// here-document, which refuses either order (accepted fail-closed reading, the
// heredoc body being the script in the other order, gt-j2n8y1).
func containerSuiteGuardStdinSources(seg shellparse.Segment) (hereStrings []string, replaced bool) {
	hasHeredoc := false
	for _, h := range seg.Heredocs {
		if h.N == "" || h.N == "0" {
			hasHeredoc = true
		}
	}
	// The redirections are in source order, so the last one that replaces
	// stdin is the one the shell reads.
	lastReplaced := false
	for _, r := range seg.Redirs {
		if r.N != "" && r.N != "0" {
			continue
		}
		switch {
		case r.Op == "<<<":
			lastReplaced = false
		case strings.HasPrefix(r.Op, "<"):
			if _, ok := containerSuiteSoleProcessSubstitution(r.Word); ok {
				continue
			}
			lastReplaced = true
		}
	}
	hereStrings = containerSuiteHereStringPayloads(seg)
	if hasHeredoc {
		if len(hereStrings) > 0 {
			return hereStrings, false
		}
		return nil, true
	}
	if lastReplaced {
		return nil, true
	}
	return hereStrings, false
}

// containerSuiteGuardReadsStdin is shellReadsStdin's reading for the container
// rule: a script operand naming stdin ("bash /dev/stdin", "bash /dev/fd/0")
// makes the shell read the here-string, here-document or pipe this guard
// reads, where shellReadsStdin reads any operand as a script file
// (gt-j2n8y1). The other guards keep shellReadsStdin.
func containerSuiteGuardReadsStdin(args []string) (reads, dashS bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" || a == "-" || a == "/dev/stdin" || a == "/dev/fd/0":
			return true, false
		case a == "--rcfile" || a == "--init-file":
			i++
		case strings.HasPrefix(a, "--"):
		case len(a) > 1 && a[0] == '+':
			i += shellClusterValues(a)
		case len(a) > 1 && a[0] == '-':
			i += shellClusterValues(a)
			if strings.Contains(a[1:], "c") {
				return false, false
			}
			if strings.Contains(a[1:], "s") {
				return true, true
			}
		default:
			redirected, skip, replacesStdin := shellRedirection(a)
			if !redirected || replacesStdin {
				return false, false
			}
			i += skip
		}
	}
	return true, false
}

// containerSuiteGuardProducerPayloads returns the script text a producer
// command line prints, the container rule's reading of
// containerSuiteProducerPayloads. printf expands every escape it writes, so
// "printf 'make\ttest' | bash" is read as the make test it runs; an escape the
// guard cannot expand is refused rather than guessed (gt-j2n8y1).
func containerSuiteGuardProducerPayloads(seg shellparse.Segment) []containerSuiteGuardPayload {
	words := containerSuiteWords(seg)
	producer, printed := segmentCommandWordPastRedirections(trimShellKeywords(words))
	var payloads []containerSuiteGuardPayload
	for _, text := range containerSuiteHereStringPayloads(seg) {
		payloads = append(payloads, containerSuiteGuardPayload{text: text})
	}
	switch filepath.Base(strings.ToLower(producer)) {
	case "printf":
		for len(printed) > 0 && strings.HasPrefix(printed[0], "-") && len(printed[0]) > 1 {
			printed = printed[1:]
		}
		if len(printed) == 0 {
			return payloads
		}
		for _, word := range append([]string{strings.Join(printed, " ")}, printed...) {
			text, ok := containerSuitePrintfEscapes(word)
			if !ok {
				payloads = append(payloads, containerSuiteGuardPayload{
					refusal: fmt.Sprintf("printf payload %s holds an escape the guard cannot expand, so the command it writes cannot be inspected for a test run", word),
				})
				continue
			}
			payloads = append(payloads, containerSuiteGuardPayload{text: text})
		}
	case "echo", "yes":
		for len(printed) > 0 && strings.HasPrefix(printed[0], "-") && len(printed[0]) > 1 {
			printed = printed[1:]
		}
		text := strings.ReplaceAll(strings.Join(printed, " "), `\n`, "\n")
		payloads = append(payloads, containerSuiteGuardPayload{text: text})
		for _, arg := range printed {
			payloads = append(payloads, containerSuiteGuardPayload{text: strings.ReplaceAll(arg, `\n`, "\n")})
		}
	}
	return payloads
}

// containerSuiteGuardProducerPayloadsIn returns the script text the producers
// of every command line in a process substitution print.
func containerSuiteGuardProducerPayloadsIn(script shellparse.Script) []containerSuiteGuardPayload {
	var payloads []containerSuiteGuardPayload
	for _, seg := range script.Segments {
		payloads = append(payloads, containerSuiteGuardProducerPayloads(seg)...)
	}
	return payloads
}

// containerSuitePrintfEscapes expands the escapes bash's printf writes, so the
// text a producer feeds a shell is judged as the shell receives it. ok is false
// when the escape form is one the guard cannot expand (a \x, \u or \U with no
// digit): the caller refuses rather than reading text the shell will not
// produce. An escape printf does not recognize is written as itself, as printf
// writes it.
func containerSuitePrintfEscapes(text string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' || i+1 >= len(text) {
			b.WriteByte(text[i])
			continue
		}
		i++
		switch c := text[i]; c {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		case '\'':
			b.WriteByte('\'')
		case '"':
			b.WriteByte('"')
		case 'c':
			return b.String(), true // printf writes nothing after \c
		case 'x', 'u', 'U':
			max := 2
			if c == 'u' {
				max = 4
			} else if c == 'U' {
				max = 8
			}
			value, digits := 0, 0
			for digits < max && i+1 < len(text) {
				d := containerSuiteHexDigit(text[i+1])
				if d < 0 {
					break
				}
				value = value*16 + d
				digits++
				i++
			}
			if digits == 0 {
				return "", false
			}
			if c == 'x' {
				b.WriteByte(byte(value))
			} else {
				b.WriteRune(rune(value))
			}
		case '0', '1', '2', '3', '4', '5', '6', '7':
			value, digits := 0, 0
			for digits < 3 && i < len(text) {
				d := containerSuiteOctalDigit(text[i])
				if d < 0 {
					break
				}
				value = value*8 + d
				digits++
				i++
			}
			i--
			b.WriteByte(byte(value))
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// containerSuiteHexDigit returns the value of a hexadecimal digit, or -1.
func containerSuiteHexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// containerSuiteOctalDigit returns the value of an octal digit, or -1.
func containerSuiteOctalDigit(c byte) int {
	if c >= '0' && c <= '7' {
		return int(c - '0')
	}
	return -1
}

// containerSuiteCommonShells is the length of the prefix two subshell stacks
// share: the groups still open when the walk reaches the second one.
func containerSuiteCommonShells(a, b []int) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// containerSuitePayloads returns the command lines the segment at index runs
// through another shell, each judged as a command line of its own: a -c
// payload, an eval string and a script the shell reads on stdin. The bd create
// store guard reads its payloads through it too.
func containerSuitePayloads(script shellparse.Script, index int) []string {
	words := containerSuiteWords(script.Segments[index])
	payloads := append(shellCPayloads(words), evalPayloads(words)...)
	return append(payloads, containerSuiteStdinPayloads(script, index)...)
}

// containerSuiteStdinPayloads returns the script text fed to a shell that reads
// stdin. The text comes from a
// here-string on the shell, a here-document on stdin, a process substitution
// the shell runs as its script, or an upstream echo/printf/yes in the same
// pipeline. A redirection that replaces stdin ("< file", "<&3", a here-
// document) feeds the shell something this guard cannot read, so neither the
// pipe nor a here-string is its script (gt-sveq8, gt-hizij).
//
// A process substitution is the script whether the shell opens it as a script
// file or reads it on stdin; the body's output is the script, so each producer
// in it is read the way a pipe's producer is (gt-hizij). The exception is
// "sh -s <(cmd)": -s reads the script from stdin, leaving the substitution as a
// positional parameter and the pipe as the script (gt-n2gt8).
func containerSuiteStdinPayloads(script shellparse.Script, index int) []string {
	seg := script.Segments[index]
	word, args := containerSuiteStdinArgs(seg)
	if !shellInvokers[filepath.Base(strings.ToLower(word))] {
		return nil
	}
	if containerSuiteStdinReplaced(seg) {
		return nil
	}
	payloads := containerSuiteHereStringPayloads(seg)

	// A word that is one process substitution is the script; a word with text
	// around one, or two side by side, is an operand path and stays one.
	var before []string
	var rest []string
	var procSubs []containerSuiteProcSub
	seenSub := false
	for _, arg := range args {
		if sub, ok := containerSuiteSoleProcessSubstitution(arg); ok {
			if !seenSub {
				before = append([]string(nil), rest...)
				seenSub = true
			}
			procSubs = append(procSubs, containerSuiteProcSub{script: sub})
			continue
		}
		rest = append(rest, arg.Text)
	}
	if !seenSub {
		before = rest
	}
	for _, r := range seg.Redirs {
		if r.N != "" && r.N != "0" || r.Op != "<" {
			continue
		}
		if sub, ok := containerSuiteSoleProcessSubstitution(r.Word); ok {
			procSubs = append(procSubs, containerSuiteProcSub{script: sub, onStdin: true})
		}
	}
	_, dashS := shellReadsStdin(toLowerTokens(before))
	if reads, _ := shellReadsStdin(toLowerTokens(rest)); !reads {
		return nil
	}
	scripted := false
	for _, sub := range procSubs {
		if dashS && !sub.onStdin {
			continue
		}
		scripted = true
		payloads = append(payloads, containerSuiteProducerPayloadsIn(sub.script)...)
	}
	if scripted {
		return payloads
	}
	for j := index - 1; j >= 0 && script.Segments[j].After == "|"; j-- {
		payloads = append(payloads, containerSuiteProducerPayloads(script.Segments[j])...)
	}
	return payloads
}

// containerSuiteProcSub is one process substitution the shell runs: the parsed body of
// its command substitution, and whether a redirection ahead of it makes the
// shell read that output on stdin.
type containerSuiteProcSub struct {
	script  shellparse.Script
	onStdin bool
}

// containerSuiteSoleProcessSubstitution returns the parsed substitution a word
// is, when the whole word is one "<(...)" or ">(...)" group. A word with text
// around one ("foo<(cmd)") or two side by side ("<(a)<(b)") is an operand the
// shell passes as a path, never the script it runs, so it is not one.
func containerSuiteSoleProcessSubstitution(w shellparse.Word) (shellparse.Script, bool) {
	if len(w.ProcSubs) != 1 || w.Text != w.ProcSubs[0].Raw {
		return shellparse.Script{}, false
	}
	return w.ProcSubs[0].Script, true
}

// containerSuiteStdinReplaced reports whether a redirection of the segment
// replaces its stdin, so a pipe or a here-string is not what the shell runs.
// A here-document on stdin is one, and judged as a script of its own; a
// process substitution on stdin is the script, judged as one.
func containerSuiteStdinReplaced(seg shellparse.Segment) bool {
	for _, h := range seg.Heredocs {
		if h.N == "" || h.N == "0" {
			return true
		}
	}
	for _, r := range seg.Redirs {
		if r.N != "" && r.N != "0" {
			continue
		}
		if r.Op == "<<<" {
			continue
		}
		if !strings.HasPrefix(r.Op, "<") {
			continue
		}
		if _, ok := containerSuiteSoleProcessSubstitution(r.Word); ok {
			continue
		}
		return true
	}
	return false
}

// containerSuiteStdinArgs is a shell segment's command word and the arguments
// the shell passes it, with the word run a bare here-string consumes left out:
// a run word is the here-string's text, not an operand, so it cannot be read as
// a script file the way a real operand is.
func containerSuiteStdinArgs(seg shellparse.Segment) (string, []shellparse.Word) {
	words := containerSuiteWords(seg)
	word, args := shellCommandWord(words)
	if word == "" || len(args) >= len(words) {
		return word, nil
	}
	// shellCommandWord returns the words after the command word unchanged, so
	// the command word is the one at this index even when a launcher's own
	// value spells a shell's name ("sudo -u bash bash").
	start := len(words) - len(args) - 1
	skip := containerSuiteHereStringRunWords(seg)
	var kept []shellparse.Word
	for i := start + 1; i < len(seg.Words); i++ {
		if !skip[i] {
			kept = append(kept, seg.Words[i])
		}
	}
	return word, kept
}

// containerSuiteProducerPayloadsIn returns the script text the producers of
// every command line in a process substitution print.
func containerSuiteProducerPayloadsIn(script shellparse.Script) []string {
	var payloads []string
	for _, seg := range script.Segments {
		payloads = append(payloads, containerSuiteProducerPayloads(seg)...)
	}
	return payloads
}

// containerSuiteHereStringRunWords marks the words a bare here-string consumes
// as its text: the run after the operator up to the next redirection. A
// here-string glued to its text ("<<<word") has no run.
func containerSuiteHereStringRunWords(seg shellparse.Segment) []bool {
	skip := make([]bool, len(seg.Words))
	for _, r := range seg.Redirs {
		if r.Op != "<<<" {
			continue
		}
		if r.Src >= 0 && r.OpSrc >= 0 && r.Src == r.OpSrc+len(r.Op) {
			continue
		}
		end := containerSuiteNextRedirectionSrc(seg, r.Src)
		for i, w := range seg.Words {
			if w.Src > r.Src && (end < 0 || w.Src < end) {
				skip[i] = true
			}
		}
	}
	return skip
}

// containerSuiteHereStringPayloads returns the text each here-string on the
// segment's stdin feeds it, the parsed counterpart of splitHereStrings. A
// here-string on another descriptor feeds that descriptor, not the script.
// A bare "<<<" takes the words after it as its text: the shell word-splits the
// run up to the next redirection, and the first word is read on its own as
// well, so a body of several words adds a reading rather than replacing the
// one a single word drew (gt-norvx).
func containerSuiteHereStringPayloads(seg shellparse.Segment) []string {
	var payloads []string
	for _, r := range seg.Redirs {
		if r.Op != "<<<" || (r.N != "" && r.N != "0") {
			continue
		}
		if r.Src >= 0 && r.OpSrc >= 0 && r.Src == r.OpSrc+len(r.Op) {
			payloads = append(payloads, r.Word.Text)
			continue
		}
		run := []string{r.Word.Text}
		end := containerSuiteNextRedirectionSrc(seg, r.Src)
		for _, w := range seg.Words {
			if w.Src <= r.Src || (end >= 0 && w.Src >= end) {
				continue
			}
			run = append(run, w.Text)
		}
		payloads = append(payloads, run[0])
		if len(run) > 1 {
			payloads = append(payloads, strings.Join(run, " "))
		}
	}
	return payloads
}

// containerSuiteNextRedirectionSrc is the source offset of the next redirection
// after from, where a here-string's word run stops, or -1 when none follows.
func containerSuiteNextRedirectionSrc(seg shellparse.Segment, from int) int {
	next := -1
	for _, r := range seg.Redirs {
		if r.OpSrc > from && (next < 0 || r.OpSrc < next) {
			next = r.OpSrc
		}
	}
	return next
}

// containerSuiteProducerPayloads returns the script text a producer command
// line prints, and the text of any here-string it is fed: what a shell reading
// its output runs. Only echo, printf and yes print text this guard can read;
// any other producer (a file, a download) yields none.
func containerSuiteProducerPayloads(seg shellparse.Segment) []string {
	words := containerSuiteWords(seg)
	producer, printed := segmentCommandWordPastRedirections(trimShellKeywords(words))
	payloads := containerSuiteHereStringPayloads(seg)
	switch filepath.Base(strings.ToLower(producer)) {
	case "echo", "printf", "yes":
		for len(printed) > 0 && strings.HasPrefix(printed[0], "-") && len(printed[0]) > 1 {
			printed = printed[1:]
		}
		text := strings.ReplaceAll(strings.Join(printed, " "), `\n`, "\n")
		payloads = append(payloads, text)
		for _, arg := range printed {
			payloads = append(payloads, strings.ReplaceAll(arg, `\n`, "\n"))
		}
	}
	return payloads
}

// containerSuiteHeredocReadAsScript reports whether a shell runs the
// here-documents on the segment at index as a script. A here-document is data
// unless a shell invoker runs on its reader line — the segment itself or one
// joined to it by a non-";" operator, so "bash <<EOF" and "cat <<EOF | bash"
// both count while a bash on a later line does not (gt-ohe8n).
func containerSuiteHeredocReadAsScript(script shellparse.Script, index int) bool {
	lo, hi := index, index
	for lo > 0 && containerSuiteLineOperator(script.Segments[lo-1].After) {
		lo--
	}
	for hi+1 < len(script.Segments) && containerSuiteLineOperator(script.Segments[hi].After) {
		hi++
	}
	for j := lo; j <= hi; j++ {
		word, _ := shellCommandWord(containerSuiteWords(script.Segments[j]))
		if shellInvokers[filepath.Base(strings.ToLower(word))] {
			return true
		}
	}
	return false
}

// containerSuiteLineOperator reports whether op keeps two segments on one
// reader line. A ";" or a newline starts a new line, and a here-document is
// read by the line its operator stands on.
func containerSuiteLineOperator(op string) bool {
	switch op {
	case "|", "&&", "||", "&":
		return true
	}
	return false
}

// evaluateContainerSuiteSegment judges a single shell segment (tokens
// between shell operators). tokens is original-case; matching is done on a
// lowercased copy so "Go Test" and "go test" are treated the same. cwd is the
// directory the segment runs in, and cwdKnown whether the walk could place it;
// an unplaced one is refused where the segment's target would be read from it
// (cwdTargetsUnplacedDir), the reading an unplaceable make -C already gets
// (gt-ofj05). The go case is located by findGoSubcommand rather than by go's
// adjacency to "test", so Go's global -C dir flag between the two ("go -C
// /tmp test ./internal/cmd") does not hide the invocation (gt-saorq); any
// other flag between them still ends the walk, so it reads as no invocation.
// That walk also accepts the toolchain's own binary names (go, go1.22.0,
// gotip, isGoBinaryName), and the -C one is where the invocation runs: a bare
// or "." target is judged in the directory that flag names, not in the
// shell's (goTestRunDir, gt-tzdpe).
func evaluateContainerSuiteSegment(tokens []string, dockerOn bool, cwd string, cwdKnown bool) (reason string, matched []string) {
	if len(tokens) == 0 {
		return "", nil
	}
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}

	if slotRunWrapped(tokens) {
		return "", nil // already wrapped by gt slot run
	}

	if i := findGoSubcommand(lower, "test"); i >= 0 && dockerOn {
		runCwd, runCwdKnown := goTestRunDir(tokens, lower, i, cwd, cwdKnown)
		pkgArgs := goTestPackageArgs(tokens[i+1:])
		// An empty runCwd is a directory nothing can be resolved against: the
		// hook's own cwd when getwd failed. A cwd-relative target is refused
		// there rather than exempted, the reading the make rule gives its ""
		// tree (makeTreeDir, makeActsOnWholeGoModule).
		if cwdTargetsUnplacedDir(pkgArgs, runCwdKnown && runCwd != "") ||
			unplacedDirRelativeArg(pkgArgs, runCwdKnown && runCwd != "", containerSuitePackages) {
			return "bare 'go test' names a directory the guard cannot place", nil
		}
		if goDirFlagShellExpanded(tokens, lower, i) && runDirTargetsUnknownDir(pkgArgs) {
			return "bare 'go test' runs in a -C directory the shell expands, which the guard cannot place", nil
		}
		for _, arg := range pkgArgs {
			if packageArgWildcard(arg) && packageWildcardMayReachContainerSuite(arg, runCwd) {
				return fmt.Sprintf("bare 'go test' package argument %q has a wildcard inside a path element that can reach a testcontainers-backed package", arg), nil
			}
		}
		wholeRepo, pkgs := containerSuiteTarget(pkgArgs, runCwd)
		if wholeRepo {
			return "bare 'go test' with a whole-repo target touches every testcontainers-backed package", nil
		}
		if len(pkgs) > 0 {
			return "bare 'go test' targets testcontainers-backed package(s)", pkgs
		}
		return "", nil
	}

	if i := findTestInvocation(lower, "make"); i >= 0 {
		if !makeActsOnWholeGoModule(tokens[i:], makeTreeDir(cwd, cwdKnown)) {
			return "", nil
		}
		// The Go Makefile's "test" target runs "go test ./..." after its
		// shell-script checks — there is no scoped form of "make test", so
		// it always touches every testcontainers-backed package.
		return "bare 'make test' runs 'go test ./...', which always touches testcontainers-backed packages", nil
	}

	return "", nil
}

// containsSubsequence reports whether want appears as a contiguous run
// within tokens (both already lowercased).
func containsSubsequence(tokens, want []string) bool {
	if len(want) == 0 || len(tokens) < len(want) {
		return false
	}
	for i := 0; i+len(want) <= len(tokens); i++ {
		match := true
		for j, w := range want {
			if tokens[i+j] != w {
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

// makeValueFlags are the make(1) short options whose value is mandatory and
// may be the NEXT token ("-C dir", "-f file", "-o file", "-W file", "-I
// dir"), and the long ones. makeRunDir looks a flag up as spelled and
// lowercased, so the lowercase entries match either case. -W and -I are listed
// in their own case because the lowercase -w (print directory) and -i (ignore
// errors) take no value: a lowercased lookup swallowed the -C behind a bare -w
// or -i (gt-i6jw6). -O (output-sync) takes no separate value either, so
// makeRunDir keeps it out of the lowercased lookup. -j and -l take an OPTIONAL
// value, so the next token is a value only when it looks like one (all
// digits). No branch ever swallows the target "test" itself.
var makeValueFlags = map[string]bool{
	"-c": true, "-f": true, "-o": true, "-W": true, "-I": true,
	"--directory": true, "--file": true, "--makefile": true, "--old-file": true, "--assume-old": true,
	"--what-if": true, "--new-file": true, "--assume-new": true, "--include-dir": true,
}
var makeOptionalNumberFlags = map[string]bool{"-j": true, "-l": true}

// findTestInvocation returns the index of the first "<tool> test" invocation
// in tokens (already lowercased) — "go test" or "make test" — or -1.
func findTestInvocation(tokens []string, tool string) int {
	return findInvocation(tokens, tool, "test")
}

// findInvocation returns the index of the first "<tool> <target>" invocation
// in tokens (already lowercased) — e.g. "go test", "go build", "make test",
// "make build" — or -1. For make the target is any goal on the command line,
// not only the first word after the program: options, variable assignments
// and earlier goals may precede it ("make -j4 test", "make -C . test",
// "make GOFLAGS=-count=1 test", "make clean test").
//
// Its go row is the narrow reading — the target must follow the binary
// directly, the adjacency the interim host-hygiene hook's regex recognized
// (idleGateReason, tap_guard_dangerous.go). A guard that judges a go
// invocation reads it through findGoSubcommand instead, which walks the -C
// dir flag.
func findInvocation(tokens []string, tool, target string) int {
	for i := 0; i+1 < len(tokens); i++ {
		if !isInvocationWord(tokens[i], tool) {
			continue
		}
		if tool == "make" {
			// make runs every goal it is given, so the target may follow
			// options, NAME=value assignments and other goals. A flag's
			// value never needs stepping over: the target is never read as
			// one (see makeValueFlags), so "-C test" is judged as the goal.
			// The goals end at the first shell separator: what follows "&&"
			// or ";" is another command, so the "test" in "make build && git
			// add test" is not a goal, while a second make after the
			// separator is found by this loop's next pass (gt-xdcrl).
			for _, tok := range tokens[i+1:] {
				if shellCommandSeparators[tok] {
					break
				}
				if tok == target {
					return i
				}
			}
			continue
		}
		if tokens[i+1] == target {
			return i
		}
	}
	return -1
}

// isInvocationWord reports whether tok names tool. make is also reached by
// path ("/usr/bin/make") and as gmake, GNU make's name on macOS, so its goals
// are read as they are after a bare make (gt-y1zks). Other tools keep the
// exact word: go reaches the guards through findGoSubcommand, which has its
// own binary-name rule.
func isInvocationWord(tok, tool string) bool {
	if tool != "make" {
		return tok == tool
	}
	base := filepath.Base(tok)
	return base == "make" || base == "gmake"
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// goTestValueFlags are go test flags that consume the NEXT token as their
// value in space-separated form (e.g. "-run TestFoo"), as opposed to a
// self-contained flag (-v, -race) or the "-flag=value" form (a single
// token, needs no special handling). Without skipping these, a value like
// "TestFoo" would be mistaken for a package path.
var goTestValueFlags = map[string]bool{
	"-run": true, "-timeout": true, "-count": true, "-tags": true,
	"-cpu": true, "-parallel": true, "-p": true, "-bench": true,
	"-benchtime": true, "-coverprofile": true, "-covermode": true,
	"-coverpkg": true, "-cpuprofile": true, "-memprofile": true,
	"-blockprofile": true, "-mutexprofile": true, "-outputdir": true,
	// The value of these is a pattern or a flag list, not a package: left out,
	// "-skip 'Test*'" would read as a package argument with a wildcard
	// (packageArgWildcard, gt-d1e8c).
	"-skip": true, "-fuzz": true, "-exec": true, "-ldflags": true, "-gcflags": true, "-mod": true,
}

// isGoTestValueFlag reports whether flag (a token's text before any "=") is a
// goTestValueFlags entry. The go command takes every flag with one dash or two
// ("-run X" and "--run X" alike), so the table is read through the one-dash
// spelling (gt-xdcrl).
func isGoTestValueFlag(flag string) bool {
	if strings.HasPrefix(flag, "--") {
		flag = flag[1:]
	}
	return goTestValueFlags[flag]
}

// goTestRunDir returns the directory a "go … test" invocation runs in and
// whether the guard could place it: cwd, moved by each -C dir flag that sits
// between the binary and the subcommand (sub, the index
// findGoSubcommand returned). Go chdirs on -C before it runs anything, so a
// bare or "." target names a package under that directory — judging it
// against the shell's own cwd refuses 'cd /tmp && go -C <container pkg> test
// .' and allows 'go -C /tmp test .' from inside the container package, the
// opposite of what Go runs in each case (gt-tzdpe).
//
// tokens is the segment in original case — the directory argument is read
// from it, because a lowercased path names a different file on a
// case-sensitive filesystem — and lower is the same tokens lowercased, which
// is what the flag and binary matching needs.
//
// The flags compose left to right, each resolved against the directory the
// one before it named, the way make -C chains (makeRunDir). A -C that cannot
// be placed leaves the directory unknown, which keeps the refusal an
// unplaceable make -C gets: a relative -C under a cwd this guard could not
// resolve names nothing it can read, so a bare or "." target is refused
// rather than read as an argument of its own (cwdTargetsUnplacedDir).
func goTestRunDir(tokens, lower []string, sub int, cwd string, cwdKnown bool) (dir string, known bool) {
	bin := goBinaryBefore(lower, sub)
	if bin < 0 {
		return cwd, cwdKnown
	}
	if !cwdKnown {
		cwd = "" // the walk's spelling for a directory it cannot place
	}
	dir, known = cwd, cwdKnown
	for _, arg := range goDirFlagArgs(tokens, lower, bin, sub) {
		resolved, ok := resolveMakeDir(dir, arg)
		if !ok {
			return "", false
		}
		dir, known = resolved, true
	}
	return dir, known
}

// goDirFlagArgs returns the directory each -C flag between the go binary
// (index bin) and the subcommand (index sub) names, in the order go applies
// them. tokens and lower are as in goTestRunDir.
func goDirFlagArgs(tokens, lower []string, bin, sub int) []string {
	var dirs []string
	for i := bin + 1; i < sub; i++ {
		switch {
		case isGoDirFlagValue(lower[i]):
			_, arg, _ := strings.Cut(tokens[i], "=")
			dirs = append(dirs, arg)
		case isGoDirFlag(lower[i]) && i+1 < sub:
			i++ // the next token is the directory this -C names
			dirs = append(dirs, tokens[i])
		}
	}
	return dirs
}

// goDirFlagShellExpanded reports whether a -C dir flag of the go invocation
// names a directory the shell expands before go sees it ("$PWD/internal",
// "~/rig", "$(pwd)"). goTestRunDir leaves the run directory unplaced for it,
// which is enough for a bare or "." target, but a package argument spelled as a
// path under that directory ("./beads") still reads as the module-root package
// "beads" that go never starts, so the container rule refuses it too
// (runDirTargetsUnknownDir, gt-8a1pa).
func goDirFlagShellExpanded(tokens, lower []string, sub int) bool {
	bin := goBinaryBefore(lower, sub)
	if bin < 0 {
		return false
	}
	for _, arg := range goDirFlagArgs(tokens, lower, bin, sub) {
		if shellExpandedPackageArg(arg) {
			return true
		}
	}
	return false
}

// runDirTargetsUnknownDir reports whether a go test package-argument list names
// something under the run directory: a bare "go test", "." and every other
// spelling go resolves against the directory it runs in
// (isCwdRelativeGoPackageArg). It is the question to ask once the run
// directory is one the shell expands (goDirFlagShellExpanded), where each such
// argument could be any package in any tree.
func runDirTargetsUnknownDir(pkgArgs []string) bool {
	if len(pkgArgs) == 0 {
		return true
	}
	for _, arg := range pkgArgs {
		if isCwdRelativeGoPackageArg(arg) {
			return true
		}
	}
	return false
}

// goTestPackageArgs extracts the package-path arguments from the tokens
// following "go test", skipping flags and the values of flags known to
// consume one. Stops at "--", which marks the start of arguments passed
// through to the test binary rather than package paths.
func goTestPackageArgs(rest []string) []string {
	var pkgs []string
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if tok == "--" {
			break
		}
		if strings.HasPrefix(tok, "-") {
			flag := strings.SplitN(tok, "=", 2)[0]
			if isGoTestValueFlag(flag) && !strings.Contains(tok, "=") && i+1 < len(rest) {
				i++
			}
			continue
		}
		pkgs = append(pkgs, tok)
	}
	return pkgs
}

// gastownModulePath is this module's path, as go.mod declares it. A go test
// argument may spell a package with the prefix
// ("github.com/steveyegge/gastown/internal/beads"); the guards compare
// package paths, so it comes off.
const gastownModulePath = "github.com/steveyegge/gastown"

// wholeRepoPackageArg is what a whole-repo wildcard normalizes to: "...",
// "./...", and their module-prefixed spellings all name every package in the
// module.
const wholeRepoPackageArg = "..."

// cwdPackageArg is what an argument naming the invocation's own directory
// normalizes to. "." and "./" both mean the caller's cwd, and so does a bare
// "go test" with no package argument; the guards resolve it against the
// actual cwd (cwdPackagePath) rather than reading it as a wildcard (gt-1lko).
const cwdPackageArg = "."

// normalizeGoPackageArg reduces a go test package argument to the form the
// guards match on: the path is cleaned, then the module prefix and a trailing
// "/..." are stripped, so "./internal/beads/...", "internal/beads/",
// "./internal//beads", "./internal/x/../beads" and
// "github.com/steveyegge/gastown/internal/beads" all normalize to
// "internal/beads". Cleaning is what makes the spelling irrelevant: go reads
// "//", "/./" and "/x/../" the way path.Clean does, and a guard comparing the
// raw text matched none of them (gt-d1e8c). The two spellings that name no
// directory — the whole-repo wildcard and the cwd itself — normalize to
// wholeRepoPackageArg and cwdPackageArg, which the callers must recognize
// before matching.
func normalizeGoPackageArg(arg string) string {
	p := path.Clean(arg)
	p = strings.TrimPrefix(p, gastownModulePath+"/")
	p = strings.TrimSuffix(p, "/...")
	switch p {
	case "", ".":
		return cwdPackageArg
	case "...":
		return wholeRepoPackageArg
	}
	return p
}

// packageArgWildcard reports whether a go test package argument carries a
// wildcard inside a path element: a "..." that is not the argument's trailing
// "/..." ("./internal/b...", "./internal/.../beads"), or a shell glob
// character the hook sees unexpanded ("./internal/b*", "./internal/{beads,x}").
// Such an argument names a set of packages, not one, and
// packageWildcardMayReachContainerSuite decides whether the set can include a
// container package (gt-d1e8c). The whole-repo spellings and the trailing
// "/..." form of a directory are not wildcards in this sense:
// normalizeGoPackageArg reads them.
//
// An argument the shell rewrites at run time is one too (shellExpandedPackageArg,
// gt-e40zb): the hook sees "$PWD/internal/beads" or "./internal/$(echo beads)"
// as written, and the package go is handed is whatever the shell makes of it.
func packageArgWildcard(arg string) bool {
	if strings.ContainsAny(arg, "*?[{") || shellExpandedPackageArg(arg) {
		return true
	}
	p := strings.TrimRight(arg, "/")
	if p == wholeRepoPackageArg {
		return false
	}
	return strings.Contains(strings.TrimSuffix(p, "/..."), "...")
}

// shellExpandedPackageArg reports whether the shell rewrites a package
// argument before go sees it: a "$" (a variable or a $(...) substitution), a
// backtick substitution, or a leading "~" (the home directory, or another
// user's). The hook cannot tell what such an argument becomes, so it is read as
// a set it cannot see into rather than as the literal text, which names no
// package on the list (gt-e40zb). A -C directory is read by the same test
// (resolveMakeDir, gt-8a1pa).
func shellExpandedPackageArg(arg string) bool {
	return strings.ContainsAny(arg, "$`") || strings.HasPrefix(arg, "~")
}

// packageWildcardMayReachContainerSuite reports whether a wildcard package
// argument (packageArgWildcard) can reach an entry of containerSuitePackages,
// for an invocation running in cwd. It answers true wherever it cannot tell:
// the guard fails closed, and refusing a pattern that reaches only light
// packages costs one wrapped run, while allowing one that reaches a container
// package starts the suite unwrapped.
//
// It does not expand the pattern against the tree — the hook sees the text
// before the shell does — it matches it against the list, element by element
// (packagePatternReaches). The argument is read the way every other spelling
// is: an absolute directory by the module-relative path it names, and one
// spelled as a path under the run directory also by the path go resolves it to
// (packageArgsUnderRunDir). A brace expansion is not matched at all, nor is an
// argument the shell expands (shellExpandedPackageArg), and nor is an absolute
// pattern whose module the guard cannot place, because the wildcard may sit in
// the part of the path that says which tree it is.
func packageWildcardMayReachContainerSuite(arg, cwd string) bool {
	return packageWildcardMayReach(arg, cwd, containerSuitePackages)
}

// packageWildcardMayReach is packageWildcardMayReachContainerSuite's rule for
// any list of packages: the polecat test-scope guard asks it of the heavy
// packages (gt-ff2l8).
func packageWildcardMayReach(arg, cwd string, packages []string) bool {
	if strings.Contains(arg, "{") || shellExpandedPackageArg(arg) {
		return true
	}
	placed := moduleRelativePackageArg(arg)
	if filepath.IsAbs(placed) {
		return true
	}
	readings := []string{normalizeGoPackageArg(placed)}
	if runPkg, _ := cwdPackagePath(cwd); runPkg != "" && isCwdRelativeGoPackageArg(arg) {
		readings = append(readings, normalizeGoPackageArg(path.Join(runPkg, arg)))
	}
	for _, pattern := range readings {
		for _, pkg := range packages {
			if packagePatternReaches(pattern, pkg) {
				return true
			}
		}
	}
	return false
}

// packagePatternReaches reports whether a package pattern can reach pkg: each
// of its path elements, up to the shorter of the two paths, matches the
// element of pkg in the same place. Stopping at the shorter one is
// packageArgCovers' rule for a pattern that names a directory containing pkg
// or one inside it. A shell glob never spans a slash, so its element is matched
// against one element of pkg; go's "..." does span one, so an element holding
// it is matched on the text before the "..." and, when that matches, the rest
// is taken to match whatever follows. A pattern path.Match cannot read
// reaches.
func packagePatternReaches(pattern, pkg string) bool {
	patternElems, pkgElems := strings.Split(pattern, "/"), strings.Split(pkg, "/")
	for i := 0; i < len(patternElems) && i < len(pkgElems); i++ {
		elem := patternElems[i]
		prefix, _, spansSlash := strings.Cut(elem, "...")
		if spansSlash {
			elem = prefix + "*"
		}
		matched, err := path.Match(elem, pkgElems[i])
		if err != nil || (matched && spansSlash) {
			return true
		}
		if !matched {
			return false
		}
	}
	return true
}

// moduleRelativePackageArgs maps each absolute directory argument that lies in
// this module to the module-relative spelling the rest of the guard matches on
// ("/work/rig/internal/beads/..." becomes "internal/beads/..."): go test takes
// a directory path as a package argument, and an absolute one names the same
// package as the relative spelling (gt-d1e8c). The module root is found from
// the argument itself (cwdPackagePath), not from the run directory, so a path
// into another worktree of the module is judged by the tree it names. An
// absolute path outside this module names nothing on the list and is left as
// written; so is the module root itself, the repo-root package that is on no
// list, unless it carries the "/..." wildcard, which names every package.
func moduleRelativePackageArgs(pkgArgs []string) []string {
	out := make([]string, len(pkgArgs))
	for i, arg := range pkgArgs {
		out[i] = moduleRelativePackageArg(arg)
	}
	return out
}

// moduleRelativePackageArg is moduleRelativePackageArgs for one argument.
func moduleRelativePackageArg(arg string) string {
	if !filepath.IsAbs(arg) {
		return arg
	}
	dir := filepath.Clean(arg)
	subtree := strings.HasSuffix(dir, "/...")
	dir = strings.TrimSuffix(dir, "/...")
	rel, ok := cwdPackagePath(dir)
	switch {
	case !ok:
		return arg
	case rel != "":
		if subtree {
			return rel + "/..."
		}
		return rel
	case subtree:
		return wholeRepoPackageArg
	}
	return arg
}

// isWholeRepoPackageArg reports whether a normalized package argument (see
// normalizeGoPackageArg) names every package in the module.
func isWholeRepoPackageArg(norm string) bool {
	return norm == wholeRepoPackageArg
}

// packageArgCovers reports whether a normalized package argument reaches pkg:
// the argument names pkg ("internal/beads"), a directory containing it
// ("internal", from "internal/..." or "./internal/..."), or a directory
// inside it ("internal/beads/sub"). Every argument spelling goes through this
// one rule, so a cwd resolved to a package path (cwdContainerPackages,
// cwdHeavyPackages) is judged exactly as the same package named as an
// argument would be — no spelling can dodge a guard the others obey.
func packageArgCovers(arg, pkg string) bool {
	return arg == pkg || strings.HasPrefix(pkg, arg+"/") || strings.HasPrefix(arg, pkg+"/")
}

// normalizedPackageArgs normalizes a go test package-argument list
// (normalizeGoPackageArg), standing in the cwd argument when the list is
// empty: a bare "go test" tests the package of the directory it was started
// in, which is what an explicit "." names.
func normalizedPackageArgs(pkgArgs []string) []string {
	args := make([]string, 0, len(pkgArgs)+1)
	for _, arg := range pkgArgs {
		args = append(args, normalizeGoPackageArg(arg))
	}
	if len(args) == 0 {
		return []string{cwdPackageArg}
	}
	return args
}

// isCwdRelativeGoPackageArg reports whether a go test package argument is
// spelled as a path under the directory the invocation runs in — ".", "..",
// "./beads/...", "../beads" — rather than as an import path. Only that
// spelling moves with the directory: go resolves it against the one it runs
// in, while an import path is already module-absolute, which is why 'go -C
// internal list beads' finds no package where 'go -C internal list ./beads'
// finds this one (gt-g6yi7).
func isCwdRelativeGoPackageArg(arg string) bool {
	return arg == "." || arg == ".." ||
		strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../")
}

// packageArgsUnderRunDir returns the normalized package arguments a go test
// argument list is judged under for an invocation running in the directory
// whose module-relative package path is cwd (cwdPackagePath; "" at the module
// root and outside this module, where the two readings below coincide). Each
// argument comes back as normalizedPackageArgs reads it, and one spelled as a
// path under the run directory — "./beads/...", "../beads" — also under the
// path go resolves that spelling to: 'go -C internal test ./beads/...' and
// 'cd internal && go test ./beads/...' both run internal/beads, while the
// module-root reading of the same argument names a package "beads" that go
// never starts, which let those runs through unwrapped (gt-g6yi7).
//
// Both readings stand, because the guard cannot tell which of them a spelling
// was written for: a run directory it cannot place joins nothing at all, and
// an explicit argument has always carried its module-root reading (gt-saorq).
// A match under either one refuses.
//
// "./..." is the run directory's own subtree, so under "internal/style" it is
// "internal/style" alone: a light package's './...' is not a whole-repo run.
// A path that leaves the module and comes back in ("../rig/internal/cmd" from
// the module root) joins to nothing the lexical readings can see, so the
// directory it lands on is also read off the file system (gt-xdcrl).
func packageArgsUnderRunDir(pkgArgs []string, cwd string) []string {
	runPkg, placed := cwdPackagePath(cwd)
	if !placed || len(pkgArgs) == 0 {
		return normalizedPackageArgs(pkgArgs) // nothing to join the arguments to
	}
	args := make([]string, 0, len(pkgArgs))
	for _, arg := range pkgArgs {
		norm := normalizeGoPackageArg(arg)
		if !isCwdRelativeGoPackageArg(arg) || norm == cwdPackageArg {
			args = append(args, norm)
			continue
		}
		if norm != wholeRepoPackageArg || runPkg == "" {
			args = append(args, norm)
		}
		if runPkg != "" {
			args = append(args, normalizeGoPackageArg(path.Join(runPkg, arg)))
		}
		if landed := moduleRelativePackageArg(filepath.Join(cwd, filepath.FromSlash(arg))); !filepath.IsAbs(landed) {
			args = append(args, normalizeGoPackageArg(landed))
		}
	}
	return args
}

// cwdTargetsUnplacedDir reports whether a go test package-argument list is one
// whose meaning rests on a directory the walk could not place. A bare "go test"
// and an explicit "." both name the directory the command runs in
// (normalizedPackageArgs), and the guard can no more say which package that is
// than it can for an unplaceable make -C; every explicit package argument
// carries its own path, so an unplaced cwd says nothing about it (gt-ofj05).
func cwdTargetsUnplacedDir(pkgArgs []string, cwdKnown bool) bool {
	if cwdKnown {
		return false
	}
	for _, p := range normalizedPackageArgs(pkgArgs) {
		if p == cwdPackageArg {
			return true
		}
	}
	return false
}

// unplacedDirRelativeArg reports whether a go test package-argument list has an
// argument spelled as a path under the run directory ("./beads/...", "./cmd",
// "../x") that the walk could not place and that can name a package in
// packages. go resolves that spelling against the directory it runs in, so
// under an unplaced one "./beads/..." can be internal/beads, yet
// normalizeGoPackageArg reads it as the module-root package "beads", which is
// on no list. cwdTargetsUnplacedDir refuses the arguments that name the
// directory itself; this is the same question for the arguments that name
// something under it (gt-ff2l8).
//
// The unplaced directory is read as any ancestor of a listed package (the
// "cd internal" it stands in for), so "./beads" is refused because
// internal/beads is on the list, and "./style" and "./internal/style" are not,
// which keeps an explicit argument that reaches no listed package on its own
// reading, as an import path does. A parent-relative argument ("../beads")
// can land anywhere, so it is refused.
func unplacedDirRelativeArg(pkgArgs []string, cwdKnown bool, packages []string) bool {
	if cwdKnown {
		return false
	}
	for _, arg := range pkgArgs {
		if !isCwdRelativeGoPackageArg(arg) {
			continue
		}
		norm := normalizeGoPackageArg(arg)
		if norm == cwdPackageArg || norm == wholeRepoPackageArg {
			continue // judged on their own (cwdTargetsUnplacedDir, isWholeRepoPackageArg)
		}
		if norm == ".." || strings.HasPrefix(norm, "../") {
			return true
		}
		for _, pkg := range packages {
			elems := strings.Split(pkg, "/")
			for i := 1; i < len(elems); i++ {
				if packageArgCovers(path.Join(strings.Join(elems[:i], "/"), norm), pkg) {
					return true
				}
			}
		}
	}
	return false
}

// containerSuitePackagesCoveredBy returns the entries of
// containerSuitePackages that a normalized package argument reaches
// (packageArgCovers).
func containerSuitePackagesCoveredBy(arg string) []string {
	var matched []string
	for _, pkg := range containerSuitePackages {
		if packageArgCovers(arg, pkg) {
			matched = append(matched, pkg)
		}
	}
	return matched
}

// cwdContainerPackages returns the entries of containerSuitePackages the
// package at cwd reaches.
func cwdContainerPackages(cwd string) []string {
	pkg, ok := cwdPackagePath(cwd)
	if !ok || pkg == "" {
		return nil
	}
	return containerSuitePackagesCoveredBy(pkg)
}

// containerSuiteTarget reports the entries of containerSuitePackages a go
// test package-argument list reaches, or true for the whole-repo wildcard.
// Arguments naming the cwd are resolved here, through cwdContainerPackages,
// by the same rule as any other argument — one function judges every
// spelling, so no caller can resolve the argument form and miss the cwd form
// (gt-1lko). cwd is the directory the invocation runs in, so an argument
// spelled as a path under it is judged there too (packageArgsUnderRunDir,
// gt-g6yi7), and an absolute directory is judged by the module-relative path
// it names (moduleRelativePackageArgs, gt-d1e8c).
func containerSuiteTarget(pkgArgs []string, cwd string) (wholeRepo bool, matched []string) {
	seen := map[string]bool{}
	for _, p := range packageArgsUnderRunDir(moduleRelativePackageArgs(pkgArgs), cwd) {
		if isWholeRepoPackageArg(p) {
			return true, nil
		}
		covered := containerSuitePackagesCoveredBy(p)
		if p == cwdPackageArg {
			covered = cwdContainerPackages(cwd)
		}
		for _, pkg := range covered {
			if !seen[pkg] {
				seen[pkg] = true
				matched = append(matched, pkg)
			}
		}
	}
	return false, matched
}

// isGastownModule reports whether the file at path is a go.mod declaring
// this module — "module .../gastown" (import-path form) or "module
// gastown" (local form). It lives here rather than in internal/plugin
// because a guard that imports internal/plugin would link testcontainers
// (via the chain from internal/plugin) into the gt binary, which it
// deliberately avoids.
func isGastownModule(goModPath string) bool {
	f, err := os.Open(goModPath) //nolint:gosec // G304: path from traversal
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.HasSuffix(line, "/gastown") || line == "module gastown"
		}
	}
	return false
}

// moduleRootFromCwd walks up from dir to the nearest go.mod and returns its
// directory when that go.mod declares this module, "" otherwise. It reads
// the module declaration rather than the checkout's directory name: the
// refinery runs from /Users/user/gt/gastown/refinery/rig and a polecat
// worktree may be named "rig", so matching the name "gastown" makes the
// lookup miss in the very layouts the guards run in and lets "go test ."
// through (gt-1lko). A nearer go.mod that is a different module — a module
// nested under plugins/, say — ends the walk: nothing under it is a
// gastown package path, so no listed package can be there to block.
func moduleRootFromCwd(dir string) string {
	if dir == "" {
		return ""
	}
	root, found, err := goModuleRoot(dir)
	if err != nil || !found || !isGastownModule(filepath.Join(root, "go.mod")) {
		return ""
	}
	return root
}

// goModuleRoot walks up from dir to the nearest go.mod and returns its
// directory. found is false when the walk reached the filesystem root
// without one; err is non-nil when the walk could not be finished at all,
// which callers read as "unknown" rather than "absent" — a directory that
// cannot be examined (a permission error part-way up, say) is not evidence
// that no module encloses it.
func goModuleRoot(dir string) (root string, found bool, err error) {
	for {
		goMod := filepath.Join(dir, "go.mod")
		_, statErr := os.Stat(goMod)
		switch {
		case statErr == nil:
			return dir, true, nil
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", false, fmt.Errorf("stat %s: %w", goMod, statErr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false, nil
		}
		dir = parent
	}
}

// makeTreeDir is the directory to judge a segment's make invocation from: the
// segment's directory, or "" — makeRunDir's spelling for a directory the guard
// cannot place, which keeps the refusal — when the walk could not place it
// (gt-ofj05).
func makeTreeDir(cwd string, cwdKnown bool) string {
	if !cwdKnown {
		return ""
	}
	return cwd
}

// makeActsOnWholeGoModule reports whether a "make … test" or "make … build"
// invocation runs the module-wide Go action its target name stands for. args
// holds the invocation's tokens from the "make" word onward and cwd is the
// hook process's directory; the pair is what locates the tree, because
// judging the hook's own cwd alone let "make -C <go tree> test" run from
// anywhere (gt-dieu9 review). The container-suite, polecat-scope and idle
// rules all read the premise through this one function (gt-mjfir).
//
// "make test" is "go test ./..." under another name only in a Go tree, and
// the containers the guard protects are started by Go test binaries. A
// directory outside every Go module — the fractals rig, whose test target is
// `npm test` into vitest — runs neither, so refusing its "make test" only
// prints advice that cannot apply (gt-dieu9). The same holds for "make
// build", which is "go build ./..." only in a Go tree. Every Go module keeps
// the refusal, not just this one: the VM the slot guards is shared, and
// another rig's suite reaches it without naming a containerSuitePackages
// entry (beads' `make test`).
//
// What the invocation leaves unpinned keeps the refusal: a directory the
// guard cannot place, a directory it cannot examine, and a makefile it cannot
// read all leave "what does this target do here" unanswered, and "no go.mod
// in sight" is not "there is none" (fail closed). A cwd of "" is that
// unpinned directory: it is what the callers pass for a segment whose cd the
// walk could not resolve (segmentWalkRoot, gt-ofj05).
func makeActsOnWholeGoModule(args []string, cwd string) bool {
	dir, known := makeRunDir(args, cwd)
	if !known || strings.TrimSpace(dir) == "" {
		return true
	}
	_, found, err := goModuleRoot(dir)
	return err != nil || found
}

// makeRunDir resolves the directory a "make … test"/"make … build" invocation
// runs in. args holds the invocation's tokens from the "make" word onward; cwd
// is the hook process's directory. A -C (--directory) is applied before make
// reads anything, and the flags chain, so "make -C a -C b test" runs in a/b.
//
// known is false when the invocation names its own makefile — -f/--file/
// --makefile, or "-" for stdin — because the target then means whatever that
// file says, which the guard cannot read and so refuses instead of guessing.
// It is false too when a -C cannot be placed: a relative directory under an
// unknown cwd names nothing this process can walk, and one that exists but
// cannot be resolved (resolveMakeDir) names nothing the guard can read.
func makeRunDir(args []string, cwd string) (dir string, known bool) {
	dir = cwd
	for i := 1; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			break // options end here; what follows is a target, not a flag
		}
		if tok == "-" {
			return dir, false // "-" is a makefile read from stdin
		}
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		flag, value, hasValue := tok, "", false
		if strings.HasPrefix(tok, "--") {
			// A long option takes its value after "=" ("--directory=rig").
			flag, value, hasValue = strings.Cut(tok, "=")
		} else if len(tok) > 2 {
			// A short one takes it attached, where the rest of the token is
			// the value "=" and all ("-Cgo" is the directory "go", "-C=go"
			// the directory "=go"). Short options cluster, so the option that
			// owns the value may sit behind valueless ones ("-sC dir").
			flag, value, hasValue = splitMakeShortCluster(tok)
		}
		lower := strings.ToLower(flag)

		switch lower {
		case "-c", "--directory":
			if !hasValue {
				if i+1 >= len(args) {
					return dir, false
				}
				i++
				value = args[i]
			}
			resolved, ok := resolveMakeDir(dir, value)
			if !ok {
				return resolved, false
			}
			dir = resolved
			continue
		case "-f", "--file", "--makefile":
			return dir, false
		}

		if hasValue {
			continue
		}
		// -O (--output-sync) takes only an attached value, so the lowercase
		// lookup must not read it as the -o that takes the next word.
		if makeValueFlags[flag] || (flag != "-O" && makeValueFlags[lower]) || (makeOptionalNumberFlags[lower] && i+1 < len(args) && isAllDigits(args[i+1])) {
			// Another option's value; step over it so a value that looks like
			// a flag ("-o -C dir" names the file "-C") is not read as one.
			if i+1 >= len(args) {
				return dir, false
			}
			i++
		}
	}
	return dir, true
}

// splitMakeShortCluster splits a clustered short-option token ("-sC", "-kCdir",
// "-sfother.mk"; tok is longer than two bytes) into the option that takes the
// rest of the token as its value, that value, and whether the value is attached.
//
// It looks for -C and -f, the two options makeRunDir acts on, behind the
// valueless letters before them. Another option that takes a value ends the
// scan first ("-oC" is -o with the value "C", not -C): the rest of the token is
// its value, which makeRunDir steps over. A value-taking letter that ends the
// token ("-so") has no attached value, so its value is the next token and
// makeRunDir steps over that one. A token with neither is read by its first
// letter (gt-zkqtn).
//
// The letters are read as makeRunDir reads a lone option, case-insensitively
// for -C and -f: "-sc dir" and "-sF file" are judged as -C and -f although make
// has no such options, so a spelling make rejects keeps the refusal it would
// have drawn as "-C" (fail closed).
func splitMakeShortCluster(tok string) (flag, value string, hasValue bool) {
	for k := 1; k < len(tok); k++ {
		rest := tok[k+1:]
		switch tok[k] {
		case 'C', 'c', 'f', 'F':
			return "-" + string(tok[k]), rest, rest != ""
		case 'o', 'O', 'W', 'I', 'E', 'j', 'l':
			return "-" + string(tok[k]), rest, rest != ""
		}
	}
	return tok[:2], tok[2:], true
}

// resolveMakeDir applies one -C argument to the directory accumulated so far.
// ok is false when the argument cannot be placed: a relative directory is
// relative to a cwd the guard does not know, and guessing from this process's
// own directory would judge a tree the command never named.
//
// The directory returned is the one the argument really names, symlinks
// resolved, so "-C <link to a Go tree>" is walked as that tree rather than
// under the link's lexical parents (gt-ofj05).
//
// An argument the shell expands (shellExpandedPackageArg: "$PWD/internal",
// "$(pwd)", a backtick, a leading "~") names a directory the hook cannot see:
// joined onto base as written it would be a path that does not exist, which
// resolveDirSymlinks hands back as placed. It is unplaced instead, the way
// `cd $DIR` is cdUnknown (gt-8a1pa).
func resolveMakeDir(base, arg string) (dir string, ok bool) {
	if arg == "" || shellExpandedPackageArg(arg) {
		return base, false
	}
	switch {
	case filepath.IsAbs(arg):
		dir = filepath.Clean(arg)
	case base == "":
		return "", false
	default:
		dir = filepath.Join(base, arg)
	}
	return resolveDirSymlinks(dir)
}

// cwdPackagePath returns the module-relative package path that a "go test ."
// run from dir names — "internal/beads" — by walking up to the enclosing
// go.mod (moduleRootFromCwd). ok is false when dir is empty, unreadable, or
// outside this module, and the path is "" when dir is the module root, whose
// package is the repo root and appears on neither guard's list. An
// unresolvable dir leaves nothing to judge, which matches the guard's
// reading of any other target it cannot place: go cannot run a "." target
// from a directory it cannot name either.
func cwdPackagePath(dir string) (pkg string, ok bool) {
	if dir == "" {
		return "", false
	}
	root := moduleRootFromCwd(dir)
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", true
	}
	return rel, true
}

// printContainerSuiteBlock prints the standard block banner to stderr, naming
// the wrapped form the guard wants instead (gt-e2rs: "message names the wrapped
// form"). It names the two lighter paths as well — the non-container packages
// directly, or letting `gt done`'s gate run the containers — because a refusal
// that names only the hardest path is what sends polecats improvising around it
// (gt-7dxw).
func printContainerSuiteBlock(w io.Writer, reason, originalCommand string, matched []string, gtRole string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ CONTAINER-SUITE COMMAND BLOCKED                              ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(w, "║  Command: %-53s ║\n", truncateStr(originalCommand, 53))
	fmt.Fprintf(w, "║  Reason:  %-53s ║\n", truncateStr(reason, 53))
	if len(matched) > 0 {
		fmt.Fprintf(w, "║  Packages: %-52s ║\n", truncateStr(strings.Join(matched, ", "), 52))
	}
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  This spins Dolt/testcontainers containers on the shared Docker ║")
	fmt.Fprintln(w, "║  VM. Running it bare can collide with another rig's suite.      ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintf(w, "  Run it wrapped instead: %s\n", containerSuiteWrap(originalCommand, gtRole))
	fmt.Fprintln(w, "  Or run the non-container packages directly (they need no slot, and they are")
	fmt.Fprintln(w, "  where your change usually lives) — or run neither and let `gt done` gate the")
	fmt.Fprintln(w, "  container suites for you: its default test-verify gate runs them once you submit.")
	fmt.Fprintln(w, "")
}

// containerSuiteWrap renders the remediation command for a blocked suite: the
// same command run through the container-gate slot. The line is printed for an
// operator to copy and paste, so it has to be exactly what a shell will
// execute — the printed form and the command are the same promise (gt-otvb).
// Three shapes used to break that:
//
//   - A leading VAR=value token is an assignment, not a program. 'gt slot run'
//     has understood one since gt-18nx, but the 'env' form is what the polecat
//     formula prints (mol-polecat-work.formula.toml) and what a gt older than
//     that fix accepts, so it is spelled out rather than relied on.
//   - A compound command cannot simply be prefixed with the wrapper: the wrap
//     would bind to the FIRST segment only, and the shell would run the rest
//     bare — the unwrapped suite this guard exists to prevent. "ls && GOFLAGS=
//     -p=6 make test" printed as one wrap ran 'ls' under the slot and 'make
//     test' outside it, and "export GT_TEST_DOCKER=1; go test ..." also asked
//     exec to run the shell builtin 'export'. Such a line goes to one shell
//     under the slot instead: sh -c '<command>' re-parses it whole, and
//     config.ShellQuote single-quotes it, so it reaches sh as one argument
//     with nothing inside expanded, split, or globbed that the original shell
//     would not have. Every shellCommandSeparator is a character ShellQuote
//     quotes for, so the quoting always fires on this path. sh is the same
//     interpreter the gate's own configured test command goes through
//     (mq_integration.go), not an extra dialect invented here.
//   - The '<rig>/<you>' placeholder is not runnable — '<' reads as a
//     redirection. The caller's own GT_ROLE is already the "rig/role" form the
//     --role flag documents, so it is printed when present and the placeholder
//     is kept only as the fallback for a hook that inherited no role.
func containerSuiteWrap(command, gtRole string) string {
	command = strings.TrimSpace(command)
	role := containerSuiteWrapRole(gtRole)
	if isCompoundShellCommand(command) {
		return fmt.Sprintf("gt slot run --role %s -- sh -c %s", role, config.ShellQuote(command))
	}
	// slot.SplitEnvPrefix is the same rule the runner applies to its own
	// argv (gt-18nx), so the guard's advice and the runner's reading of it
	// cannot drift: whatever it peels is exactly what 'env' has to carry.
	if envAssigns, _ := slot.SplitEnvPrefix(shellTokenize(command)); len(envAssigns) > 0 {
		return fmt.Sprintf("gt slot run --role %s -- env %s", role, command)
	}
	return fmt.Sprintf("gt slot run --role %s -- %s", role, command)
}

// containerSuiteWrapRole is the holder name printed in the remediation's
// --role flag: GT_ROLE when it names a rig-scoped holder ("gastown/refinery",
// "gastown/polecats/zircon"), which is the form `gt slot run --role` documents
// and makes the printed line runnable verbatim. Without one the formula's
// placeholder is printed and the operator fills it in.
func containerSuiteWrapRole(gtRole string) string {
	role := strings.TrimSpace(gtRole)
	if role == "" || !strings.Contains(role, "/") {
		return "<rig>/<you>"
	}
	// A role carrying whitespace or a quote would not survive the line as a
	// single word; refuse it rather than print a command that misreads.
	if strings.ContainsAny(role, " \t\n\"'`$") {
		return "<rig>/<you>"
	}
	return role
}

// isCompoundShellCommand reports whether command chains more than one command
// on a line, so the wrapper must be handed a shell rather than prefixed to it.
func isCompoundShellCommand(command string) bool {
	for _, tok := range shellTokenize(command) {
		if shellCommandSeparators[tok] {
			return true
		}
	}
	return false
}
