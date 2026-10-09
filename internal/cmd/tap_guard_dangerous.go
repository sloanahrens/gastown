package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
    crew/refinery session, not a polecat one (gt-deff).
  - git reset --hard
  - git reset <remote-tracking-ref>  (--soft/--mixed/--hard/implicit: resetting
    onto origin/main etc. reverts everything merged since the checkout was cut
    and, for the tree-carrying modes, destroys uncommitted work — see gt-63sz)
  - git clean with a force flag: -f, -fd, -fdx, --force
  - drop table/database
  - truncate table
  - find/bfs/fd/rg/grep -r/du/ls -R rooted at /, ~, $HOME, /Users, /System,
    /Library, or /opt (see gt-nqcy — an unbounded 'bfs /' froze a host)
  - the same walkers rooted at the town tree: the town root, any rig root,
    any path directly under the town root, a rig's worktree directories
    (polecats, crew, and the mayor/ clone), or any .repo.git — see gt-6e2l,
    where a dog's 'grep -R ... /Users/sloan/gt' ran unblocked and walked
    every rig and every worktree on the host. A path inside a single repo or
    worktree (e.g. ~/gt/<rig>/polecats/<name>/<repo>) is still allowed, as is
    a search pattern that shares a name with a directory there: 'grep -rn
    polecats ./docs' searches ./docs, not the polecats/ directory at cwd
    (gt-yts7).
  - go clean -cache/-testcache/-modcache/-fuzzcache (wipes the Go build
    cache SHARED by every agent on the host — see gt-nqcy follow-up).
  - 'go test -count=1 ./...' (with or without -json, any flag order) from a
    polecat session: an uncached whole-module run recompiles and relinks every
    package and throws the results away, saturating the shared host. Two such
    runs plus a landing gate on 10-03 drove load to 45-55 and stretched the
    gate from ~30s to 5m02s (gt-v4r0x). A seat submits with 'make presubmit',
    which tests only the packages the branch changed; the rig's Forgejo CI
    gate runs the full gate on the candidate branch. Scoped runs
    (./internal/<pkg>/..., a -run filter) and cached 'go test ./...' stay
    allowed.
  - a loop or watcher around 'gt done' or 'gt slot': a for/while/until block
    whose body invokes either, an xargs/watch/seq beside either, or a heredoc
    body written to a file that contains either shape. 'gt done' waits for the
    container-gate slot itself and is bounded; the improvised polling loop it
    replaces held the gate everyone else was queued behind (gt-7dxw). Every
    role, every cwd.

This guard also HOLDS (rather than permanently blocks) a full-suite start —
'make test' and 'make build' in a Go tree, 'go test ./...' and 'go build
./...' — when the host is already busy, unless the command is wrapped in
'gt slot run'. Re-running the exact same command after a short wait passes
once the host frees up. See gt-nqcy follow-up. The tree a held segment is
judged in is the one it runs in: a cd or pushd earlier on the same shell line
carries into the segments after it ("&&"/";" only, as the shell does), so an
agent whose cwd is a non-Go tree is held for 'cd <go tree> && make test'
rather than stepping around the gate from outside the Go tree (gt-me4vs,
gt-n7ksl). A popd takes its directory from a stack the walk does not track, so
it leaves the directory unknown rather than naming one.

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

func runTapGuardDangerous(cmd *cobra.Command, args []string) error {
	return tapGuardDangerous(os.Stdin, os.Stderr, realGuardProcess())
}

// tapGuardDangerous is the dangerous-command guard: it reads the hook payload
// from stdin (Claude Code protocol) and the session from proc, and prints a
// block to stderr.
func tapGuardDangerous(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
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
	sess := guardSession{proc: proc, townRoot: currentTownRoot(proc)}
	if reason, alternative := evaluateDangerousCommand(command, 0, sess); reason != "" {
		if alternative != "" {
			printDangerousBlockWithAlternative(stderr, reason, command, alternative)
		} else {
			printDangerousBlock(stderr, reason, command)
		}
		return NewSilentExit(2)
	}

	if load1, heldAction := evaluateIdleGate(proc, command, proc.load1); heldAction != "" {
		printIdleGateHold(stderr, load1, command, heldAction)
		return NewSilentExit(2)
	}

	return nil
}

// evaluateIdleGate reports whether command is a full-suite start
// (idleGateHeldAction) that should be held because the sampled 1-minute load
// average is above idleGateLoad1Threshold. load1 is the sampled value (0 when
// the command isn't gated or the sample failed — a failed sample fails open,
// never holding the command). heldAction is the held invocation's own spelling
// ("make test", "go build ./..."), returned so the HOLD banner can name what it
// held instead of guessing; "" means the command was not held. proc supplies
// the working directory each segment is judged in (idleGateHeldAction), which
// decides whether a make target is the whole-module Go action at all.
func evaluateIdleGate(proc guardProcess, command string, hostLoad1 func() (float64, bool)) (load1 float64, heldAction string) {
	heldAction = idleGateHeldAction(proc, command)
	if heldAction == "" {
		return 0, ""
	}
	load1, ok := hostLoad1()
	if !ok {
		return 0, ""
	}
	if load1 <= idleGateLoad1Threshold {
		return load1, ""
	}
	return load1, heldAction
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
// positives have come from (gt-5ihs attempt 2, gt-wisp-db27 finding 4).
func evaluateDangerousCommand(command string, depth int, sess guardSession) (reason, alternative string) {
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
	if r, alt := matchesPolecatMainPush(lowerTokens, inPolecatSession(sess.proc)); r != "" {
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
	if r, alt := matchesPolecatFullSuiteUncached(lowerTokens, inPolecatSession(sess.proc)); r != "" {
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
	if r, alt := matchesUnboundedScan(tokens, sess); r != "" {
		return r, alt
	}

	if r := matchesGitResetHard(tokens); r != "" {
		return r, ""
	}
	if r := matchesDDLDestruction(tokens); r != "" {
		return r, ""
	}

	if depth >= maxDangerousNestDepth {
		return "", ""
	}
	nested := nestedCommands(tokens, lowerTokens)
	nested = append(nested, commandSubstitutions(command)...)
	nested = append(nested, shellFedBodies...)
	for _, n := range nested {
		if r, alt := evaluateDangerousCommand(n, depth+1, sess); r != "" {
			return r, alt
		}
	}
	return "", ""
}

// shellInvokers are commands whose "-c" argument is itself a nested shell
// command string, not a plain argument.
var shellInvokers = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// shellCPayloadIndex returns the index, within the arguments that follow a
// shell invoker, of the command string its -c flag carries, or -1 when the
// invocation has none. -c may sit in a short-flag cluster ("-lc", "-ec",
// "-ic"), after other options ("-x -c", "--norc -c") or after "-o name", and
// the invoker may be a path ("/bin/bash"). The first operand is a script file:
// a "-c" after it belongs to that script, not to the shell (gt-kocid).
func shellCPayloadIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return -1
		case a == "-o" || a == "+o" || a == "--rcfile" || a == "--init-file":
			i++ // the option's separate value
		case strings.HasPrefix(a, "--"):
			// a long option such as --norc or --login
		case len(a) > 1 && a[0] == '+':
			// +x style option negation
		case len(a) > 1 && a[0] == '-':
			if strings.Contains(a[1:], "c") {
				if i+1 < len(args) {
					return i + 1
				}
				return -1
			}
		default:
			return -1
		}
	}
	return -1
}

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
		if shellInvokers[filepath.Base(lt)] {
			if idx := shellCPayloadIndex(lowerTokens[i+1:]); idx >= 0 {
				nested = append(nested, tokens[i+1+idx])
			}
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
// line that owns it (the operator line with the operator removed), the offset
// that line starts at, the body text, and the body's byte offsets. bodyStart
// is -1 when the operator line is the command's last line, so no body follows.
// readerStart lets a caller reach the text the shell had already run by the
// time it reads the body, without re-finding the operator (gt-v02wh).
type heredocSpan struct {
	reader      string
	readerStart int
	body        string
	bodyStart   int
	bodyEnd     int
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
		reader, readerStart := heredocReaderLine(command, start, end)

		nl := strings.IndexByte(command[end:], '\n')
		if nl < 0 {
			// No body follows on a later line (e.g. the heredoc marker is
			// the last thing on the line with nothing after it).
			spans = append(spans, heredocSpan{reader: reader, readerStart: readerStart, bodyStart: -1, bodyEnd: end})
			stripped = end
			continue
		}
		bodyStart := end + nl + 1
		bodyEnd := heredocTerminatorEnd(command, bodyStart, tag, allowIndent)
		spans = append(spans, heredocSpan{
			reader:      reader,
			readerStart: readerStart,
			body:        command[bodyStart:bodyEnd],
			bodyStart:   bodyStart,
			bodyEnd:     bodyEnd,
		})
		stripped = bodyEnd
	}
	return spans
}

// heredocReaderLine returns the text of the line the heredoc operator at
// [start,end) sits on, with the operator itself removed, and the offset that
// line starts at. The text after the operator stays: in "cat <<EOF | bash"
// the pipeline's command words, not the words before the operator, decide who
// consumes the body. The offset is the start of the whole logical line, so a
// caller can read the command text preceding it (gt-v02wh).
func heredocReaderLine(command string, start, end int) (string, int) {
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
	return strings.ReplaceAll(command[lineStart:start]+" "+command[end:lineEnd], "\\\n", " "), lineStart
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
	for _, span := range shellFedHeredocSpans(command) {
		bodies = append(bodies, span.body)
	}
	return bodies
}

// shellFedHeredocSpans is shellFedHeredocBodies' span-returning sibling: the
// same shell-fed heredocs in the same order, each keeping the reader line that
// owns it and where that line starts, so a caller can judge the body in the
// directory the shell had reached by the time it read the body (gt-1cvqj,
// gt-v02wh).
func shellFedHeredocSpans(command string) []heredocSpan {
	var spans []heredocSpan
	for _, span := range scanHeredocs(command) {
		if span.bodyStart < 0 || strings.TrimSpace(span.body) == "" {
			continue
		}
		if heredocReaderIsShellInvoker(span.reader) {
			spans = append(spans, span)
		}
	}
	return spans
}

// heredocBodyDir returns the directory a shell-fed heredoc's body runs in:
// base — the directory the invocation was in — moved by the cds the shell
// runs before it hands the body over. "cd /go/tree && bash <<EOF" runs the
// body in /go/tree, and judging it in the hook's own directory instead let
// that cd step around the test guards (gt-1cvqj). A cd the walk cannot place
// leaves the body where it already was, the reading scanWalkRoot gives an
// unknown walk root: an unplaceable directory is not evidence that the body
// runs in a guarded tree.
func heredocBodyDir(proc guardProcess, command string, span heredocSpan, base string) string {
	if dir, ok := heredocBodyCwd(proc, command, span, base); ok {
		return dir
	}
	return base
}

// heredocBodyCwd is the resolution behind heredocBodyDir: the directory the
// shell is in by the time the body is read, and whether the walk could place
// it. The walk's two unnamed outcomes read the same here: a body whose
// directory the walk cannot name keeps the one the invocation already had.
func heredocBodyCwd(proc guardProcess, command string, span heredocSpan, base string) (string, bool) {
	tokens := heredocBodyWalkTokens(command, span)
	dir, status := cdWalkRoot(proc, tokens, len(tokens), base, shellVarAssignments(tokens))
	return dir, status == cdWalkPlaced
}

// heredocBodyWalkTokens returns the tokens the shell has run by the time it
// reads a heredoc body: the command text preceding the reader line — bodies
// stripped first, so a cd written inside a data body is data and never enters
// the walk — followed by the reader line's own words up to the shell invoker
// that reads the body.
//
// The preceding text is what makes a cd on its own line count: the shell
// keeps one working directory from one command to the next, so "cd /go/tree"
// then "bash <<EOF" on the next line reads the body in /go/tree. Stopping at
// the invoker is the other half: "bash <<EOF && cd /go/tree" reads the body
// before the cd runs (gt-v02wh).
func heredocBodyWalkTokens(command string, span heredocSpan) []string {
	tokens := shellTokenize(stripHeredocBodies(command[:span.readerStart]))
	reader := shellTokenize(span.reader)
	return append(tokens, reader[:shellInvokerIndex(reader)]...)
}

// shellInvokerIndex returns the index of the first token of the first segment
// whose command word is a shell invoker — the segment that consumes a heredoc
// body — or len(tokens) when no segment is one. It marks where in a reader
// line the body is read, so the cds that decide the body's directory are the
// ones before it.
func shellInvokerIndex(tokens []string) int {
	start := 0
	for i := 0; i <= len(tokens); i++ {
		if i < len(tokens) && !shellCommandSeparators[tokens[i]] {
			continue
		}
		if word, _ := segmentCommandWord(tokens[start:i]); word != "" && shellInvokers[strings.ToLower(filepath.Base(word))] {
			return start
		}
		start = i + 1
	}
	return len(tokens)
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
// recurse into it the same as a bash -c/eval payload (gt-5ihs attempt 2:
// "recurse into ... command substitution").
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
	return splitSpacedCommand(command, spaceOutShellOperators(command))
}

// splitSpacedCommand runs shlex over the operator-spaced form of command.
func splitSpacedCommand(command, spaced string) []string {
	tokens, err := shlex.Split(spaced)
	if err != nil {
		return strings.Fields(command)
	}
	// shlex returning nothing for text that still has words in it means it
	// swallowed them (a comment it saw but this pass did not, gt-vonb1). Fail
	// toward checking: split naively so the matchers still see every word.
	if len(tokens) == 0 && strings.TrimSpace(strings.Trim(spaced, " ;")) != "" {
		return strings.Fields(spaced)
	}
	return tokens
}

// spaceOutShellOperators pads the command-chaining operators ;, &&, |, ||,
// and a background & with spaces wherever they appear outside quotes, so
// shlex splits them into their own tokens even when glued directly to an
// adjacent word with no whitespace ("rm -rf /;echo done" has no space around
// ';'). Without this, shlex — a generic word-splitter with no notion of shell
// control operators — folds the operator into whichever word touches it
// ("done;rm" as one token), hiding "rm" from every exact-token matcher
// (gt-mkrj). A doubled "&&" or "||" is emitted as a single spaced-out token
// rather than two adjacent single-character ones, so a matcher keyed on the
// whole operator (e.g. matchesPRWorkflowCommand's shellCommandSeparators)
// sees it as one token instead of two "&" or "|" tokens that never equal
// "&&"/"||" (gt-pjeh). An '&' touching a '>' is a redirection (2>&1, &>file)
// rather than a background operator, so it is left glued to the redirect;
// padding it would both split one redirection into three tokens and, now that
// '&' separates segments, cut a command's own arguments off from it
// (gt-wwwht). Characters inside single/double quotes, or escaped with a
// backslash outside quotes, are left untouched so quoted content (a sed
// script containing '|', a jq filter) stays exactly as opaque as it was
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
	// prevEscaped records whether the rune just consumed was backslash-escaped
	// (gt-w6tug). An escaped ' ' or ';' is literal text inside the current
	// word, so a '#' right after one is mid-word and starts no comment;
	// startsShellWord cannot see that from the previous rune alone.
	prevEscaped := false
	// prevIdx is the index of the rune the shell last saw before runes[i]:
	// runes[i-1] normally, but a backslash-newline is DELETED by the shell, so
	// after one the rune that follows is really preceded by whatever came
	// before the backslash and prevIdx is left pointing there rather than at
	// the deleted newline (gt-aa1ji). It is -1 when nothing has been seen yet.
	// startsShellWord judges a word-leading '#' against runes[prevIdx] for the
	// same reason it takes prevEscaped: the literal rune before the '#' is not
	// always the one the shell saw.
	prevIdx := -1
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
			// Both runes are deleted, so the rune after them is preceded by
			// whatever came before the backslash: leave prevIdx alone rather
			// than advancing it past the deleted continuation (gt-aa1ji).
			// Chained continuations carry the same rune forward.
			i++
			continue
		}
		if escaped {
			emit(r)
			escaped = false
			prevEscaped = true
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
		case r == '#' && sc.quote == 0 && startsShellWord(runes, prevIdx, prevEscaped):
			// An unquoted word-leading '#' is a comment that runs to the end
			// of its LINE (gt-vonb1). Drop it here, leaving the newline for
			// the separator rule below: left in, shlex would read the same
			// '#' as a comment to the end of the WHOLE string — the newline
			// already rewritten to " ; " — and swallow every later line.
			for i+1 < len(runes) && runes[i+1] != '\n' {
				i++
			}
		case r == '\n':
			b.WriteString(" ; ")
		case r == ';', r == '&', r == '|':
			if r == '&' && ((i > 0 && runes[i-1] == '>') || (i+1 < len(runes) && runes[i+1] == '>')) {
				// A '&' touching a '>' on either side is a redirection
				// (2>&1, &>file), not a background operator. Left glued it
				// stays one token, so the '&' tokens that reach the matchers
				// are all real segment boundaries (gt-wwwht).
				emit(r)
			} else if (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r {
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
		// Reset at the end of the body, not the top: the escaped branch above
		// sets the flag and continues, so a top-of-body reset would clear it
		// before the rune it describes (gt-w6tug). prevIdx records the rune the
		// shell saw here too; the branches that 'continue' above skip it, which
		// leaves the rune before a deleted backslash-newline in place (gt-aa1ji).
		prevEscaped = false
		prevIdx = i
	}
	return b.String()
}

// startsShellWord reports whether the rune after runes[prevIdx] begins a new
// shell word: it is the first character of the command (prevIdx < 0), or
// follows whitespace or a command-chaining operator or an opening parenthesis.
// prevIdx is the index of the rune the shell last saw, which is the rune
// literally before the one under test except across a deleted backslash-newline
// — spaceOutShellOperators passes the rune before the backslash there, because
// the shell deletes both and the following rune is really preceded by it
// (gt-aa1ji). prevEscaped says the rune before was backslash-escaped, which
// puts the next rune inside that same word — an escaped ' ' or ';' is literal
// text, not a boundary — so it is never a word start (gt-w6tug). A '#' that
// merely sits inside a word (a#b, ${#x}, $#) is not a comment (gt-vonb1).
func startsShellWord(runes []rune, prevIdx int, prevEscaped bool) bool {
	if prevEscaped {
		return false
	}
	if prevIdx < 0 {
		return true
	}
	switch runes[prevIdx] {
	case ' ', '\t', '\n', ';', '&', '|', '(':
		return true
	}
	return false
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
func printDangerousBlock(w io.Writer, reason, originalCommand string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ❌ DANGEROUS COMMAND BLOCKED                                    ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(w, "║  Command: %-53s ║\n", truncateStr(originalCommand, 53))
	fmt.Fprintf(w, "║  Reason:  %-53s ║\n", truncateStr(reason, 53))
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  If this is intentional, ask the user to run it manually.        ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "")
}

// printDangerousBlockWithAlternative is printDangerousBlock plus an
// unabbreviated suggestion line printed below the fixed-width box, so the
// alternative isn't lost to truncateStr's 53-char box limit.
func printDangerousBlockWithAlternative(w io.Writer, reason, originalCommand, alternative string) {
	printDangerousBlock(w, reason, originalCommand)
	if alternative != "" {
		fmt.Fprintln(w, "  "+alternative)
		fmt.Fprintln(w, "")
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
func matchesUnboundedScan(tokens []string, sess guardSession) (string, string) {
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
		// starts from. With no path argument the walker runs over cwd and the
		// pattern keeps its old reading — nothing positional is a root there,
		// which is the case the implied root below covers.
		patternSkip, hasRoot := scanRootSlot(base, rest)
		if !hasRoot {
			// A walker that names no path of its own starts at cwd, which no
			// token in the invocation shows: grep -rn TODO in the town root
			// walks the same tree as grep -rn TODO ~/gt, and the argument
			// rules below see nothing to judge (gt-3e6wa). Resolve that
			// implied root and put it through the same rules, so what makes a
			// scan dangerous is the tree it walks, not whether the root was
			// spelled.
			if root, ok := scanWalkRoot(sess.proc, fields, i, vars); ok {
				if label, suggestion := scanRootHazard(root, root, sess); label != "" {
					return fmt.Sprintf("Unbounded scan (%s, cwd is %s)", base, label),
						scanNoPathAlternative(root, suggestion)
				}
			}
		}
		for j, arg := range rest {
			resolved := resolveShellVar(arg, vars)
			root := scanRootPath(sess.proc, resolved)
			if j == patternSkip {
				root = scanPatternPath(sess.proc, resolved)
			}
			label, suggestion := scanRootHazard(resolved, root, sess)
			if label != "" {
				return fmt.Sprintf("Unbounded scan (%s rooted at %s)", base, label), "Alternative: " + suggestion
			}
		}
	}
	return "", ""
}

// scanRootHazard names the rule a resolved scan root trips and the suggestion
// printed under the block, or ("", "") when the root is bounded. token is the
// root as the caller spelled it — or the working directory, when the root was
// implied — which the filesystem and home labels print; the town label names
// the shape of the tree instead (townScanHazard).
func scanRootHazard(token, root string, sess guardSession) (label, suggestion string) {
	// A spelling broad enough to walk the whole filesystem. Judged on the
	// spelling because that is what the denylist is keyed on (~, $HOME), and
	// because a spelling like /opt names the root whatever is under it.
	if isUnboundedScanRoot(token) {
		return token, filesystemScanSuggestion
	}
	// The expanded home directory names the same root as the spellings the
	// denylist carries: an agent that writes /Users/me spells the home
	// directory in full and lands here.
	if isHomeDirScanRoot(sess.proc, root) {
		return "the home directory " + token, homeScanSuggestion
	}
	// The same walkers rooted at the town tree: the town root, a rig root, a
	// rig's worktree directory, or a .repo.git (gt-6e2l).
	if hazard := townScanHazard(root, sess.townRoot); hazard != "" {
		return hazard, townScanSuggestion
	}
	return "", ""
}

// The allow-path suggestion printed under each block: the same shape for all
// three rules (narrow the root until it names something bounded), specialised
// to what the agent should reach for instead.
const (
	filesystemScanSuggestion = "brew --prefix, pkg-config, 'go env GOROOT'/'go env GOMODCACHE', " +
		"or a search rooted inside the repo/rig instead of the whole filesystem."
	homeScanSuggestion = "search inside the repo/rig you are working in; the home " +
		"directory holds every checkout plus Documents/Desktop/Music and walking it " +
		"pegs the host and trips macOS privacy prompts."
	townScanSuggestion = "scan one repo or worktree instead of the town tree — " +
		"cd into the rig or worktree you need (~/gt/<rig>/polecats/<name>/<repo>) and grep there, " +
		"or name the specific subdirectory. The town root, rig roots, rig worktree dirs, and .repo.git are off limits."
)

// scanNoPathAlternative renders the alternative under a block whose root was
// the walker's working directory. The reason names only the tree the walk
// hits, so the text has to say where the walk started: the command spells no
// path at all, and without that the agent has nothing to correct.
func scanNoPathAlternative(cwd, suggestion string) string {
	return fmt.Sprintf("Alternative: %s (This command named no path, so the walk started at its working directory %s.)",
		suggestion, cwd)
}

// scanRootSlot locates the walk root among a scan invocation's arguments: the
// index of the argument the tool reads as its search pattern, and whether the
// invocation names a path for the walk to start from. pattern is -1 whenever
// no argument is exempt from being read as a path — for the tools that take no
// pattern at all (find, bfs, du, ls), and for a pattern tool whose invocation
// names no path, where the walker runs over cwd instead (gt-3e6wa).
//
// A tool with a pattern grammar (grep, rg, ag, fd) answers through it: the
// positional arguments after the pattern are its paths. find and bfs name
// theirs before the expression, which begins at the first option. du and ls
// take any non-flag argument as a file operand, so an option's separate value
// (du -d 1) reads as one and spares the implied-root check — a miss on a
// shape that carries no operand of its own (ls -R, du -sh), never a block.
func scanRootSlot(base string, args []string) (pattern int, hasRoot bool) {
	if gram, isPatternTool := scanGrammars[base]; isPatternTool {
		pattern, hasRoot = scanPatternSlot(gram, args)
		if !hasRoot {
			return -1, false
		}
		return pattern, true
	}
	if base == "find" || base == "bfs" {
		return -1, findNamesPath(args)
	}
	return -1, hasFileOperand(args)
}

// findNamesPath reports whether a find/bfs invocation names a path to walk at
// all: the words between the command's own options and its expression, which
// starts at the first dash-led argument or at the "(", "!" or "," that can
// begin it. So `find -name x` names no path and walks cwd, while
// `find /var/log -name x` names one.
func findNamesPath(args []string) bool {
	i := 0
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P") {
		i++
	}
	return i < len(args) && !isFlagToken(args[i]) && args[i] != "(" && args[i] != "!" && args[i] != ","
}

// hasFileOperand reports whether args carry a file operand: a non-flag
// argument, in the tools whose every operand names a file or directory to
// walk (du, ls).
func hasFileOperand(args []string) bool {
	for _, arg := range args {
		if !isFlagToken(arg) {
			return true
		}
	}
	return false
}

// dirChangeCommands names the shell builtins the walk follows — the ones that
// leave the shell in a different directory. cd and pushd name that directory as
// an operand; popd takes it from the directory stack, which the walk does not
// track, so a popd loses the directory rather than naming one (gt-n7ksl).
var dirChangeCommands = map[string]bool{"cd": true, "pushd": true, "popd": true}

// scanWalkRoot resolves the directory a scan invocation walks when it names
// no path of its own: the guard's working directory — the session's cwd —
// moved by any cd, pushd or popd the invocation runs before the scan (gt-3e6wa,
// gt-n7ksl).
//
// tokens and scanIdx locate the scan in the invocation, so the directory
// changes read here are the ones in earlier segments of the same shell line:
// `cd /tmp && grep -rn TODO` walks /tmp, and reading the session's cwd instead
// would block a bounded scan run from a rig root.
//
// ok is false when the walk root cannot be known — a change this process
// cannot resolve, a cd whose "||" branch the line does not show, or an
// unreadable working directory. An unknown root is not a hazard, so the caller
// blocks nothing on it: the walk's two unnamed outcomes read the same here, and
// the distinction cdWalkRoot draws between them is for the guards that refuse
// on one (gt-ofj05).
func scanWalkRoot(proc guardProcess, tokens []string, scanIdx int, vars map[string]string) (string, bool) {
	root, err := proc.getwd()
	if err != nil {
		return "", false
	}
	dir, status := cdWalkRoot(proc, tokens, scanIdx, root, vars)
	return dir, status == cdWalkPlaced
}

// cdWalkStatus says what the walk can name about the directory the shell is in
// at the end of the cds it was given. The two outcomes without a directory are
// separate because the callers do different things with them: a cd the guard
// cannot place leaves the segment's targets unjudged, while the walk's other
// give-up leaves the base standing (gt-0lzdi, gt-ofj05).
type cdWalkStatus int

const (
	// cdWalkPlaced: dir is the directory the shell is in.
	cdWalkPlaced cdWalkStatus = iota
	// cdWalkUnplaced: the line carries a cd this guard cannot place — a target
	// it cannot expand, `cd -`, more than one operand. The shell may have
	// changed into a directory the line never names.
	cdWalkUnplaced
	// cdWalkGivenUp: the walk has no directory to name, but no cd on the line
	// is one the guard failed to place — a cd the shell refuses at the dead end
	// of an "&&" list whose remainder the walk cannot name from, or a "||"
	// chain whose left cd resolved and so leaves the branch it took unshown.
	cdWalkGivenUp
)

// cdWalkOutcome names the walk's unnamed outcome for a change it cannot follow
// to a directory, in the terms cdWalkRoot reports (cdWalkStatus). A change this
// guard cannot place is an unplaced line — the shell may have changed into a
// directory the line never names — and one the shell refuses at a dead end, or
// leaves open because it hangs off a "||" the walk cannot follow, is a
// give-up.
func cdWalkOutcome(status cdStatus) cdWalkStatus {
	if status == cdUnknown {
		return cdWalkUnplaced
	}
	return cdWalkGivenUp
}

// cdWalkRoot applies to base the directory changes of tokens that precede end
// and returns the directory the shell is in at end. It is the one tracker the
// guards share: scanWalkRoot is this walk with the base taken from the process,
// segmentWalkRoot is it applied to one segment of a line, and the shell-fed
// heredoc recursion judges a body in the directory the shell had reached by the
// time it read the body (gt-1cvqj, gt-v02wh). Every caller therefore agrees on
// whether a given change carries forward, and the status says which directory
// the walk has to name, if any (cdWalkStatus).
//
// A change this process cannot resolve loses the directory from that point on
// rather than being guessed at (gt-n7ksl): the shell may or may not have moved,
// so what the commands after it run in is not something the walk can name. A cd
// the shell refuses loses nothing: the shell stays where it was, so the walk
// keeps the directory it already has and a relative target after the refusal
// resolves against it (gt-34vra). A change carried by ";" or by nothing at all
// is where that keeps going — the commands after a ";" certainly run, so a
// later absolute target needs no base and names the directory again.
//
// An "&&" list stops at a change that does not resolve (gt-n7ksl): the list is
// at its dead end, and no cd inside it after that point runs — `cd /nonexistent
// && cd /tmp` never reaches /tmp. The list itself does end, at the ";" that
// follows, and the shell reads on from there — `cd /nonexistent && true ; cd
// /tmp` is in /tmp once the ";" is read — so the walk skips the list's
// remainder and reads the changes from that ";" on (gt-34vra). A "||" or a
// background "&" in that remainder is not a point it reads from: the "||"
// turns on whether the list left of it failed and the "&" runs what follows in
// this shell, so the walk reports the line's outcome rather than name a
// directory out of a stretch of the line it has not followed.
//
// The status is the outcome the walk reached: cdWalkPlaced with the directory
// the shell is in, or the unnamed outcome of the change it could not resolve
// (cdWalkOutcome). A later change that does place the directory clears it, the
// commands after that one running in a tree the line names.
func cdWalkRoot(proc guardProcess, tokens []string, end int, base string, vars map[string]string) (string, cdWalkStatus) {
	root := base
	lost := cdWalkPlaced
	for i := 0; i < end; i++ {
		if !dirChangeCommands[tokens[i]] || !shellCommandStart(tokens, i) {
			continue
		}
		args := commandArgs(tokens, i)
		sep := i + 1 + len(args)
		// "cd A || cd B ..." runs cd B only when cd A fails, and the walk
		// reads changes out of branches it cannot follow. So a cd A that
		// resolves drops the directory rather than name cd B's, which is the
		// shell's only when the "||" was taken: `cd <go tree> || cd <safe> &&
		// make test` runs in the go tree, `cd <safe> || cd <rig root> && grep
		// -rn TODO` in <safe>. A cd A the shell refuses did fail, so the "||"
		// is taken and the directory the walk already had stands (gt-0lzdi).
		if sep < end && tokens[sep] == "||" {
			if _, status := dirChangeTarget(proc, tokens[i], args, root, vars); status != cdRefused {
				return "", cdWalkOutcome(status)
			}
			continue
		}
		// Only "&&" and ";" carry the change to the command that follows: a
		// change in a pipeline or a background job runs in a subshell of its
		// own, so the walk keeps the working directory the shell already had.
		// Reading the separator before the target drops an unresolvable change
		// with them: it never reaches this shell, so it cannot make the
		// directory unknown either (gt-n7ksl).
		//
		// A brace group is no such subshell: it runs in this shell, and the ";"
		// that must end its last command is the separator this reads, so
		// `cd A ; { cd B ; } ; make test` runs make in B (gt-ajyw8).
		if sep < end && tokens[sep] != "&&" && tokens[sep] != ";" {
			continue
		}
		target, status := dirChangeTarget(proc, tokens[i], args, root, vars)
		if status == cdResolved {
			root = target
			lost = cdWalkPlaced
			continue
		}
		// A cd the shell refuses is one the walk can still name: the shell
		// stays where it was, so the directory the walk already has stands,
		// and a later relative target resolves against it rather than against
		// nothing (gt-34vra). A change the walk cannot place may have moved
		// the shell into a directory the line never names, so that one drops
		// the directory rather than be guessed at — but the walk keeps going,
		// because a later absolute target needs no base and names the
		// directory again (gt-n7ksl). Where it does not, this change's outcome
		// is the line's; a change the guard cannot place is the stronger of
		// the two, and stays the line's whatever a later refusal adds.
		if status != cdRefused {
			root = ""
			if outcome := cdWalkOutcome(status); lost == cdWalkPlaced || outcome == cdWalkUnplaced {
				lost = outcome
			}
		}
		// An "&&" list ends at that change, so no cd after it inside the list
		// runs — `cd /nonexistent && cd /tmp` never reaches /tmp, and naming it
		// would judge the line in a tree the shell stays out of. The list does
		// end, though, at the ";" that follows, and the shell reads on from
		// there (gt-34vra), so the walk skips the list's remainder and reads
		// from that ";" — a cd inside the remainder is not one the shell
		// reaches, and a "||" or a background "&" in it is a point the walk
		// cannot name a directory from (cdWalkListEnd).
		if sep < end && tokens[sep] == "&&" {
			resume := cdWalkListEnd(tokens, sep+1, end)
			if resume < 0 {
				return "", cdWalkOutcome(status)
			}
			i = resume
		}
	}
	return root, lost
}

// cdWalkListEnd returns the index of the ";" that ends the "&&" list a cd
// dead-ended at — the point the shell reads on from, and so the first token
// whose changes the walk may name a directory for (gt-34vra). It returns -1
// when the list has no end in tokens[start:end), and when a "||" or a
// background "&" comes first: the "||" turns on whether the list left of it
// failed, and the "&" runs what follows in this shell, so neither leaves the
// directory the walk is carrying as the one the shell has there.
func cdWalkListEnd(tokens []string, start, end int) int {
	for i := start; i < end; i++ {
		switch tokens[i] {
		case ";":
			return i
		case "||", "&":
			return -1
		}
	}
	return -1
}

// segmentWalkRoot resolves the directory a guarded shell segment runs in: base
// — the invocation's directory, or the directory a shell-fed heredoc's reader
// line left the shell in — moved by the cd, pushd and popd segments earlier on
// the same shell line, by cdWalkRoot's rules (gt-5mc21, gt-n7ksl). tokens[start]
// is the segment's first token, so the walk reads the earlier segments' changes
// and only those a "&&" or ";" carries — a change in a pipeline or a background
// job, or one whose directory a "||" chain leaves open, is one the segment does
// not inherit.
//
// The directory comes back as the tree it really names, symlinks resolved:
// these rules ask which tree a command runs in, and a path reached through a
// link carries the link's lexical parents, not the target's, so a link to a
// package inside a Go tree would read as a non-Go directory (gt-ofj05).
//
// ok is false only for a cd this guard cannot place (cdWalkUnplaced), and the
// callers refuse then, the reading an unplaceable make -C already gets: the
// segment's directory decides what its targets name, so an unplaced one leaves
// the question open rather than answered by the hook's cwd (gt-ofj05). The
// walk's other unnamed outcome — a "||" whose branch is unshown, a cd the shell
// refuses — keeps the base (gt-0lzdi). A line that never changes directory
// resolves to base with ok true.
func segmentWalkRoot(proc guardProcess, tokens []string, start int, base string, vars map[string]string) (string, bool) {
	dir, status := cdWalkRoot(proc, tokens, start, base, vars)
	switch status {
	case cdWalkUnplaced:
		return "", false
	case cdWalkGivenUp:
		dir = base
	}
	resolved, ok := resolveDirSymlinks(dir)
	if !ok {
		return "", false
	}
	return resolved, true
}

// shellCommandStart reports whether the token at i begins a shell command
// rather than continuing an argument list: the line's first word, the word
// after a separator, or the word after the "(" or "{" that opens a group.
// The two groups differ in what the walk does with a cd inside them — a
// subshell's change is not the shell's, a brace group's is — but both hold a
// command list, so the word after either opens one (gt-ajyw8).
func shellCommandStart(tokens []string, i int) bool {
	if i == 0 {
		return true
	}
	return shellCommandSeparators[tokens[i-1]] || tokens[i-1] == "(" || tokens[i-1] == "{"
}

// cdStatus says what the walk can tell about the directory change a cd or
// pushd segment makes. The zero value is the reading a caller must fall back on
// when it can tell nothing, so a new spelling added to dirChangeTarget is
// unknown until it is deliberately classified.
type cdStatus int

const (
	// cdUnknown: the guard cannot tell whether the change succeeds — `cd -`,
	// a directory-stack entry, an argument it cannot expand, or more than one
	// operand. Never resolved by assumption.
	cdUnknown cdStatus = iota
	// cdRefused: the shell refuses the change — the target is not an existing
	// directory — so the directory the shell was in stands, and a "||" left of
	// it is taken.
	cdRefused
	// cdResolved: the target is an existing directory, so the change succeeds.
	cdResolved
)

// dirChangeTarget resolves the directory a cd or pushd segment leaves the shell
// in, relative to base — the directory the shell was in when it ran — for the
// walk root cdWalkRoot tracks. A bare `cd` goes to the home directory, and a
// pushd -n leaves the shell where it is. The status separates a change the
// shell refuses from one whose result this guard cannot know, which the "||"
// rule has to tell apart (gt-0lzdi; see the cdStatus constants).
//
// base is "" when the walk has already lost the directory, which is also what
// an unreadable working directory gives, and then only an absolute target
// resolves: a relative one has no base to be resolved against, so resolving it
// against this process's own directory would judge a tree the command never
// named (gt-1cvqj) — and guessing one is what this walk exists not to do
// (gt-n7ksl).
//
// The status is cdUnknown for every spelling whose result this guard does not
// name: a popd or a bare pushd, whose target is a directory-stack entry; `cd
// -`'s previous directory; an argument this process cannot expand; or more than
// one operand, which the walk leaves unread rather than read as the refusal a
// "||" turns on. A target that is not an existing directory is cdRefused — the
// change fails, and a "||" left of it is taken.
func dirChangeTarget(proc guardProcess, name string, args []string, base string, vars map[string]string) (string, cdStatus) {
	// A popd changes to a directory-stack entry, and the walk tracks no stack:
	// which directory that is cannot be named, whatever operands the popd
	// carries.
	if name == "popd" {
		return "", cdUnknown
	}
	// pushd -n pushes onto the stack without changing the directory, so the
	// shell stays in the one the walk already knows — including when that one
	// is itself unknown.
	if name == "pushd" && hasExactArg(args, "-n") {
		if base == "" {
			return "", cdUnknown
		}
		return base, cdResolved
	}
	var operands []string
	for _, arg := range args {
		switch arg {
		case "-L", "-P", "--":
			continue
		}
		if isFlagToken(arg) {
			return "", cdUnknown
		}
		operands = append(operands, arg)
	}
	if len(operands) > 1 {
		return "", cdUnknown
	}
	if len(operands) == 0 {
		// A bare cd is the home directory; a bare pushd swaps with the
		// directory stack, which this walk does not track.
		if name != "cd" {
			return "", cdUnknown
		}
		home, err := proc.homeDir()
		if err != nil || home == "" {
			return "", cdUnknown
		}
		return home, cdResolved
	}
	path, ok := expandHomePath(proc, resolveShellVar(operands[0], vars))
	if !ok || path == "" || strings.Contains(path, "$") {
		return "", cdUnknown
	}
	if !filepath.IsAbs(path) {
		if base == "" {
			return "", cdUnknown
		}
		path = filepath.Join(base, path)
	}
	path = filepath.Clean(path)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return "", cdRefused
	}
	return path, cdResolved
}

// resolveDirSymlinks returns path with every symlink resolved. ok is false
// when a path that exists cannot be resolved — a link loop, an unreadable
// component — which is a directory the guard cannot place. A path that does
// not exist is returned unchanged with ok true: there is nothing to resolve,
// and its callers judge a missing directory by the tree that would enclose it
// (goModuleRoot, gt-dieu9).
//
// A relative path is returned unchanged as well: EvalSymlinks would answer
// with a relative path resolved against this process's own directory, which is
// not the one the caller meant — an empty one, from a session whose working
// directory could not be read, would come back as "." and name this process's
// tree.
func resolveDirSymlinks(path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return path, true
	}
	resolved, err := filepath.EvalSymlinks(path)
	switch {
	case err == nil:
		return resolved, true
	case errors.Is(err, fs.ErrNotExist):
		return path, true
	default:
		return path, false
	}
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

// matchesDangerousRmRf blocks "rm -rf /" targeting the root filesystem.
// Only blocks when the target is literally "/" or "/*". Normal cleanup
// commands like "rm -rf ./build/" are allowed. tokens must be lowercased,
// shell-aware tokens (see shellTokenize).
func matchesDangerousRmRf(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if filepath.Base(tok) != "rm" || !inCommandPosition(segment, i) {
				continue
			}
			// Flags are read across the whole invocation: -r -f, -fr,
			// --recursive --force and --no-preserve-root all spell the same
			// thing (gt-pb77k).
			recursive, force := false, false
			for _, f := range segment[i+1:] {
				switch {
				case f == "--recursive":
					recursive = true
				case f == "--force" || f == "--no-preserve-root":
					force = true
				case strings.HasPrefix(f, "--"):
				case len(f) > 1 && f[0] == '-':
					recursive = recursive || strings.ContainsAny(f[1:], "rR")
					force = force || strings.Contains(f[1:], "f")
				}
			}
			if !recursive || !force {
				continue
			}
			for _, f := range segment[i+1:] {
				if f == "/" || f == "/*" || f == "//" || f == "/." {
					return "filesystem destruction (rm -rf /)"
				}
			}
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

// packageManagerPatterns lists system package manager install commands. Each
// entry's tokens are [command word, install subcommand]: the command word is
// the package manager and "install" is an argument of that invocation (see
// matchesPackageInstall).
var packageManagerPatterns = []struct {
	tokens []string
	reason string
}{
	{[]string{"apt", "install"}, "System package install (apt) — use workspace tools instead"},
	{[]string{"apt-get", "install"}, "System package install (apt-get) — use workspace tools instead"},
	{[]string{"dnf", "install"}, "System package install (dnf) — use workspace tools instead"},
	{[]string{"yum", "install"}, "System package install (yum) — use workspace tools instead"},
	// pacman is deliberately absent here — see matchesPacmanInstall. Its
	// install is a -S flag, not an "install" word, and its -S (sync/install)
	// vs -s (search modifier, e.g. -Ss/-Qs) flags differ only by case, which
	// this table's lowercased match can't tell apart (finding 7,
	// gt-wisp-db27).
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

// matchesPackageInstall blocks system package manager install commands and,
// more narrowly, "pip install --system" and global npm installs. Each shell
// segment is read as one invocation (gt-24lz6): the segment's command word —
// basename, skipping env assignments and launchers (segmentCommandWord) — is
// the package manager, and "install" is one of that invocation's arguments,
// in any position so an option before the subcommand ("apt-get -y install
// curl") is still blocked (gt-qis3f). tokens must be lowercased, shell-aware
// tokens (see shellTokenize).
func matchesPackageInstall(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		word, args := segmentCommandWord(segment)
		if word == "" {
			continue
		}
		base := filepath.Base(word)

		// Simple package-manager pairs (apt install, dnf install, etc.): the
		// command word is the manager and "install" is an argument.
		for _, p := range packageManagerPatterns {
			if base == p.tokens[0] && hasExactArg(args, p.tokens[1]) {
				return p.reason
			}
		}

		// pip install --system (but not regular pip install into a venv)
		if (base == "pip" || base == "pip3") && hasExactArg(args, "install") && hasExactArg(args, "--system") {
			return "System-level pip install — use a virtualenv or workspace tools instead"
		}

		// npm install -g / npm install --global
		if base == "npm" && hasExactArg(args, "install") && (hasExactArg(args, "-g") || hasExactArg(args, "--global")) {
			return "Global npm install — use workspace tools instead"
		}
	}

	return ""
}

// matchesDangerousGitPush blocks a forced `git push`: --force, -f (alone or
// in a short-flag cluster such as -fu), or a "+" refspec, any of which rewrites
// remote history. Safe variants (--force-with-lease, --force-if-includes) pass.
//
// Structured like matchesGitClean: git must be running in command position
// (a path such as /usr/bin/git counts), push must be its subcommand after any
// global options (-C dir, -c k=v), and the flag must be among push's own
// arguments in the same shell segment, so a "-f" belonging to a later command
// is not misread as a force push (gt-kocid). tokens must be lowercased,
// shell-aware tokens (see shellTokenize).
func matchesDangerousGitPush(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if filepath.Base(tok) != "git" || !inCommandPosition(segment, i) {
				continue
			}
			rest := segment[i+1:]
			sub := gitSubcommandIndex(rest)
			if sub < 0 || rest[sub] != "push" {
				continue
			}
			for _, f := range rest[sub+1:] {
				if isForcePushArg(f) {
					return "Force push rewrites remote history and can destroy others' work"
				}
			}
		}
	}
	return ""
}

// isForcePushArg reports whether one argument of `git push` forces the push.
func isForcePushArg(f string) bool {
	switch {
	case f == "--force":
		return true
	case strings.HasPrefix(f, "--"):
		return false // --force-with-lease, --force-if-includes and every other long option
	case len(f) > 1 && f[0] == '+':
		return true // a "+" refspec forces that ref
	case len(f) > 1 && f[0] == '-':
		return strings.Contains(f[1:], "f") // -f, or a cluster such as -fu
	}
	return false
}

// polecatMainPushReason and polecatMainPushAlternative are the block banner
// and its allow-path suggestion for a polecat pushing the default branch.
//
// The alternative names the two flows that legitimately push a default branch
// but get no env signal here (gt-deff): a release (beads-release /
// gastown-release) and a plugin script's own push when an agent runs its
// instructions by hand. Neither gets a signal because a signal an agent sets
// for itself is a user override, not a gate - only the Refinery's merge and
// `gt done`'s own direct merge, which gt itself sets, are gates. So the
// allow path for both is a crew or refinery session.
const (
	polecatMainPushReason      = "Polecats never push to main/master (use gt done)"
	polecatMainPushAlternative = "Alternative: `gt done` pushes your polecat/<name>/<bead> branch and the Refinery " +
		"merges it to the default branch after verification — a direct push to main skips " +
		"the MR, the Refinery gate run, and the om review (gt-ibt8). A release, or a plugin " +
		"script's own push, runs from a crew/refinery session instead (gt-deff)."
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
func inPolecatSession(proc guardProcess) bool {
	if role := strings.TrimSpace(proc.getenv("GT_ROLE")); role != "" {
		return isPolecatRole(role)
	}
	return proc.getenv("GT_POLECAT_PATH") != ""
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
	// One invocation at a time: a ref or flag that belongs to a later command
	// in the chain is not this push's argument (gt-yapnr).
	for _, args := range gitSubcommandArgs(tokens, "push") {
		for _, f := range args {
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
	for _, args := range gitSubcommandArgs(tokens, "reset") {
		for _, f := range args {
			// A bare "--" ends the revisions: everything after it is a pathspec,
			// so "git reset -- origin/main" is unstaging a path that happens to be
			// spelled like a ref, not resetting onto one.
			if f == "--" {
				break
			}
			for _, prefix := range gitResetRemotePrefixes {
				if strings.HasPrefix(f, prefix) {
					return "Reset onto a remote-tracking ref drops merged work",
						"Alternative: `git rebase " + f + "` — rebase your CHANGES onto the remote ref; never reset your tree onto it."
				}
			}
		}
	}
	return "", ""
}

// hasForceFlag reports whether args carry git's force flag in either
// spelling: the long form "--force", or a short option with 'f' bundled in
// ("-f", "-fd", "-fdx"). The short match is case-sensitive through
// hasShortFlagLetter (-F is not -f); the long form is an exact token
// comparison, so an unrelated long flag that merely starts with the same
// letters ("--force-with-lease") is not read as one.
func hasForceFlag(args []string) bool {
	for _, a := range args {
		if a == "--force" {
			return true
		}
	}
	return hasShortFlagLetter(args, 'f')
}

const gitCleanReason = "git clean -f/--force deletes untracked files irreversibly"

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
// is matched case-sensitively, and in both its spellings — -f/-fd/-fdx or
// --force (hasForceFlag) — so an unrelated "-x" is not read as one and a
// long flag of another subcommand is not read as the long form.
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
			if hasForceFlag(rest[sub+1:]) {
				return gitCleanReason
			}
		}
	}
	return ""
}

const gitResetHardReason = "Hard reset discards all uncommitted changes irreversibly"

// matchesGitResetHard blocks a `git reset` invocation carrying --hard.
//
// Structured like matchesGitClean, and for the same reason (gt-24lz6): the
// words are one invocation — "git" running, "reset" as its subcommand,
// "--hard" among that invocation's own arguments — not three fragments free
// to land anywhere in the token list. Containment matched them wherever they
// appeared, so a compound line that happened to spell "git", "reset" and
// "--hard" in three different segments was rejected as a hard reset.
//
// matchesDangerousGitReset runs first and reports the more specific
// reset-onto-a-remote-ref reason, so "git reset --hard origin/main" still
// names the remote-tracking hazard rather than this one.
//
// tokens must be original-case, shell-aware tokens (see shellTokenize): the
// command and subcommand are compared case-insensitively and the flag
// case-sensitively, as in matchesGitClean.
func matchesGitResetHard(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if !strings.EqualFold(filepath.Base(tok), "git") || !inCommandPosition(segment, i) {
				continue
			}
			rest := segment[i+1:]
			sub := gitSubcommandIndex(rest)
			if sub < 0 || !strings.EqualFold(rest[sub], "reset") {
				continue
			}
			for _, arg := range rest[sub+1:] {
				if arg == "--hard" {
					return gitResetHardReason
				}
			}
		}
	}
	return ""
}

// ddlDestructionPatterns are the destructive SQL verb/object pairs the guard
// blocks, with the reason to report for each. Matching is case-insensitive —
// SQL is — so "DROP TABLE users" is the same statement as "drop table users".
var ddlDestructionPatterns = []struct {
	verb   string
	object string
	reason string
}{
	{"drop", "table", "database table destruction"},
	{"drop", "database", "database destruction"},
	{"truncate", "table", "database table truncation"},
}

// matchesDDLDestruction blocks a destructive DDL statement: "drop table",
// "drop database" or "truncate table".
//
// The verb and its object must be adjacent within one shell segment, with the
// verb in command position — the same one-invocation reading as
// matchesGitClean (gt-24lz6). Containment asked only that the two words appear
// somewhere in the token list, so a compound line with "drop" in one segment
// and an unrelated "table" in another was rejected as SQL destruction.
//
// inCommandPosition keeps a text-only mention out: `echo drop table users`
// prints words, it drops nothing. A quoted SQL payload stays opaque for the
// same reason (TestQuotedSQLStaysOpaque), while the fail-closed reading still
// blocks an object reached through a non-text command
// (`mysql -e drop table users`).
//
// tokens must be original-case, shell-aware tokens (see shellTokenize).
func matchesDDLDestruction(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if i+1 >= len(segment) || !inCommandPosition(segment, i) {
				continue
			}
			for _, p := range ddlDestructionPatterns {
				if strings.EqualFold(tok, p.verb) && strings.EqualFold(segment[i+1], p.object) {
					return p.reason
				}
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
	"if you believe the cache is corrupt, escalate with the evidence instead of clearing it."

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

// polecatFullSuiteUncachedReason is the block text for an uncached
// whole-module 'go test' from a polecat seat (gt-v4r0x).
const polecatFullSuiteUncachedReason = "'go test -count=1' over the whole module (./...) recompiles every package from scratch and discards each result, saturating the Go build cache and this shared host"

const polecatFullSuiteUncachedAlternative = "Alternative: run 'make presubmit' — it lints, builds, and tests only the packages your branch changed against origin/main " +
	"(plus the tree-wide guard tests); the rig's Forgejo CI gate runs the full gate on the candidate branch. A cached 'go test ./...' and scoped uncached runs stay allowed."

// matchesPolecatFullSuiteUncached blocks an uncached whole-module 'go test'
// from a polecat session: a 'go test' with '-count=1' set and at least one
// package argument naming the whole-repo wildcard (./..., ...; see
// normalizeGoPackageArg). Such a run — with or without -json, in any flag
// order — recompiles and relinks every package and throws the results away,
// so a seat that runs it competes with the very gate it is waiting on. Two
// runs plus a landing gate on 10-03 drove load to 45-55 and stretched the
// gate from ~30s to 5m02s (gt-v4r0x). 'make presubmit' already tests only
// the changed packages, so a seat has no need for the full-tree form.
//
// Only the full-tree form is blocked. A plain cached 'go test ./...' (no
// -count=1) reuses the shared cache and stays allowed; scoped uncached runs
// (go test -count=1 ./internal/cmd/...) and -run-filtered runs stay allowed;
// crew, refinery and operator sessions are untouched
// (polecatSession is supplied by the caller from inPolecatSession, exactly
// as matchesPolecatMainPush does, so the matcher stays pure and
// table-testable).
//
// tokens must be lowercased, shell-aware tokens (see shellTokenize). Flag
// scanning stops at a shell separator or '--', so a later unrelated command
// on the same line cannot contribute a -count=1 that belongs to itself
// (mirroring matchesGoCleanSharedCache's separator reset).
func matchesPolecatFullSuiteUncached(tokens []string, polecatSession bool) (reason, alternative string) {
	if !polecatSession {
		return "", ""
	}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == "go" && tokens[i+1] == "test" {
			if fullSuiteUncachedGoTest(tokens[i+2:]) {
				return polecatFullSuiteUncachedReason, polecatFullSuiteUncachedAlternative
			}
		}
	}
	return "", ""
}

// fullSuiteUncachedGoTest reports whether the tokens following a 'go test'
// invocation (rest) form an uncached whole-module run: -count=1 is set (as
// '-count=1' or '-count 1') and some package argument normalizes to the
// whole-repo wildcard. Flag value tokens are skipped via goTestValueFlags so
// a '-run ./...'-style value is never mistaken for a package argument, and
// scanning stops at '--' (test-binary args) or a shell separator.
func fullSuiteUncachedGoTest(rest []string) bool {
	count1 := false
	wholeRepo := false
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if shellCommandSeparators[tok] || tok == "--" {
			break
		}
		if strings.HasPrefix(tok, "-") {
			flag, value, hasValue := strings.Cut(tok, "=")
			if flag == "-count" {
				if hasValue {
					count1 = value == "1"
				} else if i+1 < len(rest) && rest[i+1] == "1" {
					count1 = true
				}
			}
			if goTestValueFlags[flag] && !hasValue && i+1 < len(rest) {
				i++
			}
			continue
		}
		if isWholeRepoPackageArg(normalizeGoPackageArg(tok)) {
			wholeRepo = true
		}
	}
	return count1 && wholeRepo
}

// idleGateLoad1Threshold is the 1-minute load average above which a
// full-suite start is HELD (exit 2, retryable) rather than permitted. This
// is the standing operator rule ("do NOT start or retry a full-suite gate
// while 1-min loadavg > 60"); the guard used to derive its own, stricter
// threshold from load1/NumCPU, which held gates on idle, many-core hosts
// because macOS loadavg counts uninterruptible-wait processes, not just
// CPU-runnable ones (gt-e6xh).
const idleGateLoad1Threshold = 60

// idleGateAlternative is the HOLD banner's suggested next step. The first
// sentence is true of every held command and is fixed; the second names the
// command that was actually held (heldAction), because advice to run a
// different target — a fixed "use GOFLAGS=-p=8 make test" — cost a polecat
// held on 'make build' a second full-module run instead of the one it wanted
// (gt-p7g2m). GOFLAGS=-p=8 caps go's build parallelism for either target.
func idleGateAlternative(heldAction string) string {
	return "Wait 2 minutes and re-run this exact command; it will pass once load1 <= 60. " +
		fmt.Sprintf("Avoid bare '%s' at full parallelism — use GOFLAGS=-p=8 %s.", heldAction, heldAction)
}

// idleGateHeldAction reports the held invocation's own spelling ("" when the
// command is not gated). The HOLD banner names it back to the operator: a
// polecat held on 'make build' must not be told to run the test suite
// (gt-p7g2m), so the advice line is built from what was actually matched
// rather than a fixed target.
//
// It detects an unwrapped full-suite start: 'go test ./...', 'go build ./...',
// or a 'make test'/'make build' whose target is the whole-module Go action
// (makeActsOnWholeGoModule — a non-Go rig's own target costs the host a
// per-rig build, not the shared module's, so it is not gated; gt-mjfir).
// Heredoc bodies are stripped first (stripHeredocBodies)
// so prose written to a file — "cat > note.md <<'EOF'\nRun make test\nEOF" —
// is never misread as a live invocation, the same rule evaluateDangerousCommand
// applies. The remaining text is split into shell segments (on ;/&&/||/|,
// same as evaluateContainerSuiteCommand) and each is evaluated independently,
// so a "gt slot run -- make test" wrapper and an unrelated later "make test"
// on the same compound line are judged separately. Unlike
// evaluateDangerousCommand, this does NOT recurse into bash -c/eval payloads
// or command substitutions — the interim hook it replaces didn't either, and
// no incident has required it; scope stays narrow until one does.
//
// Each segment is judged in the directory its own line runs it in — the hook
// cwd moved by the cd, pushd and popd segments earlier on the same line, by the
// shared walk (segmentWalkRoot, over cdWalkRoot/dirChangeTarget: "&&" and ";"
// carry the change, a "||" whose left cd resolves and a cd the shell refuses
// both leave the directory unknown, and the hook cwd is the fallback) — and
// held when the directory is one the walk cannot place at all (gt-ofj05).
// Judging every segment against the hook cwd instead let an agent whose cwd is
// a non-Go tree run the whole module suite with 'cd <go tree> && make test' —
// the make target resolved in the non-Go tree, where it is not the module's
// (gt-me4vs), the same hole the container-suite and test-scope rules had
// (gt-5mc21).
func idleGateHeldAction(proc guardProcess, command string) string {
	tokens := shellTokenize(strings.TrimSpace(stripHeredocBodies(command)))
	vars := shellVarAssignments(tokens)
	cwd, _ := proc.getwd()
	var segment []string
	start := 0
	for i, tok := range tokens {
		if shellCommandSeparators[tok] {
			dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
			if held := idleGateHeldSegment(segment, dir, known); held != "" {
				return held
			}
			segment = nil
			start = i + 1
			continue
		}
		segment = append(segment, tok)
	}
	dir, known := segmentWalkRoot(proc, tokens, start, cwd, vars)
	return idleGateHeldSegment(segment, dir, known)
}

// isIdleGatedSuiteStartCommand reports whether command contains an unwrapped
// full-suite start, discarding the matched spelling (idleGateHeldAction).
func isIdleGatedSuiteStartCommand(proc guardProcess, command string) bool {
	return idleGateHeldAction(proc, command) != ""
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
// between shell operators), discarding the held spelling
// (idleGateHeldSegment).
func isIdleGatedSuiteStartSegment(tokens []string, cwd string, cwdKnown bool) bool {
	return idleGateHeldSegment(tokens, cwd, cwdKnown) != ""
}

// idleGateHeldSegment judges a single shell segment (tokens between shell
// operators) and returns the held invocation as it should be spelled back to
// the operator — the target of a held make ('make test'/'make build', however
// its flags were written) or the whole-repo go form ('go test ./...'/'go build
// ./...'). "" means the segment is not gated. tokens is original-case; matching
// is done on a lowercased copy so "Make Test" and "make test" are treated the
// same. cwd and cwdKnown locate a make invocation's tree, through makeTreeDir
// and makeActsOnWholeGoModule.
func idleGateHeldSegment(tokens []string, cwd string, cwdKnown bool) string {
	if len(tokens) == 0 {
		return ""
	}
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	if hasPrefix(lower, idleGateSlotRunPrefix) {
		return ""
	}
	if i := findInvocation(lower, "make", "test"); i >= 0 && makeActsOnWholeGoModule(tokens[i:], makeTreeDir(cwd, cwdKnown)) {
		return "make test"
	}
	if i := findInvocation(lower, "make", "build"); i >= 0 && makeActsOnWholeGoModule(tokens[i:], makeTreeDir(cwd, cwdKnown)) {
		return "make build"
	}
	if i := findInvocation(lower, "go", "test"); i >= 0 && wholeRepoArgFollows(tokens, i+2) {
		return "go test " + tokens[i+2]
	}
	if i := findInvocation(lower, "go", "build"); i >= 0 && wholeRepoArgFollows(tokens, i+2) {
		return "go build " + tokens[i+2]
	}
	return ""
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

// actualHostLoad1 returns the current 1-minute load average
// (guardProcess.load1 in production). It reads internal/daemon's raw
// load-average sample (one instant sysctl/proc read, no subprocess sampling
// loop) rather than shelling out to
// `top` for several seconds on every suite-start command — the same
// mechanism the deleted main_branch_test patrol's gate used
// (gt-f57o), read unnormalized so a host whose load comes from non-CPU
// (uninterruptible-wait) contention isn't misjudged as CPU-saturated
// (gt-e6xh).
func actualHostLoad1() (load1 float64, ok bool) {
	return daemon.EstimateLoad1(), true
}

// printIdleGateHold prints the HOLD banner to stderr — distinct from
// printDangerousBlock's BLOCKED banner since this command is expected to
// succeed on retry, not to be avoided entirely. heldAction is the gated
// invocation evaluateIdleGate matched, named in the banner's advice line so
// the suggestion matches the command it held (gt-p7g2m).
func printIdleGateHold(w io.Writer, load1 float64, command, heldAction string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ⏸  SUITE START HELD (host busy)                                 ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(w, "║  Command:   %-51s ║\n", truncateStr(command, 51))
	fmt.Fprintf(w, "║  Load avg:  %-51s ║\n", fmt.Sprintf("%.1f (need <= %d)", load1, idleGateLoad1Threshold))
	fmt.Fprintln(w, "║                                                                  ║")
	fmt.Fprintln(w, "║  Another suite is running on this shared host.                  ║")
	fmt.Fprintln(w, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(w, "  "+idleGateAlternative(heldAction))
	fmt.Fprintln(w, "")
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

// gitSubcommandArgs returns, for every git invocation in tokens whose
// subcommand is sub, the arguments that follow the subcommand — one slice per
// invocation, each confined to its own shell segment so a later command in the
// chain (&&, ;, |) never lends its words to an earlier one. git may be a path
// (/usr/bin/git) and may carry global options (-C dir, -c k=v) before the
// subcommand (gt-yapnr).
func gitSubcommandArgs(tokens []string, sub string) [][]string {
	var out [][]string
	for _, segment := range splitShellSegments(tokens) {
		for i, tok := range segment {
			if !strings.EqualFold(filepath.Base(tok), "git") || !inCommandPosition(segment, i) {
				continue
			}
			rest := segment[i+1:]
			if at := gitSubcommandIndex(rest); at >= 0 && strings.EqualFold(rest[at], sub) {
				out = append(out, rest[at+1:])
			}
		}
	}
	return out
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
