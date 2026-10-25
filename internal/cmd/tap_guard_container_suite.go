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
	"github.com/steveyegge/gastown/internal/slot"
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

// containerSuitePackages lists the repo-relative Go import paths whose tests
// spin real Dolt/testcontainers containers (or a shell script that launches
// them), derived (2026-09-11, gt-e2rs; re-verified 2026-09-22, gt-dlzc) by
// finding every package whose TEST BINARY links internal/testutil — the
// only path into container-spinning code (testutil.RequireDoltContainer,
// testutil.WithDolt, testutil.LeaseScratchDoltContainer, all in
// internal/testutil/doltserver.go) — with
// `go list -test -f '{{.ImportPath}} {{join .Deps "\n"}}' ./internal/...`
// and filtering for the module's testutil import path. This catches both
// call-site packages (internal/daemon, internal/mail,
// internal/doltserver, internal/beads, internal/polecat —
// test files calling the entry points) and import-only packages (doctor,
// crew, deacon, ... — the tests import testutil for the hermetic harness,
// and the harness re-exports the opt-in env GT_TEST_DOCKER, so the linked
// entry points CAN run when the switch is on even though the package's own
// test files call no container function). A package whose test binary links
// testutil is on this list even if no current test would actually start a
// container: the cost of a false entry is one unnecessary 'gt slot run'
// wrap (and doctor's tests are the ones that read the Docker daemon's
// capacity anyway, so even they are not wasted); a container-spinning
// package MISSING from this list is invisible to the guard entirely.
var containerSuitePackages = []string{
	"internal/beads",
	"internal/cmd",
	"internal/crew",
	"internal/daemon",
	"internal/deps",
	"internal/doctor",
	"internal/doltserver",
	"internal/git",
	"internal/mail",
	"internal/plugin",
	"internal/polecat",
	"internal/protocol",
	"internal/rig",
	"internal/testutil",
}

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
	// GT_ROLE decides first (see isPolecatContext): a stale GT_POLECAT does not
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

// segmentPayloads returns the command lines the segment starting at
// tokens[start] runs through a shell: -c payloads, an eval string and the
// script fed to a stdin shell. Both test guards judge each as its own command.
func segmentPayloads(tokens []string, start int, segment []string) []string {
	payloads := append(shellCPayloads(segment), evalPayloads(segment)...)
	return append(payloads, stdinShellPayloads(tokens, start, segment)...)
}

// stdinShellPayloads returns the script text fed to a shell that reads stdin
// (`echo 'make gate' | sh`, `sh <<< 'make gate'`), which the segment judge
// otherwise sees as a bare sh. The text comes from a here-string on the shell
// or on any producer upstream in its pipeline, and from the arguments of an
// upstream echo, printf or yes. Every producer counts whatever filters sit
// between it and the shell, since a filter can rewrite the text and reading it
// as the script is the stricter reading (gt-1daus).
//
// A process substitution word on the shell ("sh <(echo 'make gate')", "sh <
// <(echo 'make gate')") is the script itself, whether the shell opens it as its
// script file or reads it on stdin, so it is read like a pipe producer and the
// pipe upstream, which the shell never reads, is not (gt-hizij). The body may
// hold several commands, and the output of each is part of the script. The
// exception is "sh -s <(cmd)": -s reads the script from stdin, so the
// substitution is only an argument and the pipe is still the script
// (gt-n2gt8).
func stdinShellPayloads(tokens []string, start int, segment []string) []string {
	word, args := shellCommandWord(segment)
	if !shellInvokers[filepath.Base(strings.ToLower(word))] {
		return nil
	}
	args, payloads := splitHereStrings(args)
	// -s counts only ahead of the first substitution: after it, "sh <(cmd) -s"
	// runs the substitution as the script file and -s is the file's argument.
	before := args
	for i, arg := range args {
		if strings.HasPrefix(arg, "<(") {
			before = args[:i]
			break
		}
	}
	_, dashS := shellReadsStdin(toLowerTokens(before))
	args, subs := splitProcessSubstitutions(args, tokens[segmentEnd(tokens, start):])
	if reads, _ := shellReadsStdin(toLowerTokens(args)); !reads {
		return nil
	}
	scripted := false
	for _, sub := range subs {
		if dashS && !sub.onStdin {
			continue
		}
		scripted = true
		for _, command := range sub.commands {
			payloads = append(payloads, producerPayloads(command)...)
		}
	}
	if scripted {
		return payloads
	}
	for end := start - 1; end > 0 && tokens[end] == "|"; {
		begin := end
		for begin > 0 && !shellCommandSeparators[tokens[begin-1]] {
			begin--
		}
		payloads = append(payloads, producerPayloads(tokens[begin:end])...)
		end = begin - 1
	}
	return payloads
}

// producerPayloads returns the script text a producer command line prints, and
// the text of any here-string it is fed: what a shell reading its output runs.
// Only echo, printf and yes print text nothing here can read past; any other
// producer (a file, a download) yields none.
func producerPayloads(segment []string) []string {
	producer, printed := segmentCommandWordPastRedirections(trimShellKeywords(segment))
	printed, payloads := splitHereStrings(printed)
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

// splitHereStrings returns args without their "<<<" here-string redirections,
// and the text each one feeds its command's stdin. A here-string on another
// descriptor ("3<<<text", "{fd}<<<text") feeds that descriptor, not the
// script, so it is dropped without its text (gt-hizij).
//
// A bare "<<<" takes the words after it as its text: the shell word-splits an
// unquoted body, so the run of words up to the next redirection is the script.
// Reading only its first word left "bash <<< bd create --title=x" unjudged
// (gt-norvx); that word is still read on its own, so a body of several words
// adds a reading rather than replacing the one a single word drew.
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

// processSubstitution is one "<(...)" word on a shell: the command lines inside
// it, and whether a "<" ahead of it makes the shell read it on stdin.
type processSubstitution struct {
	commands [][]string
	onStdin  bool
}

// segmentEnd returns the index of the separator that ends the segment starting
// at tokens[start], or len(tokens) for the last segment.
func segmentEnd(tokens []string, start int) int {
	for end := start; end < len(tokens); end++ {
		if shellCommandSeparators[tokens[end]] {
			return end
		}
	}
	return len(tokens)
}

// splitProcessSubstitutions returns args without their "<(...)" words, and what
// each one holds. A bare "<" or "0<" ahead of one is dropped with it:
// "sh < <(cmd)" reads the substitution on stdin. after holds the tokens behind
// the segment args came from, separators included: a separator inside the body
// ends the segment, so "sh <(echo a; echo 'make test')" reaches here as
// "sh <(echo a" and the rest of the body is the next segment. The body is cut
// into command lines at those separators.
//
// The substitution closes at the first word that ends in ")" with the body's
// parentheses back to level. A word that holds a pair of its own ("echo
// $(date)") does not close it; the quote marks are gone by now, so the balance
// is all that is left to read; a quoted parenthesis ("echo ')'") skews it, and
// TestStdinShellPayloadsMultiCommandProcessSubstitution pins that such a body
// is still judged. One that never closes runs to the end of the
// line, so a line the shell would reject is still judged (gt-hizij, gt-n2gt8).
func splitProcessSubstitutions(args, after []string) (rest []string, subs []processSubstitution) {
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "<(") {
			rest = append(rest, args[i])
			continue
		}
		var sub processSubstitution
		if n := len(rest); n > 0 && (rest[n-1] == "<" || rest[n-1] == "0<") {
			rest = rest[:n-1]
			sub.onStdin = true
		}
		words := append(append([]string(nil), args[i:]...), after...)
		var command []string
		endCommand := func() {
			if len(command) > 0 {
				sub.commands = append(sub.commands, command)
				command = nil
			}
		}
		depth, used := 1, len(words)
		for j, word := range words {
			if j > 0 && shellCommandSeparators[word] {
				endCommand()
				continue
			}
			if j == 0 {
				word = strings.TrimPrefix(word, "<(")
			}
			depth += strings.Count(word, "(") - strings.Count(word, ")")
			closed := depth <= 0 && strings.HasSuffix(word, ")")
			word = strings.TrimSuffix(word, ")")
			if word != "" {
				command = append(command, word)
			}
			if closed {
				used = j + 1
				break
			}
		}
		endCommand()
		subs = append(subs, sub)
		i += min(used, len(args)-i) - 1
	}
	return rest, subs
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

// nestedScript is a command line a shell runs inside the command being judged,
// with the directory it runs in (the base when known is false).
type nestedScript struct {
	body  string
	dir   string
	known bool
}

// substitutionScripts returns the body of every $(...) and backtick command
// substitution the shell expands in command, each with the directory it runs
// in. The segment judges tokenize "out=$(go test ./internal/cmd)" as the words
// "out=$(go", "test", ... and so never see a go test, and "echo $(make gate)"
// as an echo, so the bodies are judged as command lines of their own the way
// evaluateDangerousCommand does (gt-t1pct).
//
// commandSubstitutionSpans cuts each body with the parentheses balanced and
// the quotes respected, so "$(grep -E '(ok|FAIL)' out)" and "$(tail -n $(nproc))"
// are judged whole (gt-wrr5c). A substitution nested in a body is judged when
// the body is, one nesting level deeper.
//
// A substitution runs in the directory the cds ahead of it on the same shell
// line leave the shell in, found by the walk segmentWalkRoot gives a segment.
// A "$(" the shell leaves literal is skipped: after a backslash, or inside
// single quotes, where a bead note spelling "$(make gate)" is data. Heredoc
// bodies are stripped first, as the segment judges do, and the substitutions of
// an unquoted body are added back (heredocSubstitutionScripts).
func substitutionScripts(proc guardProcess, command, cwd string, cwdKnown bool) []nestedScript {
	text := stripHeredocBodies(command)
	var scripts []nestedScript
	for _, span := range commandSubstitutionSpans(text) {
		body := substitutionBody(text, span)
		if strings.TrimSpace(body) == "" {
			continue
		}
		tokens := shellTokenize(text[:span.open])
		segStart := 0
		for i, tok := range tokens {
			if shellCommandSeparators[tok] {
				segStart = i + 1
			}
		}
		dir, known := segmentWalkRoot(proc, tokens, segStart, cwd, shellVarAssignments(tokens))
		if !known || !cwdKnown {
			dir, known = cwd, false
		}
		scripts = append(scripts, nestedScript{body: body, dir: dir, known: known})
	}
	return append(scripts, heredocSubstitutionScripts(proc, command, cwd, cwdKnown)...)
}

// heredocSubstitutionScripts returns the substitutions written in the bodies of
// unquoted heredocs, each with the directory the shell is in when it reads the
// body. stripHeredocBodies drops those bodies before substitutionScripts scans
// the line, yet the shell expands "$(make gate)" in "cat > f <<EOF" while
// reading the body, so the suite runs though no segment word is a make. A quoted
// tag keeps its body literal, and an unterminated heredoc is not stripped, so
// substitutionScripts already scanned it (gt-6fkng, gt-x50o8).
func heredocSubstitutionScripts(proc guardProcess, command, cwd string, cwdKnown bool) []nestedScript {
	var scripts []nestedScript
	for _, span := range scanHeredocs(command) {
		if span.bodyStart < 0 || span.quoted || span.unterminated {
			continue
		}
		dir, placed := heredocBodyCwd(proc, command, span, cwd)
		known := placed && cwdKnown
		if !known {
			dir = cwd
		}
		for _, sub := range heredocSubstitutionSpans(span.body) {
			// substitutionBody drops the backslash an escaped nested backtick
			// pair carries, so the nested command the body's own parse runs is
			// judged here as it is in substitutionScripts (gt-h3312).
			body := substitutionBody(span.body, sub)
			if strings.TrimSpace(body) == "" {
				continue
			}
			scripts = append(scripts, nestedScript{body: body, dir: dir, known: known})
		}
	}
	return scripts
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
// A heredoc body in the substitution is stepped over whole, as scanHeredocs
// judges it, so the quote marks and parentheses in it count for nothing
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
		// text. A span scanHeredocs placed somewhere this walk reads as quoted
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
// order, as scanHeredocs judges the whole text. An unterminated body is left out:
// it runs to the end of the text, which would hide the ")" that closes the
// substitution (gt-lx8i8).
func heredocBodiesFrom(text string, from int) []heredocSpan {
	if !strings.Contains(text[from:], "<<") {
		return nil
	}
	var bodies []heredocSpan
	for _, span := range scanHeredocs(text) {
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
// is a go or make binary (gt-t1pct, gt-6fkng, substitutionScripts).
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
	tokens := shellTokenize(strings.TrimSpace(stripHeredocBodies(command)))
	// Container-backed tests are opt-in (testutil.DockerTestsEnv). A bare
	// `go test` of a container-bearing package cannot start a container
	// unless the command turns the switch on somewhere — as an env prefix,
	// via env(1), or an earlier `export` on the same line — so only then is
	// the slot required. `make test` sets it in the Makefile and is judged
	// unconditionally. The switch is looked for across the WHOLE command,
	// not per segment, because `export X=1; go test ...` enables it for the
	// later segment. An opt-in already exported in the hook's environment
	// counts too: the test process inherits it without it being typed.
	dockerOn := commandEnablesDockerTests(tokens) || envOptIn == "1"
	vars := shellVarAssignments(tokens)
	// nestedOptIn is what every script this command runs as a separate shell
	// inherits: the line's own opt-in when it carries one, the hook
	// environment's otherwise. A -c payload, a command substitution and a
	// shell-fed heredoc body are all that same handoff, so a GT_TEST_DOCKER=1
	// before the reader word reaches the body the shell reads
	// (gt-zyccn).
	nestedOptIn := envOptIn
	if dockerOn {
		nestedOptIn = "1"
	}

	// judge covers one segment and the -c payloads of the shells it starts. A
	// payload runs in the segment's directory and inherits the line's container
	// opt-in; a slot-run wrapper exempts its payload with the rest of the
	// segment. A directory the walk cannot place is carried to the payload as
	// unplaced rather than read as the base, so a payload that names it
	// ("bash -c 'go test ./beads'") is refused (gt-jj3or).
	judge := func(segment []string, start int) (string, []string) {
		segment = commandSegment(segment)
		dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
		known = known && cwdKnown
		if r, m := evaluateContainerSuiteSegment(segment, dockerOn, dir, known); r != "" {
			return r, m
		}
		if slotRunWrapped(segment) {
			return "", nil
		}
		payloads := segmentPayloads(tokens, start, segment)
		if depth >= maxTestGuardNestDepth {
			return nestedTooDeepRefusal(len(payloads) > 0)
		}
		if !known {
			dir = cwd
		}
		for _, payload := range payloads {
			if r, m := evaluateContainerSuiteCommandDepth(proc, payload, dir, known, nestedOptIn, depth+1); r != "" {
				return r, m
			}
		}
		return "", nil
	}

	var segment []string
	start := 0
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := judge(segment, start); r != "" {
				return r, m
			}
			segment = nil
			start = i + 1
			continue
		}
		segment = append(segment, tok)
	}
	if r, m := judge(segment, start); r != "" {
		return r, m
	}

	spans := shellFedHeredocSpans(command)
	subs := substitutionScripts(proc, command, cwd, cwdKnown)
	if depth >= maxTestGuardNestDepth {
		return nestedTooDeepRefusal(len(spans) > 0 || len(subs) > 0)
	}
	// A command substitution runs outside any slot-run wrapper that precedes
	// it on the line (`gt slot run -- echo $(go test ./...)`), so it is judged
	// whatever its segment is, with the container opt-in the line carries. A
	// substitution whose directory the walk could not place is judged as
	// unplaced, the handoff the payload above gets (gt-jj3or).
	for _, sub := range subs {
		if r, m := evaluateContainerSuiteCommandDepth(proc, sub.body, sub.dir, sub.known, nestedOptIn, depth+1); r != "" {
			return r, m
		}
	}
	// The bodies come off the untouched command: stripHeredocBodies removed
	// them from the text above, so they are judged here once, as code, in the
	// directory the shell had reached by the time it read them
	// (heredocBodyCwd, gt-1cvqj, gt-v02wh). A body whose directory the walk
	// cannot place is judged as unplaced, as heredocSubstitutionScripts
	// already judges the substitutions inside one (gt-jj3or).
	for _, span := range spans {
		bodyCwd, placed := heredocBodyCwd(proc, command, span, cwd)
		known := placed && cwdKnown
		if !known {
			bodyCwd = cwd
		}
		if r, m := evaluateContainerSuiteCommandDepth(proc, span.body, bodyCwd, known, nestedOptIn, depth+1); r != "" {
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

// commandEnablesDockerTests reports whether any token sets the container
// opt-in switch to 1 (GT_TEST_DOCKER=1, bare, quoted, or as an export/env
// value). The hook's own environment is checked by the caller: an exported
// GT_TEST_DOCKER=1 in the session reaches `go test` without appearing in
// the command text.
func commandEnablesDockerTests(tokens []string) bool {
	return commandSetsDockerTests(tokens, "1")
}

// commandSetsDockerTests reports whether any token assigns the container
// opt-in switch the given value (bare, quoted, or as an export/env value).
// It is commandEnablesDockerTests' parser, shared with gt done's
// --pre-verified slot decision, which also needs to see an explicit "0".
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
// refinery runs from /Users/sloan/gt/gastown/refinery/rig and a polecat
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
