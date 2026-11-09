package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/hoststat"
	"github.com/steveyegge/gastown/internal/shellparse"
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
    where a dog's 'grep -R ...' of the operator's home ran unblocked and
    walked every rig and every worktree on the host. A path inside a single
    repo or worktree (e.g. ~/gt/<rig>/polecats/<name>/<repo>) is still
    allowed, as is a search pattern that shares a name with a directory
    there: 'grep -rn polecats ./docs' searches ./docs, not the polecats/
    directory at cwd (gt-yts7).
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

	if cpuIdle, _, heldAction := evaluateIdleGate(proc, command, proc.cpuIdle); heldAction != "" {
		printIdleGateHold(stderr, cpuIdle, command, heldAction)
		return NewSilentExit(2)
	}

	return nil
}

// evaluateIdleGate reports whether command is a full-suite start
// (idleGateHeldAction) that the host is too busy to take: a matched command
// comes back as heldAction, its own spelling ("make test", "go build ./..."),
// so the HOLD banner names what it held instead of guessing. cpuIdle and
// idleKnown are the sampled idle share and whether the sample could be read
// at all, so a caller can tell a host it read as idle from one it could not
// read. An unread host holds nothing, because a CPU that could not be
// measured is no evidence of a busy one and holding on it would stop every
// suite start where top cannot be driven (gt-6rieh). proc supplies the
// working directory each segment is judged in (idleGateHeldAction), which
// decides whether a make target is the whole-module Go action at all.
func evaluateIdleGate(proc guardProcess, command string, hostCPUIdle func() (float64, bool)) (cpuIdle float64, idleKnown bool, heldAction string) {
	heldAction = idleGateHeldAction(proc, command)
	if heldAction == "" {
		return 0, false, ""
	}
	cpuIdle, idleKnown = hostCPUIdle()
	if !idleKnown || cpuIdle >= idleGateCPUIdleThreshold {
		return cpuIdle, idleKnown, ""
	}
	return cpuIdle, idleKnown, heldAction
}

// maxDangerousNestDepth bounds the nested-payload recursion in
// evaluateDangerousCommand so a pathological input (e.g. deeply chained
// "eval eval eval ...") can't loop unboundedly. The bound fails closed: a
// payload nested past it is refused rather than allowed unjudged (gt-7ydceb).
const maxDangerousNestDepth = 3

// evaluateDangerousCommand runs every dangerous-pattern check against
// command and, up to maxDangerousNestDepth, recurses into every command line
// it runs through another shell: a -c payload, an eval string, a script a
// shell reads on stdin (a here-string, a pipeline producer, a process
// substitution run as the script), a gt slot run argv, a heredoc body a shell
// reads, and every command and process substitution. Without this, a wrapper
// like `bash -c "git reset --hard"` was invisible to every check: shlex
// collapses the quoted payload into a single token, so none of the
// fragment-based matchers (which require each fragment as its own token) ever
// fire on it. Quoted text that is NOT one of these shell-executing forms (a
// SQL string, a mail body, a jq/sed script) deliberately stays opaque — that
// is where this guard's real false positives have come from (gt-5ihs attempt
// 2, gt-wisp-db27 finding 4).
//
// The nested command lines come from the parsed Script — its Segments, Subs
// and Heredocs, the way the container-suite guard reads them. Every payload is
// parsed before it is judged, so a fragment the shell never runs is never
// judged; one the parser cannot read is refused with the construct named,
// because the parser's reading is the only structured reading the nested
// checks have (gt-7ydceb). Bash-valid shapes the parser misreads are refused
// by the same gate, except the two judgeDisagreeingShellText reads; that
// fail-closed trade is decided in gt-wnblap.
func evaluateDangerousCommand(command string, depth int, sess guardSession) (reason, alternative string) {
	script, err := shellparse.Parse(command)
	if err != nil {
		// The parser could not read the command, so the command is refused whole,
		// with the construct the parser named. No second reader judges its text
		// for a dangerous operation to name instead (gt-a7jzr5).
		if depth > 0 {
			return dangerousNestedUnreadable(err), ""
		}
		return dangerousUnreadable(err), ""
	}
	return judgeDangerousScript(script, command, depth, sess)
}

// judgeDangerousScript judges one command line the parser read. The token
// matchers read the words the shell will pass — quoting resolved, expansions
// kept as source text, redirections and here-document bodies out of the word
// list — instead of a second tokenization of the text, so the parser's reading
// is the reading the matchers take (gt-kr5vf). The done/slot loop rule keeps a
// text reading for its loop keywords, which are not segments
// (matchesDoneSlotLoop, run by judgeDangerousLoopText), and reads the
// substitutions it judges from the parsed script (dangerousSubstitutionLoop).
// The two shapes the parser reads differently from the shell keep the shell's
// text for their token checks (judgeDisagreeingScript).
func judgeDangerousScript(script shellparse.Script, text string, depth int, sess guardSession) (reason, alternative string) {
	// A declaration builtin's word spelling "<<" is a here-document to the
	// shell's lexer, which reads the operator before the builtin sees its
	// argument: "let y=1<<x" feeds the next lines to the body as data. The
	// parser reads the word as an arithmetic expression and reports no
	// Heredoc there, so the line is judged by judgeDisagreeingScript, which
	// follows the shell for its token checks rather than the parsed words that
	// would judge the body as live commands.
	if dangerousDeclarationHeredoc(script) || dangerousANSICContinuation(text) {
		return judgeDisagreeingScript(script, text, depth, sess)
	}
	tokens := dangerousTokenLine(script)
	if reason, alternative = judgeDangerousTokens(tokens, toLowerTokens(tokens), sess); reason != "" {
		return reason, alternative
	}
	if reason, alternative = judgeDangerousLoopText(text, script, depth); reason != "" {
		return reason, alternative
	}
	return evaluateDangerousScriptNested(script, depth, sess)
}

// dangerousDeclarationBuiltins are the builtins shellparse reads as a
// declaration clause: their assignments are arguments of the builtin, after
// its own name, not the command prefix a plain command carries.
var dangerousDeclarationBuiltins = map[string]bool{
	"declare": true, "local": true, "export": true, "readonly": true, "typeset": true, "let": true,
}

// dangerousTokenLine rebuilds the token line the dangerous matchers read: each
// segment's words and assignments in the order the shell reads them, the
// operator that ends the segment, and a "(" / ")" pair around the segments a
// subshell encloses. It is containerSuiteWalkLine with two corrections the
// matchers need: a declaration builtin's assignments follow its name (they are
// arguments to it), and an assignment keeps the "=" shellparse's text drops
// from a valueless one ("BD_ACTOR=" reads as "BD_ACTOR"), so a matcher that
// looks for an assignment sees one.
func dangerousTokenLine(script shellparse.Script) []string {
	var tokens []string
	var shells []int
	for _, seg := range script.Segments {
		common := containerSuiteCommonShells(shells, seg.Shells)
		for len(shells) > common {
			tokens = append(tokens, ")")
			shells = shells[:len(shells)-1]
		}
		for len(shells) < len(seg.Shells) {
			tokens = append(tokens, "(")
			shells = append(shells, seg.Shells[len(shells)])
		}
		declaration := len(seg.Words) == 1 && dangerousDeclarationBuiltins[seg.Words[0].Text]
		if !declaration {
			for _, a := range seg.Assigns {
				tokens = append(tokens, dangerousAssignToken(a))
			}
		}
		for _, w := range seg.Words {
			tokens = append(tokens, w.Text)
		}
		if declaration {
			for _, a := range seg.Assigns {
				tokens = append(tokens, dangerousAssignToken(a))
			}
		}
		if seg.After != "" {
			tokens = append(tokens, seg.After)
		}
	}
	for len(shells) > 0 {
		tokens = append(tokens, ")")
		shells = shells[:len(shells)-1]
	}
	return tokens
}

// dangerousAssignToken is one assignment as the matchers read it. shellparse's
// text loses the "=" of a valueless assignment ("BD_ACTOR" for "BD_ACTOR="), and
// a declaration builtin's flags arrive as assignments whose text is "=-x", so
// the raw source is used for those; every other assignment keeps its decoded
// text, where a quoted value's quotes are already resolved.
func dangerousAssignToken(a shellparse.Word) string {
	if strings.HasPrefix(a.Raw, "-") || !strings.Contains(a.Text, "=") {
		return a.Raw
	}
	return a.Text
}

// dangerousDeclarationHeredoc reports whether the parsed script holds a
// declaration builtin whose word spells "<<". The shell's lexer reads that
// operator before the builtin sees its argument, so bash reads a here-document
// there while shellparse parses the word as an arithmetic expression and
// reports no Heredoc: "let y=1<<x" hands the following lines to the body as
// data. The "a shift in a let argument is a real heredoc" row of
// TestHeredocDoesNotHideRealCommand pins that reading.
func dangerousDeclarationHeredoc(script shellparse.Script) bool {
	for _, seg := range script.Segments {
		if len(seg.Words) == 0 {
			continue
		}
		switch seg.Words[0].Text {
		case "declare", "local", "export", "readonly", "typeset", "let":
		default:
			continue
		}
		for _, w := range seg.Words[1:] {
			if strings.Contains(w.Text, "<<") {
				return true
			}
		}
	}
	return false
}

// dangerousANSICContinuation reports whether text opens an ANSI-C string across
// a backslash-newline ("$\\\n'"). bash deletes the continuation before it reads
// the quote, so it reads a $'...' string; shellparse reads a parameter
// expansion and splits the line differently (gt-javvg). judgeDisagreeingShellText
// judges a line with this shape on its text, as bash reads it.
func dangerousANSICContinuation(text string) bool {
	for i := 0; i+1 < len(text); i++ {
		if text[i] != '$' {
			continue
		}
		q := skipLineContinuationBytes(text, i+1)
		if q > i+1 && q < len(text) && text[q] == '\'' {
			return true
		}
	}
	return false
}

// judgeDisagreeingScript judges a line the parser reads differently from the
// shell. The token checks and the loop rule read the shell's reading of the
// text (judgeDisagreeingShellText); then the nested payloads the parser did read
// on that line are judged as on any other line, because the shell runs each one
// live. A payload the parser places elsewhere is not judged here; the parser's
// reading is the only structured reading the nested checks have (gt-7ydceb).
// Pinned by TestDisagreeingShapesJudgeNestedPayloads.
func judgeDisagreeingScript(script shellparse.Script, text string, depth int, sess guardSession) (reason, alternative string) {
	if reason, alternative = judgeDisagreeingShellText(text, script, depth, sess); reason != "" {
		return reason, alternative
	}
	return evaluateDangerousScriptNested(script, depth, sess)
}

// judgeDisagreeingShellText judges a command the parser reads differently from
// the shell, with the token checks on the text's own token line, then the
// done/slot loop rule. It covers exactly two shapes: a declaration builtin's
// "<<" word, which the shell reads as a here-document operator and the parser
// does not (declarationHeredocReaders strips its body), and an ANSI-C string
// opened across a backslash-newline, which the shell joins before it reads the
// quote and the parser does not (gt-javvg). The parser's tree is not the shell's
// reading of these lines, so the text is the only reading the matchers have
// there. Pinned by TestHeredocDoesNotHideRealCommand and
// TestDangerousGuardANSICLineContinuation.
func judgeDisagreeingShellText(command string, script shellparse.Script, depth int, sess guardSession) (reason, alternative string) {
	tokens := shellTokenize(stripHeredocBodies(command))
	if reason, alternative = judgeDangerousTokens(tokens, toLowerTokens(tokens), sess); reason != "" {
		return reason, alternative
	}
	return judgeDangerousLoopText(command, script, depth)
}

// judgeDangerousLoopText is the done/slot loop rule read from the command text.
// It is the one nested read the parsed Script cannot feed that rule: the
// flattener records what a shell runs, and a for or while block's keywords open
// and close around their segments instead of being segments of their own ("for
// i in 1 2; do gt done; done" flattens to one "gt done" segment). The rule is a
// shape of the text — a block that CLOSES around a gt done or gt slot, a repeat
// or an xargs beside one (gt-7dxw) — so it reads the text, not the parsed
// Script. It answers only that rule: every other nested judgment comes from the
// parsed Script (gt-7ydceb), and a file-written body it cannot read is refused,
// not passed (doneSlotLoopFileWrittenBodies). The rule reads the unstripped text
// because a heredoc body the command writes to a FILE can hold the retry script;
// the heredoc bodies it finds are the parsed ones (doneSlotLoopFileWrittenBodies).
func judgeDangerousLoopText(text string, script shellparse.Script, depth int) (reason, alternative string) {
	tokens := shellTokenize(stripHeredocBodies(text))
	if reason, alternative = matchesDoneSlotLoop(text, tokens, toLowerTokens(tokens)); reason != "" {
		return reason, alternative
	}
	return dangerousSubstitutionLoop(script, depth)
}

// judgeDangerousTokens is the dangerous rules read over one token line.
func judgeDangerousTokens(tokens, lowerTokens []string, sess guardSession) (reason, alternative string) {
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
	if r, alt := matchesPolecatAttribution(tokens, inPolecatSession(sess.proc), polecatOwnIdentities(sess.proc)); r != "" {
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
	return "", ""
}

// evaluateDangerousScriptNested judges the command lines one parsed script runs
// through another shell, up to maxDangerousNestDepth; a script that nests past
// that depth is refused, never allowed unjudged (gt-7ydceb). The text payloads
// are judged as text — each is a command line of its own, parsed and gated by
// evaluateDangerousCommand — and every substitution the shell expands is judged
// as the already-parsed script it is.
func evaluateDangerousScriptNested(script shellparse.Script, depth int, sess guardSession) (reason, alternative string) {
	payloads := nestedCommands(script)
	payloads = append(payloads, dangerousHeredocBodies(script)...)
	subs := dangerousScriptSubs(script)
	subs = append(subs, dangerousRecoveredSubs(script)...)
	// A script that nests nothing is judged in full: its own words are the
	// whole of what it runs. One that nests something at the cap has reached
	// an unjudged payload, so the cap refuses it (gt-7ydceb).
	if depth >= maxDangerousNestDepth {
		if len(payloads) > 0 || len(subs) > 0 {
			return dangerousNestTooDeep(), ""
		}
		return "", ""
	}
	for _, payload := range payloads {
		if r, alt := evaluateDangerousCommand(payload, depth+1, sess); r != "" {
			return r, alt
		}
	}
	for _, sub := range subs {
		if r, alt := evaluateDangerousScript(sub, depth+1, sess); r != "" {
			return r, alt
		}
	}
	return "", ""
}

// evaluateDangerousScript judges one command line a shell runs from a
// substitution — "$(...)", "<(...)" or `...` — with the same rules, then
// recurses into what it nests. The substitution was parsed as part of the
// command that held it, so there is no text left to gate: whatever it runs, the
// parser read.
func evaluateDangerousScript(script shellparse.Script, depth int, sess guardSession) (reason, alternative string) {
	// The script is judged on the token line rebuilt from its segments, as the
	// top-level script is in judgeDangerousScript. The done/slot
	// loop rule is not read here: a substitution keeps no text, so that rule
	// cannot see a loop in its body, and dangerousSubstitutionLoop carries it
	// from the parsed script of the line that holds the substitution (gt-7ydceb).
	tokens := dangerousTokenLine(script)
	if reason, alternative = judgeDangerousTokens(tokens, toLowerTokens(tokens), sess); reason != "" {
		return reason, alternative
	}
	return evaluateDangerousScriptNested(script, depth, sess)
}

// nestedCommands returns the command lines a parsed script hands to another
// shell: a gt slot run argv, a -c payload, an eval string and a script a shell
// reads on stdin. Each is judgement text: it is parsed by the caller, and
// refused when the parser cannot read it.
func nestedCommands(script shellparse.Script) []string {
	tokens, _ := containerSuiteWalkLine(script)
	payloads := slotRunPayloads(tokens)
	for i := range script.Segments {
		words := containerSuiteWords(script.Segments[i])
		payloads = append(payloads, shellCPayloads(words)...)
		payloads = append(payloads, evalPayloads(words)...)
		payloads = append(payloads, containerSuiteStdinPayloads(script, i)...)
	}
	return payloads
}

// dangerousHeredocBodies returns the here-document bodies a shell runs as a
// script. A body is data to its reader unless a shell invoker runs on its
// reader line — the segment itself or one joined to it by a non-";" operator,
// so "bash <<EOF" and "cat <<EOF | bash" both count while a bash on a later
// line does not — and only stdin's body feeds a shell its script (gt-9g0y,
// gt-ohe8n).
func dangerousHeredocBodies(script shellparse.Script) []string {
	var bodies []string
	for i, seg := range script.Segments {
		if !containerSuiteHeredocReadAsScript(script, i) {
			continue
		}
		for _, h := range seg.Heredocs {
			if h.N != "" && h.N != "0" {
				continue
			}
			if strings.TrimSpace(h.Body) == "" {
				continue
			}
			bodies = append(bodies, h.Body)
		}
	}
	return bodies
}

// dangerousRecoveredSubs returns the process substitutions the parser could not
// read. mvdan reads the word of a "${..}" expansion as one literal, so a
// "<(...)" written inside it ("echo ${v:-<(bash <<EOF...)}") carries no
// ProcSubst, while the shell runs it all the same: the body of a heredoc there
// is read by the process substitution's command. The text inside the parens is
// cut out and parsed as the script it is, and judged like any other
// substitution. Only an unquoted word is read: a quoted "<(...)" is text the
// shell passes through (gt-lx8i8). Kept until shellparse reports these: gt-70q8j2.
func dangerousRecoveredSubs(script shellparse.Script) []shellparse.Script {
	var subs []shellparse.Script
	var recover func(s shellparse.Script)
	recoverWord := func(w shellparse.Word) {
		if len(w.ProcSubs) > 0 || w.Raw != w.Text {
			return
		}
		for i := 0; i+1 < len(w.Raw); i++ {
			if (w.Raw[i] != '<' && w.Raw[i] != '>') || w.Raw[i+1] != '(' {
				continue
			}
			end := closingParen(w.Raw, i+2, heredocBodiesFrom(w.Raw, i+2))
			if end < 0 {
				continue
			}
			if inner, err := shellparse.Parse(w.Raw[i+2 : end]); err == nil {
				subs = append(subs, inner)
			}
			i = end
		}
	}
	recover = func(s shellparse.Script) {
		for _, seg := range s.Segments {
			for _, w := range seg.Words {
				recoverWord(w)
			}
			for _, a := range seg.Assigns {
				recoverWord(a)
			}
			for _, sub := range seg.Subs {
				recover(sub)
			}
		}
		for _, sub := range s.Subs {
			recover(sub)
		}
	}
	recover(script)
	return subs
}

// dangerousScriptSubs returns every substitution a parsed script makes the
// shell expand, as the scripts they are: command and process substitutions in
// words, assignment values, redirection targets and here-document bodies, plus
// the ones written in a construct header (a for loop's list, a case word).
func dangerousScriptSubs(script shellparse.Script) []shellparse.Script {
	var subs []shellparse.Script
	for _, seg := range script.Segments {
		subs = append(subs, seg.Subs...)
	}
	return append(subs, script.Subs...)
}

// dangerousSubstitutionLoop is the done/slot loop rule read from the
// substitutions the parser reads in script: the bodies of "$(...)", backquotes,
// and process substitutions, including those in an unquoted heredoc body, and
// those in a quoted heredoc body read as a script (quotedHeredocBodyLoop). The
// flattener records what a shell runs, and a for or while block's keywords open
// and close around their segments instead of being segments of their own ("for
// i in 1 2; do gt done; done" flattens to one "gt done" segment). The rule is a
// shape of the text — a block that CLOSES around a gt done or gt slot, a repeat
// or an xargs beside one (gt-7dxw) — so each body's text is read for it, and a
// body's own substitutions are read one level deeper from its parsed Script. It
// answers only that rule: every other nested judgment comes from the parsed
// Script (gt-7ydceb).
func dangerousSubstitutionLoop(script shellparse.Script, depth int) (reason, alternative string) {
	if depth >= maxDangerousNestDepth {
		return "", ""
	}
	for _, sub := range dangerousScriptSubs(script) {
		body := sub.Raw
		if body == "" {
			continue
		}
		tokens := shellTokenize(body)
		if r, alt := matchesDoneSlotLoop(body, tokens, toLowerTokens(tokens)); r != "" {
			return r, alt
		}
		if r, alt := dangerousSubstitutionLoop(sub, depth+1); r != "" {
			return r, alt
		}
	}
	for _, seg := range script.Segments {
		for _, h := range seg.Heredocs {
			if !h.Quoted {
				continue
			}
			if r, alt := quotedHeredocBodyLoop(h.Body, depth); r != "" {
				return r, alt
			}
		}
	}
	return "", ""
}

// quotedHeredocBodyLoop is the substitution half of the done/slot loop rule for
// the body of a quoted here-document. The shell keeps that body literal, so the
// parsed script holds no substitution in it, yet the body is what a file it is
// written to later runs, and what a shell it is fed to runs. The body is read as
// a script of its own and judged as a command line is, so a loop in one of its
// substitutions is refused whether or not a file is written (gt-bogfce). A body
// the parser cannot read that may hold a substitution is refused too, since no
// substitution in it can be ruled out.
func quotedHeredocBodyLoop(body string, depth int) (reason, alternative string) {
	script, err := shellparse.Parse(body)
	if err != nil {
		if mayHoldSubstitution(body) {
			return quotedHeredocBodyUnreadableReason(err), ""
		}
		return "", ""
	}
	return dangerousSubstitutionLoop(script, depth+1)
}

// quotedHeredocBodyUnreadableReason is the refusal for a quoted here-document
// body the parser cannot read and that may hold a substitution, naming the
// construct.
func quotedHeredocBodyUnreadableReason(err error) string {
	return "a quoted here-document body cannot be read (" + dangerousConstruct(err) + ") and may hold a substitution, so it cannot be checked for a retry loop"
}

// dangerousUnreadable is the refusal for a command shellparse could not read,
// naming the construct. Every session is refused: tapGuardDangerous judges
// every command, so there is no role the guard leaves unbound. The parser's
// own text is not echoed; an error that is not an *shellparse.UnparseableError
// keeps the generic construct name.
func dangerousUnreadable(err error) string {
	return "command cannot be read (" + dangerousConstruct(err) + "), so it cannot be checked for dangerous operations"
}

// dangerousNestedUnreadable is the refusal for a nested command line the parser
// could not read. Its text is refused the way a top-level command is, and for
// the same reason: nothing can say what it runs. The construct is named so the
// operator can see what was rejected.
func dangerousNestedUnreadable(err error) string {
	return "nested command cannot be read (" + dangerousConstruct(err) + "), so it cannot be checked for dangerous operations"
}

// dangerousNestTooDeep is the refusal for a payload nested past
// maxDangerousNestDepth (gt-7ydceb). It names the cap so the operator can tell
// it from a payload the parser could not read.
func dangerousNestTooDeep() string {
	return fmt.Sprintf("nested command is deeper than maxDangerousNestDepth (%d), so it cannot be checked for dangerous operations", maxDangerousNestDepth)
}

// dangerousConstruct is the shellparse name of the construct that made a
// command unreadable, or a generic name for a failure the parser did not name.
func dangerousConstruct(err error) string {
	var unparseable *shellparse.UnparseableError
	if errors.As(err, &unparseable) && unparseable.Construct != "" {
		return unparseable.Construct
	}
	return "shell syntax"
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
//
// Each o or O in a cluster ("-eo", "-euo", "-oc", "-O") owns the next word as
// its value, so the payload is the word after the values.
func shellCPayloadIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return -1
		case a == "--rcfile" || a == "--init-file":
			i++ // the option's separate value
		case strings.HasPrefix(a, "--"):
			// a long option such as --norc or --login
		case len(a) > 1 && a[0] == '+':
			// +x style option negation
			i += shellClusterValues(a)
		case len(a) > 1 && a[0] == '-':
			values := shellClusterValues(a)
			payload := i + 1 + values
			i += values
			if strings.Contains(a[1:], "c") {
				if payload < len(args) {
					return payload
				}
				return -1
			}
		default:
			return -1
		}
	}
	return -1
}

// shellClusterValues returns how many of the words after the shell option word
// a ("-o", "+o", "-eo", "-oc", "-O") it owns as values: one for each o (-o
// pipefail) and O (-O extglob) letter, wherever it sits in the cluster, since
// the shell reads the letters in order. shellCPayloadIndex and shellReadsStdin
// walk the words before a script or -c payload past them.
func shellClusterValues(a string) int {
	if len(a) < 2 || (a[0] != '-' && a[0] != '+') || a[1] == '-' {
		return 0
	}
	return strings.Count(a[1:], "o") + strings.Count(a[1:], "O")
}

// slotRunPayloads returns, for every `gt slot run [flags] <argv>` the command
// really runs, the argv as a command line of its own. slot.Run execs that
// argv under the slot, so `gt slot run -- git reset --hard` is a git reset;
// inCommandPosition treats a segment opening with gt as text-only, which hid
// it from every matcher (gt-hqenc). slot's argv needs no `--` to be exec'd, so
// the payload is the argv slot.Run will be handed either way (gt-sni9y). Each
// argv word is re-quoted, so a word with spaces (a commit message that names a
// dangerous command) stays one word when the payload is judged. Every other gt
// subcommand keeps its text-only treatment. tokens are shell-aware tokens (see
// shellTokenize).
func slotRunPayloads(tokens []string) []string {
	var payloads []string
	for _, segment := range splitShellSegments(tokens) {
		for i := 0; i+2 < len(segment); i++ {
			if filepath.Base(strings.ToLower(segment[i])) != "gt" ||
				!strings.EqualFold(segment[i+1], "slot") ||
				!strings.EqualFold(segment[i+2], "run") ||
				(i > 0 && !inCommandPosition(segment, i)) {
				continue
			}
			argv := slotRunArgv(segment[i+3:])
			if len(argv) == 0 {
				continue
			}
			quoted := make([]string, 0, len(argv))
			for _, word := range argv {
				quoted = append(quoted, shellQuoteWord(word))
			}
			payloads = append(payloads, strings.Join(quoted, " "))
		}
	}
	return payloads
}

// slotRunFlagNames are `gt slot run`'s own flags that take a value
// (internal/cmd/slot.go). A word naming one is never the argv — pflag pulls it,
// and the value beside it, out of the operator's words — while every other
// flag-shaped word is: cobra fails the command on a flag it does not know, so
// "-rf" in `gt slot run rm -rf /` is argv, and dropping it would drop the
// command the payload is there to judge.
var slotRunFlagNames = map[string]bool{"role": true, "timeout": true, "nice": true}

// slotRunArgv returns the argv `gt slot run` hands to exec, read from the
// operator's words after "run". pflag takes slot's own value flags out of
// those words wherever they sit, in either the separate (--role X) or glued
// (--role=X) form, so the command starts at the first word left; a `--`
// terminates that parsing and is dropped, and the words after it are
// positional whatever they look like (gt-sni9y). words kept rather than
// dropped matter as much as words dropped: the returned argv is what the slot
// will really run, so judging it matches judging the command.
func slotRunArgv(words []string) []string {
	var argv []string
	for i := 0; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			return append(argv, words[i+1:]...)
		}
		flag, glued := slotRunFlag(word)
		if flag == "" {
			argv = append(argv, word)
			continue
		}
		if !glued {
			i++ // the flag's own value word
		}
	}
	return argv
}

// slotRunFlag returns the name of the `gt slot run` flag word names ("" when
// it names none) and whether word glued that flag's value to it with "=", so
// only an unglued flag still owes the walk its value word.
func slotRunFlag(word string) (string, bool) {
	if len(word) < 2 || word[0] != '-' {
		return "", false
	}
	name, _, glued := strings.Cut(word, "=")
	name = strings.TrimLeft(name, "-")
	if !slotRunFlagNames[name] {
		return "", false
	}
	return name, glued
}

// shellQuoteWord single-quotes word unless it is made only of characters the
// shell never treats specially, so shellTokenize reads the result back as the
// one word it came from.
func shellQuoteWord(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// heredocStartPattern matches a heredoc redirection operator: "<<TAG",
// "<<-TAG" (strip-leading-tabs form), or either with the tag quoted
// ("<<'TAG'", `<<"TAG"`). A guard that only needs to know whether a line
// carries one (polecat_paths) reads it; the body spans come from heredocSpans.
var heredocStartPattern = regexp.MustCompile(`<<(-?)[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// heredocSpan is one here-document body found in a command's text: where the
// body starts and ends in the source, the body text, and the two states a
// caller acts on. bodyEnd is past the delimiter line, where the shell resumes
// reading commands, so stripping [bodyStart,bodyEnd) removes the body and its
// terminator together. quoted is true when the delimiter is quoted: the shell
// leaves such a body literal, while an unquoted one gets its $(...) and
// backticks expanded (gt-x50o8). unterminated is true when no line equals the
// delimiter, so the body runs to the end of the text — a span the strippers
// refuse to act on (gt-79opc).
type heredocSpan struct {
	bodyStart    int
	bodyEnd      int
	body         string
	quoted       bool
	unterminated bool

	// readerPipelineHasShell is true when a shell invoker shares the pipeline
	// that holds the operator ("cat <<EOF | bash"), so the shell runs the body
	// as a script rather than reading it as data. A shell on another stage of
	// the same "&&" line does not read this body. It is set from the parsed
	// script.
	readerPipelineHasShell bool
}

// heredocReader is one parsed here-document together with the source offset
// of the end of the segment that reads it. The body follows the newline that
// ends that segment's logical line, so the offset is what places the body in
// the text. readerPipelineHasShell says whether the operator's pipeline runs a
// shell.
type heredocReader struct {
	doc                    shellparse.Heredoc
	readerEnd              int
	readerPipelineHasShell bool
}

// heredocSpans returns the here-document bodies in text, in source order. The
// parsed Script decides them: a "<<" is a redirection only where the shell reads
// one, so a shift in an arithmetic expression, a "${v%<<x}" pattern, a comment
// and a quoted string are excluded by the parser rather than by a second reader
// of the shell's syntax (gt-yza8u, gt-dherr, gt-adt8r, gt-lx8i8). The one reader
// the parser does not report is a declaration builtin's "<<" word, which
// declarationHeredocReaders adds. Text the parser refuses has no spans, so
// stripHeredocBodies returns it whole.
func heredocSpans(text string) []heredocSpan {
	script, err := shellparse.Parse(text)
	if err != nil {
		return nil
	}
	return heredocSpansFromScript(text, script)
}

// declarationHeredocReaders returns the here-documents a declaration builtin
// reads. The shell's lexer reads a "<<" word of the builtin's arguments as a
// here-document operator before the builtin sees it, so "let y=1<<x" hands the
// following lines to the shell as a body; the parser reads the word as an
// arithmetic expression and reports no Heredoc (dangerousDeclarationHeredoc).
// Only a plain word is read: one with quotes, expansions or a backslash is not
// an operator the shell reads whole, and it is left to the parser. The
// delimiter is the text after the operator, or the next word when the operator
// ends its word. A reader whose position the parser did not give is reported
// by heredocReadersUnplaced.
func declarationHeredocReaders(script shellparse.Script) []heredocReader {
	var readers []heredocReader
	for i, seg := range script.Segments {
		if len(seg.Words) == 0 || !dangerousDeclarationBuiltins[seg.Words[0].Text] {
			continue
		}
		for j := 1; j < len(seg.Words); j++ {
			raw := seg.Words[j].Raw
			if strings.ContainsAny(raw, "$'\"`\\") {
				continue
			}
			k := strings.Index(raw, "<<")
			if k < 0 {
				continue
			}
			tag := strings.TrimPrefix(raw[k+2:], "-")
			if tag == "" && j+1 < len(seg.Words) {
				tag = seg.Words[j+1].Raw
			}
			if tag == "" || strings.ContainsAny(tag, "$'\"`\\") {
				continue
			}
			doc := shellparse.Heredoc{Delimiter: tag}
			readers = append(readers, heredocReader{doc: doc, readerEnd: segmentSourceEnd(seg), readerPipelineHasShell: heredocPipelineRunsShell(script, i)})
		}
	}
	return readers
}

// heredocReadersUnplaced reports whether a here-document of script has a reader
// whose source position the parser did not give, so its body cannot be placed
// in the text. A caller that acts on bodies in the text refuses such a command
// rather than read the body at a guessed offset.
func heredocReadersUnplaced(script shellparse.Script) bool {
	var readers []heredocReader
	collectHeredocReaders(script, &readers)
	for _, r := range append(readers, declarationHeredocReaders(script)...) {
		if r.readerEnd < 0 {
			return true
		}
	}
	return false
}

// heredocSpansFromScript places the here-documents of a parsed script in text.
// A body starts after the newline that ends its reader's logical line — a
// backslash-newline continues that line, so the body only starts past the
// continuation — and ends past the line that equals the delimiter. Bodies are
// read in reader order, so a later body starts where the earlier one ended. A
// reader whose position is unknown has no span here; heredocReadersUnplaced
// lets the caller refuse the command instead.
func heredocSpansFromScript(text string, script shellparse.Script) []heredocSpan {
	var readers []heredocReader
	collectHeredocReaders(script, &readers)
	readers = append(readers, declarationHeredocReaders(script)...)
	var placed []heredocReader
	for _, r := range readers {
		if r.readerEnd >= 0 {
			placed = append(placed, r)
		}
	}
	readers = placed
	sort.SliceStable(readers, func(i, j int) bool { return readers[i].readerEnd < readers[j].readerEnd })
	var spans []heredocSpan
	cursor := 0
	for _, r := range readers {
		start := heredocLineEndAfter(text, r.readerEnd)
		if start < cursor {
			start = cursor
		}
		end, ok := heredocDelimiterEnd(text, start, r.doc.Delimiter)
		if !ok {
			// No line equals the delimiter: the body is the rest of the text.
			// The span is recorded, but nothing strips it (gt-79opc).
			spans = append(spans, heredocSpan{bodyStart: start, bodyEnd: len(text), body: text[start:], quoted: r.doc.Quoted, unterminated: true, readerPipelineHasShell: r.readerPipelineHasShell})
			cursor = len(text)
			continue
		}
		spans = append(spans, heredocSpan{bodyStart: start, bodyEnd: end, body: text[start:end], quoted: r.doc.Quoted, readerPipelineHasShell: r.readerPipelineHasShell})
		cursor = end
	}
	return spans
}

// collectHeredocReaders appends every here-document in script, its substitutions
// included, with the source end of the segment that reads it. A substitution
// keeps its here-documents in the nested Script the parser gives it, and their
// bodies are in the same source text.
func collectHeredocReaders(script shellparse.Script, out *[]heredocReader) {
	for i, seg := range script.Segments {
		if len(seg.Heredocs) > 0 {
			end := segmentSourceEnd(seg)
			shell := heredocPipelineRunsShell(script, i)
			for _, h := range seg.Heredocs {
				*out = append(*out, heredocReader{doc: h, readerEnd: end, readerPipelineHasShell: shell})
			}
		}
		for _, sub := range seg.Subs {
			collectHeredocReaders(sub, out)
		}
	}
	for _, sub := range script.Subs {
		collectHeredocReaders(sub, out)
	}
}

// segmentSourceEnd returns the offset just past the last word or own
// redirection of seg, or -1 when the parser gave none of them a position.
// Inherited redirections are left out: they belong to an enclosing compound
// command, whose redirect may sit after the here-document body the segment
// reads. Assignment words carry no position in shellparse, so they are left
// out too; a segment that is only an assignment and a here-document has no
// position, and its body is reported by heredocReadersUnplaced.
func segmentSourceEnd(seg shellparse.Segment) int {
	end := -1
	for _, w := range seg.Words {
		if w.Src >= 0 && w.Src+len(w.Raw) > end {
			end = w.Src + len(w.Raw)
		}
	}
	for _, r := range seg.Redirs {
		if r.Inherited || r.Src < 0 {
			continue
		}
		if e := r.Src + len(r.Word.Raw); e > end {
			end = e
		}
	}
	return end
}

// heredocLineEndAfter returns the offset just past the newline that ends the
// logical line starting at or after from, skipping backslash-newline pairs the
// shell deletes. At the end of the text it returns its length.
func heredocLineEndAfter(text string, from int) int {
	for i := from; i < len(text); i++ {
		if text[i] == '\n' {
			return i + 1
		}
		if text[i] == '\\' && i+1 < len(text) && text[i+1] == '\n' {
			i++
		}
	}
	return len(text)
}

// heredocDelimiterEnd returns the offset just past the first line at or after
// bodyStart whose content is the delimiter, and false when no such line exists.
// A tab-indented line is accepted for either operator. Only "<<-" strips tabs
// (bash does not strip them for "<<"), but the parsed Heredoc keeps no dash flag
// (shellparse is out of scope here), so the tab-indented spelling is taken for
// both. That is safe in one direction only: a plain "<<" body that a
// tab-indented line ends early leaves the rest of the body as text the
// matchers judge as live, and the stripped span stays inside the real body, so
// no live command is ever stripped as data.
func heredocDelimiterEnd(text string, bodyStart int, delimiter string) (int, bool) {
	for offset := bodyStart; offset <= len(text); {
		lineEnd := strings.IndexByte(text[offset:], '\n')
		line, next := text[offset:], len(text)
		if lineEnd >= 0 {
			line, next = text[offset:offset+lineEnd], offset+lineEnd+1
		}
		if line == delimiter || strings.TrimLeft(line, "\t") == delimiter {
			return next, true
		}
		if lineEnd < 0 {
			break
		}
		offset = next
	}
	return len(text), false
}

// stripHeredocBodies removes heredoc body text from command before any
// tokenization or pattern matching runs. A heredoc body is DATA being
// written to a file or piped to a command's stdin — the same class of
// risk as a quoted SQL string or a mail body, not shell syntax to
// evaluate — so scanning it for dangerous fragments produces false
// positives: "cat > note.md <<'EOF' ... git push --force ... EOF" blocked
// an ordinary file write because the DATA it wrote happened to mention a
// dangerous phrase (gt-mkrj). An unterminated heredoc is left in place: its
// body is the whole tail of the text, so stripping on a match that may not
// even be a redirection erased every command after it (gt-79opc).
func stripHeredocBodies(command string) string {
	stripped, _ := stripHeredocBodiesWithCuts(command)
	return stripped
}

// heredocBodyCut is one heredoc body stripHeredocBodies removed: at is the
// offset in the stripped text the body was cut at, and length the bytes it
// took out of the command.
type heredocBodyCut struct {
	at, length int
}

// stripHeredocBodiesWithCuts is stripHeredocBodies with the cut it made at each
// body, in order, so an offset in the stripped text maps back to the command
// (commandOffset).
func stripHeredocBodiesWithCuts(command string) (string, []heredocBodyCut) {
	var b strings.Builder
	var cuts []heredocBodyCut
	pos := 0
	for _, s := range heredocSpans(command) {
		if s.unterminated || s.bodyEnd <= s.bodyStart {
			continue
		}
		if s.bodyStart < pos {
			continue
		}
		b.WriteString(command[pos:s.bodyStart])
		cuts = append(cuts, heredocBodyCut{at: b.Len(), length: s.bodyEnd - s.bodyStart})
		pos = s.bodyEnd
	}
	b.WriteString(command[pos:])
	return b.String(), cuts
}

// commandOffset maps an offset in the text stripHeredocBodies returned to the
// same byte of the command. A body cut at the offset sat before that byte, so
// the byte lies past it.
func commandOffset(cuts []heredocBodyCut, offset int) int {
	out := offset
	for _, c := range cuts {
		if c.at <= offset {
			out += c.length
		}
	}
	return out
}

// heredocPipelineRunsShell reports whether a shell invoker shares the pipeline
// that holds the segment's here-document operator: "cat <<EOF | bash" feeds bash
// the body, while a shell joined to the line by "&&" or ";" runs before or after
// it and reads no body from it.
func heredocPipelineRunsShell(script shellparse.Script, index int) bool {
	lo, hi := index, index
	for lo > 0 && script.Segments[lo-1].After == "|" {
		lo--
	}
	for hi+1 < len(script.Segments) && script.Segments[hi].After == "|" {
		hi++
	}
	for j := lo; j <= hi; j++ {
		word, _ := segmentCommandWordPastRedirections(containerSuiteWords(script.Segments[j]))
		if shellInvokers[strings.ToLower(filepath.Base(word))] {
			return true
		}
	}
	return false
}

// heredocReaderLineRunsShell reports whether a shell invoker runs on the reader
// line the segment at index sits on, which is what makes the here-documents on
// it scripts rather than data.
func heredocReaderLineRunsShell(script shellparse.Script, index int) bool {
	lo, hi := index, index
	for lo > 0 && heredocLineOperator(script.Segments[lo-1].After) {
		lo--
	}
	for hi+1 < len(script.Segments) && heredocLineOperator(script.Segments[hi].After) {
		hi++
	}
	for j := lo; j <= hi; j++ {
		word, _ := segmentCommandWordPastRedirections(containerSuiteWords(script.Segments[j]))
		if shellInvokers[strings.ToLower(filepath.Base(word))] {
			return true
		}
	}
	return false
}

// heredocLineOperator reports whether op keeps two segments on one reader line.
// A ";" or a newline starts a new line, and a here-document is read by the line
// its operator stands on.
func heredocLineOperator(op string) bool {
	switch op {
	case "|", "&&", "||", "&":
		return true
	}
	return false
}

// heredocBodySubstitutions returns the command substitutions written in the
// bodies of unquoted heredocs. stripHeredocBodies drops those bodies before
// its text is scanned, yet the shell expands "$(git reset --hard)" in
// "cat > f <<EOF" while reading the body. A quoted tag keeps its body
// literal, so its substitutions stay inert (gt-x50o8).
//
// The parser does not tell a heredoc body's substitutions from a word's:
// Segment.Subs holds both, with no offset that sets them apart. So every Sub of
// a segment with an unquoted body is returned, a word's included, and that
// word's substitution is judged a second time with the verdict it already had.
// Narrowing to the body's text would be a match on raw text that can miss a
// body whose escapes the parser resolved, and a miss is an allow, so the wider
// reading stays. Substitutions nested in other substitutions are read the same
// way.
//
// The parser's Raw is the body as written, so a backquote body keeps the escapes
// the shell removes when it cuts the body (substitutionBody, gt-h3312). A body
// whose cut differs from Raw is returned both ways, so a nested command the shell
// runs (an escaped inner backquote, an escaped dollar the cut unescapes) is
// judged (gt-4wd164). The parser does not say which substitutions are backquotes,
// so a "$(...)" body with the same escapes is judged its cut too: a refusal it
// may not need, never an allow.
func heredocBodySubstitutions(command string) []string {
	script, err := shellparse.Parse(command)
	if err != nil {
		// Fail closed for every command the parser rejects, not only heredoc
		// ones, as gt-j41yim and gt-mru91l do: the whole command goes back to
		// the caller, whose walk refuses an unreadable payload before it asks
		// for substitutions, so the payload is judged once as unreadable and
		// the walk does not descend into it.
		return []string{command}
	}
	var subs []string
	var walk func(s shellparse.Script)
	walk = func(s shellparse.Script) {
		for _, seg := range s.Segments {
			unquoted := false
			for _, h := range seg.Heredocs {
				unquoted = unquoted || !h.Quoted
			}
			for _, sub := range seg.Subs {
				if unquoted && sub.Raw != "" {
					subs = append(subs, sub.Raw)
					if cut := backquoteBodyCut(sub.Raw); cut != sub.Raw {
						subs = append(subs, cut)
					}
				}
				walk(sub)
			}
		}
		for _, sub := range s.Subs {
			walk(sub)
		}
	}
	walk(script)
	return subs
}

// backquoteBodyCut is body as the shell cuts a backquote substitution's body. It
// reuses substitutionBody by wrapping body in backquotes, so the unescape is the
// one gt-h3312 specified.
func backquoteBodyCut(body string) string {
	return substitutionBody("`"+body+"`", substitutionSpan{open: 0, bodyStart: 1, bodyEnd: 1 + len(body)})
}

// skipLineContinuationBytes is skipLineContinuations for command's bytes: it
// returns the offset of the first byte at or after i that is not part of a
// backslash-newline pair. Callers reach it just past an unescaped "$", so no
// pair is escaped by the byte before it (gt-javvg).
func skipLineContinuationBytes(command string, i int) int {
	for i+1 < len(command) && command[i] == '\\' && command[i+1] == '\n' {
		i += 2
	}
	return i
}

// segmentCommandWordPastRedirections is segmentCommandWord with the leading
// redirections stepped over, so the command the shell runs behind them is
// judged: "2>/dev/null bash <<EOF" feeds bash the body exactly as "bash <<EOF"
// does (gt-pdbzu). A redirection owns its target word, so the target is stepped
// over with the operator. A process substitution word ahead of the command is
// stepped over whole: it names a file, never the command that runs (gt-8rxxy).
func segmentCommandWordPastRedirections(segment []string) (string, []string) {
	word, args, _ := segmentCommandWordPastRedirectionsKeepingLead(segment)
	return word, args
}

// segmentCommandWordPastRedirectionsKeepingLead is
// segmentCommandWordPastRedirections that also returns the redirection tokens
// it stepped over, in shell order. A caller that reads what a redirection
// carries needs them: a here-string ahead of a shell is the script it reads.
func segmentCommandWordPastRedirectionsKeepingLead(segment []string) (word string, args, lead []string) {
	for {
		word, args = segmentCommandWord(segment)
		if processSubstitutionWord(word) {
			rest, closed := skipProcessSubstitution(append([]string{word}, args...))
			if !closed {
				return "", nil, lead
			}
			segment = rest
			continue
		}
		rest := append([]string{word}, args...)
		skip := redirectionSkip(rest)
		if skip == 0 {
			return word, args, lead
		}
		lead = append(lead, rest[:skip]...)
		segment = rest[skip:]
		if len(segment) == 0 {
			return "", nil, lead
		}
	}
}

// redirectionSkip returns how many tokens at the front of segment one leading
// redirection occupies — the operator alone when it carries its target
// ("2>/dev/null"), or the operator and the next word when the target stands on
// its own ("2> /dev/null") — and 0 when the first token is not a redirection.
// A bare operator with no target left skips one token, so a caller can always
// advance by the count. Every command-word scan reads a leading redirection
// through this, so they step over the same shapes (gt-cwi1q).
func redirectionSkip(segment []string) int {
	if len(segment) == 0 {
		return 0
	}
	width := redirectionWidth(segment[0])
	if width == 2 && len(segment) < 2 {
		return 1
	}
	return width
}

// skipProcessSubstitution returns the tokens behind the "<(cmd)" or ">(cmd)"
// substitution that starts at tokens[0], and false when it never closes. The
// body may hold unquoted words, so the substitution is consumed by its
// parentheses rather than by one token (gt-8rxxy).
func skipProcessSubstitution(tokens []string) ([]string, bool) {
	depth := 0
	for i, word := range tokens {
		depth += strings.Count(word, "(") - strings.Count(word, ")")
		if depth <= 0 && strings.HasSuffix(word, ")") {
			return tokens[i+1:], true
		}
	}
	return nil, false
}

// redirectionWidth reports whether a token is a redirection and how many tokens
// it occupies: 1 when the operator carries its target ("2>/dev/null", "2>&1",
// "&>log"), 2 when the target is a word of its own ("2> /dev/null", "<<- EOF"),
// and 0 when the token is no redirection.
//
// Everything that marks a redirection apart from the file it names belongs to
// the operator, so it is matched whole: the fd digits ("2>"), an fd allocated
// by name ("{fd}>"), the "&" of "&>" and of ">&2", the doubled ">>" and "<<",
// the strip-tabs "-" of "<<-", and the clobber "|" of ">|" (gt-8rxxy).
//
// A "<(cmd)" or ">(cmd)" process substitution is a word, not a redirection,
// and redirectionWidth reports it as 0; segmentCommandWordPastRedirections
// steps over it by name.
func redirectionWidth(token string) int {
	i := 0
	if strings.HasPrefix(token, "{") {
		// "{fd}>log" allocates the descriptor by name.
		end := strings.IndexByte(token, '}')
		if end < 0 {
			return 0
		}
		i = end + 1
	} else {
		for i < len(token) && token[i] >= '0' && token[i] <= '9' {
			i++
		}
	}
	if i+1 < len(token) && token[i] == '&' && token[i+1] == '>' {
		i++
	}
	if i >= len(token) || (token[i] != '<' && token[i] != '>') {
		return 0
	}
	if i+1 < len(token) && token[i+1] == '(' {
		return 0
	}
	opStart := i
	for i < len(token) && (token[i] == '<' || token[i] == '>') {
		i++
	}
	operator := token[opStart:i]
	switch {
	case operator == "<<" && i < len(token) && token[i] == '-':
		i++
	case operator == ">" && i < len(token) && token[i] == '|':
		i++
	case i < len(token) && token[i] == '&':
		// "<&0", ">&2", ">&-": the descriptor after the "&" is the target.
		i++
	}
	if i < len(token) {
		return 1
	}
	return 2
}

// processSubstitutionWord reports whether word opens a "<(cmd)" or ">(cmd)"
// process substitution, which the shell replaces with a file name.
func processSubstitutionWord(word string) bool {
	return strings.HasPrefix(word, "<(") || strings.HasPrefix(word, ">(")
}

// heredocTerminatorEnd scans command starting at bodyStart for a line
// whose content is exactly tag (leading tabs stripped first when
// allowIndent, matching the "<<-TAG" form) and returns the offset just
// past that terminator line, or -1 when no such line exists.
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
	return -1
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

// parsedSubstitutionBodies returns the body of every substitution the shell
// expands in command as the parser reads it: "$(...)", a backquote, and the
// "<(...)" and ">(...)" process substitutions. Each body is the source text the
// shell runs (Script.Raw), so a guard recurses into it as a command of its own.
// readable is false when the parser cannot read command; the caller decides
// how to fail closed.
func parsedSubstitutionBodies(command string) (bodies []string, readable bool) {
	script, err := shellparse.Parse(command)
	if err != nil {
		return nil, false
	}
	for _, sub := range dangerousScriptSubs(script) {
		if sub.Raw != "" {
			bodies = append(bodies, sub.Raw)
		}
	}
	return bodies, true
}

// blankQuotedHeredocBodies is command with the body of every quoted heredoc
// replaced by spaces, newlines and offsets kept. A quoted tag leaves its body
// literal, so nothing in it runs and a "$(...)" there is not a substitution.
// An unquoted body is left as it is: the shell expands its substitutions while
// it reads the body (heredocBodySubstitutions), so the parser must see them. An
// unterminated heredoc has no closed body to blank, so it is left as it is too.
// The terminator line is kept, since a span ends with it and the heredoc needs
// it to close, so the rest of the command still parses.
func blankQuotedHeredocBodies(command string) string {
	b := []byte(command)
	for _, span := range heredocSpans(command) {
		if span.unterminated || !span.quoted || span.bodyEnd <= span.bodyStart {
			continue
		}
		// The span's last line is the terminator: blank up to its first byte.
		content := command[span.bodyStart:span.bodyEnd]
		terminator := strings.LastIndex(strings.TrimSuffix(content, "\n"), "\n") + 1
		for i := span.bodyStart; i < span.bodyStart+terminator; i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// mayHoldSubstitution reports whether command holds the text a command or
// process substitution opens with. It is a substring test, not a scan: it
// answers only whether a command the parser cannot read could hold a
// substitution the guard must judge. It also matches a "$(" the shell reads as
// literal text, such as one in single quotes; an unreadable line cannot tell the
// two apart, so that refusal is the fail-closed reading and is kept on purpose.
func mayHoldSubstitution(command string) bool {
	return strings.Contains(command, "`") || strings.Contains(command, "$(") ||
		strings.Contains(command, "<(") || strings.Contains(command, ">(")
}

// backslashesBefore counts the backslashes immediately preceding pos, which is
// what decides whether the character at pos is escaped: an odd run escapes it,
// an even run leaves it standing as syntax (gt-y6mhx).
func backslashesBefore(s string, pos int) int {
	n := 0
	for i := pos - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n
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
		// The naive split keeps quote characters, which hid 'sudo and x="$("
		// from the matchers (gt-h45w3). A command with a quote and a paren is
		// split by lenientShellWords, which reads the quotes; any other
		// malformed command splits as before. It cuts the separators the words
		// would otherwise fold in: a newline, ;, && or | left inside the
		// enclosing word made a later line's "rm -rf /" read as an argument of
		// the line before it (gt-w3j7z).
		if strings.ContainsAny(command, `"'`) && strings.ContainsAny(command, "()") {
			return lenientShellSplit(command, true)
		}
		// Any other malformed command splits on whitespace over the spaced
		// text, which keeps the separators spaceOutShellOperators made as
		// tokens of their own (gt-w9eli).
		return strings.Fields(spaced)
	}
	// shlex returning nothing for text that still has words in it means it
	// swallowed them (a comment it saw but this pass did not, gt-vonb1). Fail
	// toward checking: split naively so the matchers still see every word.
	if len(tokens) == 0 && strings.TrimSpace(strings.Trim(spaced, " ;")) != "" {
		return strings.Fields(spaced)
	}
	return tokens
}

// lenientShellWords splits command into words the way shlex does, but never
// fails: a quote left open runs to the end of the text, and a trailing
// backslash is kept. shlex gives up on those, and this is what lets the
// matchers see the words of `echo "$(" bash -c 'sudo x'` (gt-h45w3). A
// backslash escapes the next rune outside quotes, and inside double quotes only
// before ", \, $ and a backtick, as the shell reads it. A word of empty quotes
// stays as an empty word, as shlex returns it.
func lenientShellWords(command string) []string {
	return lenientShellSplit(command, false)
}

// lenientShellSplit is the walk behind lenientShellWords. With separators
// set, it also cuts at an unquoted ";", "&", "|" or newline and emits the
// operator as a token of its own — a newline as ";" and a doubled "&&" or
// "||" as one token, an "&" touching a ">" staying in its redirection — the
// tokens spaceOutShellOperators gets shlex to produce. That is the only
// separator reading a malformed command has: the spaced text shlex failed on
// escapes its substitutions' quoting for shlex, which reads back as one long
// quoted word, so a later line's command would ride into the line before it
// (gt-w3j7z).
func lenientShellSplit(command string, separators bool) []string {
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\\' && i+1 < len(runes) && (quote == 0 || strings.ContainsRune("\"\\$`", runes[i+1])):
			i++
			word.WriteRune(runes[i])
			inWord = true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inWord = true
		case separators && (r == ';' || r == '&' || r == '|' || r == '\n'):
			if r == '&' && ((i > 0 && runes[i-1] == '>') || (i+1 < len(runes) && runes[i+1] == '>')) {
				// A '&' touching a '>' is a redirection (2>&1, &>file),
				// not a background operator (gt-wwwht).
				word.WriteRune(r)
				inWord = true
				continue
			}
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
			switch {
			case r == '\n':
				words = append(words, ";")
			case (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r:
				words = append(words, string([]rune{r, r}))
				i++
			default:
				words = append(words, string(r))
			}
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
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
// matchers that need the body get it from the parsed command instead
// (parsedSubstitutionBodies).
//
// An unquoted subshell — `( list )` — has its parentheses spaced out too, so
// each is a token of its own. A "(" is spaced where it opens a shell word (so
// a function definition's `f()` and a quoted paren are left alone) and the
// ")" closing the group it opened is spaced to match, which is what tells a
// group terminator from the ")" of a `case` pattern. Glued, "(git" and
// "main)" hid a subshell's command word and its last word from the
// dangerous-command matchers, which find their command by name and their
// arguments per shell segment (gt-5eniu).
func spaceOutShellOperators(command string) string {
	var b strings.Builder
	b.Grow(len(command) + 8)
	scopes := []shellQuoteScope{{}}
	escaped := false
	// groupDepth counts the unquoted subshell groups — `( list )` — open at
	// this level, so a `)` can be told from a `case` pattern's (`a)`), which
	// opens no group and must stay glued to its pattern word (gt-5eniu).
	groupDepth := 0
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
		case sc.quote == '$':
			// Inside $'...' a backslash escapes the next rune, including the
			// quote, so \' does not end the string. shlex has no such quote,
			// so the string is re-emitted double-quoted with the escapes
			// shlex does understand: a quote hidden in it can no longer swallow
			// the commands behind it (deep-review gt-dilqk).
			switch {
			case r == '\\' && i+1 < len(runes):
				next := runes[i+1]
				i++
				switch next {
				case '\'':
					b.WriteRune('\'')
				case '"':
					b.WriteString(`\"`)
				case '\\':
					b.WriteString(`\\`)
				default:
					b.WriteString(`\\`)
					b.WriteRune(next)
				}
			case r == '\\':
				b.WriteString(`\\`)
			case r == '\'':
				b.WriteRune('"')
				scopes[len(scopes)-1].quote = 0
			case r == '"':
				b.WriteString(`\"`)
			default:
				b.WriteRune(r)
			}
		case sc.quote == '\'':
			if r == '\'' {
				scopes[len(scopes)-1].quote = 0
			}
			emit(r)
		case sc.quote == 0 && !sc.escapeForShlex && isANSICDollar(runes, i):
			b.WriteRune('"')
			scopes[len(scopes)-1].quote = '$'
			// Land on the quote itself; the loop step moves past it (gt-javvg).
			i = skipLineContinuations(runes, i+1)
		case r == '$' && sc.quote == '"' && i+2 < len(runes) && quotedDollarParen(r, runes[i+1], runes[i+2]) && !quotedSubstitutionCloses(command, runes, i):
			// A quoted dollar-paren — "$(" — whose substitution never closes
			// ends the double-quoted word at the quote after the "(", so the
			// ")" around it stays in view and the command glued inside is
			// judged (gt-qg832). A substitution that does close is a quoting
			// scope of its own, taken by the "$(" branch below; reading this
			// one flat turned a quoted argument's ";" into a separator
			// (gt-l18zp).
			scopes[len(scopes)-1].quotedParen = true
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
			closing := scopes[len(scopes)-1].depth == 0
			if closing && scopes[len(scopes)-1].quotedParen {
				// The word before the ")" that closes the substitution holding
				// a quoted dollar-paren is an argument of its own: glued,
				// "rm -rf /)" ends in "/)" and the root target is no token
				// any matcher compares (gt-qg832). Only the substitution's OWN
				// ")" is padded — one nested inside it closes a group, and is
				// emitted as in any other substitution body.
				b.WriteString(" ) ")
			} else {
				emit(r)
			}
			if closing {
				closeScope()
			}
		case r == '(' && plainGroupScope(sc) && startsShellWord(runes, prevIdx, prevEscaped):
			// An unquoted "(" that opens a shell word begins a SUBSHELL group,
			// which the shell runs as commands the way a brace group is. Emitted
			// spaced so it is a token of its own and the command behind it
			// stands in command position: glued, "(git" is one token, and every
			// matcher that finds its command word by name (git push -f, rm -rf
			// /, the polecat main-push) reads straight past it (gt-5eniu).
			b.WriteString(" ( ")
			groupDepth++
		case r == ')' && plainGroupScope(sc) && groupDepth > 0:
			// The ")" that closes a group opened above is spaced the same way,
			// so the group's last word is not glued to it and hidden from the
			// matchers — "main)" hid a push destination, "-f)" a force flag
			// (gt-5eniu). Only a PAIRED ")" is touched, so the ")" of a `case`
			// pattern ("a)" in `case x in a) ...`) opens no group and stays
			// with its pattern word, which trimShellKeywords reads.
			b.WriteString(" ) ")
			groupDepth--
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
			if (r == '&' && ((i > 0 && runes[i-1] == '>') || (i+1 < len(runes) && runes[i+1] == '>'))) ||
				(r == '|' && prevIdx >= 0 && runes[prevIdx] == '>' && !prevEscaped) {
				// A '&' touching a '>' on either side is a redirection
				// (2>&1, &>file), not a background operator. A '|' right
				// after a '>' is the clobber redirect (">|file", "2>|file",
				// "{fd}>file"). Left glued each stays one token, so the
				// operator and its target reach redirectionWidth whole and a
				// command behind them is still found (gt-wwwht, gt-l18zp).
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

// plainGroupScope reports whether sc is the plain command-line quoting scope —
// unquoted, not a command substitution or backtick body, and not a scope
// opened inside a double-quoted word (escapeForShlex). A subshell's parentheses
// are grouping only there: inside `$( ... )` or a backtick body they nest in
// the substitution, and inside such a substitution's own quoted stretch they
// are literal text (see spaceOutShellOperators, gt-n8ir, gt-5eniu).
func plainGroupScope(sc shellQuoteScope) bool {
	return sc.quote == 0 && !sc.subst && !sc.tick && !sc.escapeForShlex
}

// quotedDollarParen reports whether the three characters spell the quoted
// dollar-paren shape "$(" — a substitution opener whose body begins with the
// double quote the shell ends the surrounding double-quoted word at. The quote
// walk in spaceOutShellOperators and the span scan in closingDoubleQuote judge
// the shape here so the two agree on where that word ends (gt-qg832).
func quotedDollarParen(dollar, paren, quote rune) bool {
	return dollar == '$' && paren == '(' && quote == '"'
}

// quotedSubstitutionCloses reports whether the substitution whose "$" stands
// at rune i of runes closes within command. closingDoubleQuote skips a
// substitution that closes whole; spaceOutShellOperators reads one flat only
// when it does not, so the two agree on where the quoted word ends (gt-l18zp).
// Kept on the text answer: spaceOutShellOperators is shared by seven guards, and
// retiring it is planned in gt-crn9gu.
func quotedSubstitutionCloses(command string, runes []rune, i int) bool {
	from := len(string(runes[:i+2])) // the byte just past the "("
	return closingParen(command, from, heredocBodiesFrom(command, from)) >= 0
}

// isANSICDollar reports whether the "$" at runes[i] opens a $'...' string. The
// quote walk and spaceOutShellOperators judge the opener here, through
// ansiCDollarQuote, so they agree on where a $'...' string starts (gt-0vbb2,
// gt-h4go2). heredocLineEnd reads bytes forward and skips the second "$" of a
// "$$" in place for the same reason.
func isANSICDollar(runes []rune, i int) bool {
	return ansiCDollarQuote(len(runes), runesAt(runes), i) >= 0
}

// ansiCDollarQuote returns the index of the "'" that the "$" at dollar opens a
// $'...' string with, or -1, over the n characters at reads. The quote after a
// "$" is an ANSI-C one only when the "$" is one the shell reads as a "$" of its
// own. A backslash-escaped "$" is literal text, and the second "$" of "$$" is
// the tail of the PID expansion, so "$$'" holds a plain quote after the PID
// (gt-0vbb2). A backslash-newline between the "$" and the quote is deleted by
// the shell, so the quote is still the one that follows it (gt-javvg).
func ansiCDollarQuote(n int, at func(int) rune, dollar int) int {
	if dollar < 0 || dollar >= n || at(dollar) != '$' || escapedAt(at, dollar) || pidTailAt(at, dollar) {
		return -1
	}
	// A backslash after an unescaped "$" or a deleted pair is never itself
	// escaped, so the pairs are stepped over without that check.
	q := dollar + 1
	for q+1 < n && at(q) == '\\' && at(q+1) == '\n' {
		q += 2
	}
	if q < n && at(q) == '\'' {
		return q
	}
	return -1
}

// runesAt is the character reader ansiCDollarQuote takes, over runes.
func runesAt(runes []rune) func(int) rune {
	return func(i int) rune { return runes[i] }
}

// skipLineContinuations returns the index of the first rune at or after i that
// is not part of a backslash-newline pair, which the shell deletes before it
// reads the words around it (gt-javvg).
func skipLineContinuations(runes []rune, i int) int {
	for i+1 < len(runes) && runes[i] == '\\' && runes[i+1] == '\n' && !escapedRune(runes, i) {
		i += 2
	}
	return i
}

// lineContinuationsBefore is the backward form of skipLineContinuations: it
// returns the index just past the last rune before i that is not part of a
// backslash-newline pair, so runes[result-1] is the rune the shell reads before
// runes[i] (gt-javvg).
func lineContinuationsBefore(runes []rune, i int) int {
	for i >= 2 && runes[i-1] == '\n' && runes[i-2] == '\\' && !escapedRune(runes, i-2) {
		i -= 2
	}
	return i
}

// pidTailAt reports whether the "$" at at(i) is the second half of a "$$" the
// shell reads as the PID: the "$" before it is unescaped and begins that pair
// itself, so a third "$" after "$$" starts an expansion of its own.
func pidTailAt(at func(int) rune, i int) bool {
	if i < 1 || at(i) != '$' {
		return false
	}
	start := i
	for start > 0 && at(start-1) == '$' {
		start--
	}
	// The first "$" of the run is never a tail. The escape on it decides the
	// second, and each "$" after that alternates with the one before it.
	tail := false
	for j := start + 1; j <= i; j++ {
		tail = !tail && (j > start+1 || !escapedAt(at, start))
	}
	return tail
}

// escapedRune reports whether the rune at runes[i] is backslash-escaped: an odd
// run of backslashes ends right before it.
func escapedRune(runes []rune, i int) bool {
	return escapedAt(runesAt(runes), i)
}

// escapedAt is escapedRune over the characters at reads.
func escapedAt(at func(int) rune, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && at(j) == '\\'; j-- {
		n++
	}
	return n%2 == 1
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

// commandReservedWords are the reserved words after which the shell reads a
// command, so a "((" glued to one opens an arithmetic command. "time" takes a
// pipeline, so it stands here with the words that take a command list.
var commandReservedWords = map[string]bool{
	"then": true, "do": true, "else": true, "elif": true,
	"if": true, "while": true, "until": true, "for": true,
	"time": true,
}

// endsCommandReservedWord reports whether the "(" after runes[prevIdx] stands
// where the shell reads a command, because what ends there delimits at the "("
// (gt-he2fi, gt-4jcks): one of commandReservedWords, or a "!" or "{", standing
// as a word of its own, or a ")" that closes a case pattern or a function's
// "()". A word the shell read as part of a longer one (a "dothen" or an
// escaped character) is not reserved. In a command bash accepts, a ")" glued
// to "((" closes only one of those two (a function body may be an arithmetic
// command), so the ")" needs no check of which construct it closes.
func endsCommandReservedWord(runes []rune, prevIdx int, prevEscaped bool) bool {
	if prevEscaped || prevIdx < 0 {
		return false
	}
	if runes[prevIdx] == ')' {
		return true
	}
	start := prevIdx
	if r := runes[prevIdx]; r != '!' && r != '{' {
		for start > 0 && runes[start-1] >= 'a' && runes[start-1] <= 'z' {
			start--
		}
		if !commandReservedWords[string(runes[start:prevIdx+1])] {
			return false
		}
	}
	return start == 0 || (runes[start-1] != '\\' && startsShellWord(runes, start-1, false))
}

// shellQuoteScope is one quoting scope in spaceOutShellOperators: the
// top-level command line, or the body of a command substitution, which the
// shell re-parses as a command with quoting of its own (gt-n8ir).
type shellQuoteScope struct {
	quote rune // the quote open in this scope: 0, '\'', '"' or '$' (an ANSI-C $'...' string)
	subst bool // opened by "$(" — closed by the ")" matching its own depth
	depth int  // unmatched "(" seen inside a subst body
	tick  bool // opened by a backtick — closed by the next backtick
	// escapeForShlex backslashes every '"' and '\' this scope emits, so that
	// a substitution opened inside a double-quoted word stays the one opaque
	// token those quotes promise when shlex — which has no notion of scope
	// nesting — splits the result (gt-n8ir).
	escapeForShlex bool
	// quotedParen records a quoted dollar-paren — "$(" — read inside this
	// substitution, which ends the double-quoted word it sits in. The ")"
	// that closes this substitution — and only it, not a ")" nested inside —
	// is then padded out, so a word glued to it ("/" in "rm -rf /)") is still
	// an argument of its own (gt-qg832).
	quotedParen bool
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
	// subshells counts the "(" groups open at i. A subshell runs its commands
	// in a child process, so no cd inside one is this shell's and the walk
	// reads none of them: `cd A ; (cd B) ; make test` runs make in A. Only a
	// "(" that opens a command is counted, so a quoted "(" used as an argument
	// is not mistaken for one (gt-5eniu).
	subshells := 0
	for i := 0; i < end; i++ {
		if subshells > 0 {
			// A "(" that opens a command nests one level deeper, so the ")"
			// closing it leaves the walk inside the outer group: `cd A && (true;
			// (true); cd B;) ; make test` runs make in A (gt-1daus).
			if tokens[i] == "(" && shellCommandStart(tokens, i) {
				subshells++
			} else if tokens[i] == ")" {
				subshells--
			}
			continue
		}
		if tokens[i] == "(" && shellCommandStart(tokens, i) {
			// A group still open at end holds the command the walk is placing,
			// so its cds are that command's: `(cd A && make test)` runs make
			// in A (gt-y1zks).
			if !subshellClosesBefore(tokens, i, end) {
				continue
			}
			subshells++
			continue
		}
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

// subshellClosesBefore reports whether the "(" at open has its ")" before end.
// Groups opened inside it are matched first, so the ")" of an inner one is not
// taken for the outer's.
func subshellClosesBefore(tokens []string, open, end int) bool {
	depth := 0
	for i := open + 1; i < end; i++ {
		switch {
		case tokens[i] == "(" && shellCommandStart(tokens, i):
			depth++
		case tokens[i] == ")":
			if depth == 0 {
				return true
			}
			depth--
		}
	}
	return false
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
// subshell's change is not the shell's, a brace group's is, so cdWalkRoot
// skips the subshell's contents — but both hold a command list, so the word
// after either opens one (gt-ajyw8, gt-5eniu).
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
// basename, skipping env assignments, launchers and the redirections ahead of
// it (segmentCommandWordPastRedirections) — is the package manager, and
// "install" is one of that invocation's arguments, in any position so an
// option before the subcommand ("apt-get -y install curl") is still blocked
// (gt-qis3f), and "2>/dev/null apt-get install curl" is read as the install it
// is (gt-8rxxy). tokens must be lowercased, shell-aware tokens (see
// shellTokenize).
func matchesPackageInstall(tokens []string) string {
	for _, segment := range splitShellSegments(tokens) {
		word, args := segmentCommandWordPastRedirections(segment)
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
// for itself is a user override, not a gate - only the landing worker's merge and
// `gt done`'s own direct merge, which gt itself sets, are gates. So the
// allow path for both is a crew session.
const (
	polecatMainPushReason      = "Polecats never push to main/master (use gt done)"
	polecatMainPushAlternative = "Alternative: `gt done` pushes your polecat/<name>/<bead> branch and the landing worker " +
		"merges it to the default branch after verification — a direct push to main skips " +
		"the MR, the landing worker's gate run, and the om review (gt-ibt8). A release, or a plugin " +
		"script's own push, runs from a crew session instead (gt-deff)."
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
	// git resolves "heads/main" and "refs/heads/main" to the same branch.
	dst = strings.TrimPrefix(strings.TrimPrefix(dst, "refs/"), "heads/")
	return polecatMainPushBranches[dst]
}

// gitResetRemotePrefixes are token prefixes that name a remote-tracking ref:
// "origin/main", "upstream/main", and the long form of either.
var gitResetRemotePrefixes = []string{"origin/", "upstream/", "refs/remotes/"}

// gitResetNamesRemote reports whether one lowercased `git reset` argument names
// a remote-tracking commit: a remote-tracking ref, a bare remote name (which
// git reads as <remote>/HEAD), FETCH_HEAD, which a fetch points at the remote's
// tip, or an upstream-of shorthand (@{u}, @{upstream}, @{push}, alone or after
// a branch name). A trailing revision operator (~, ~N, ^, ^N or a ^{...} peel,
// in any run) does not change which revision the base names, so it is stripped
// for this check only: "fetch_head~1", "@{u}^" and "@{u}^{}" all reset onto the
// remote's history.
func gitResetNamesRemote(arg string) bool {
	arg = trimRevisionSuffix(arg)
	for _, prefix := range gitResetRemotePrefixes {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	switch arg {
	case "origin", "upstream", "fetch_head":
		return true
	}
	for _, suffix := range []string{"@{u}", "@{upstream}", "@{push}"} {
		if strings.HasSuffix(arg, suffix) {
			return true
		}
	}
	return false
}

// trimRevisionSuffix drops a trailing run of git revision operators from a
// revision: the ancestry operators "~", "~N", "^" and "^N", repeated
// ("fetch_head~2^", "@{u}^^"), and the peel operator "^{...}" in both of its
// spellings — "^{}" (peel to the commit) and "^{<type>}" ("@{u}^{commit}",
// "origin/main^{tree}"), in any order relative to an ancestry operator
// ("FETCH_HEAD~1^{commit}", "@{u}^{commit}~2"). None of them changes which
// revision the base names, so a guard that reads the base must read through
// them all. Only digits after an ancestry operator and the braced remainder of
// a "^" are consumed, so a revision with no operator keeps its trailing digits
// ("release1") and a name whose own syntax ends in a brace ("@{u}", the reflog
// entry "main@{1}") is left as it is.
func trimRevisionSuffix(rev string) string {
	for {
		i := len(rev)
		for i > 0 && rev[i-1] >= '0' && rev[i-1] <= '9' {
			i--
		}
		if i > 0 && (rev[i-1] == '~' || rev[i-1] == '^') {
			rev = rev[:i-1]
			continue
		}
		if i > 0 && rev[i-1] == '}' {
			if open := strings.LastIndexByte(rev[:i-1], '{'); open > 0 && rev[open-1] == '^' {
				rev = rev[:open-1]
				continue
			}
		}
		return rev
	}
}

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
// Only a reset whose TARGET is a remote-tracking ref — spelled out, or hidden
// behind a shell expansion (gitResetTargetIsExpansion) — is blocked, so the
// ordinary `git reset --soft HEAD~1` squash, and every reset to a literal local
// ref or pathspec, still works. tokens must be lowercased, shell-aware tokens
// (see shellTokenize); the "git reset" adjacency check matches
// matchesDangerousGitPush, which treats `git -C <dir> push --force` as out of
// scope for the same reason: the guard scans argv shapes, and the branch
// content check in gt done is what actually fails closed.
func matchesDangerousGitReset(tokens []string) (reason, alternative string) {
	for _, args := range gitSubcommandArgs(tokens, "reset") {
		for _, f := range args {
			// A bare "--" ends the revisions: everything after it is a pathspec,
			// so "git reset -- origin/main" is unstaging a path that happens to be
			// spelled like a ref, not resetting onto one.
			if f == "--" {
				break
			}
			if gitResetNamesRemote(f) {
				return "Reset onto a remote-tracking ref drops merged work",
					"Alternative: `git rebase " + f + "` — rebase your CHANGES onto the remote ref; never reset your tree onto it."
			}
		}
		if target, ok := gitResetTarget(args); ok && gitResetTargetIsExpansion(target) {
			return "Reset onto a shell expansion hides the ref it resets to",
				"Alternative: name the revision literally and `git rebase <ref>` it — a substitution or variable target is what hides a reset onto the remote."
		}
	}
	return "", ""
}

// gitResetTarget returns the revision a `git reset` invocation names — the
// first argument after the subcommand that is not a flag — and whether the
// invocation names one at all. Git reads its mode and -q flags before the
// revision, so the first non-flag word is the <tree-ish>; a word after that is
// a pathspec, which only does anything when a revision is present. A reset
// with no non-flag word ("git reset", "git reset --hard") names no revision
// and resets to HEAD.
func gitResetTarget(args []string) (string, bool) {
	for _, f := range args {
		// A bare "--" ends the revisions, so a target written after it is a
		// pathspec and this invocation names none (gt-63sz).
		if f == "--" {
			return "", false
		}
		if strings.HasPrefix(f, "-") {
			continue
		}
		return f, true
	}
	return "", false
}

// gitResetTargetIsExpansion reports whether a `git reset` target is a shell
// expansion — a command substitution ("$(git rev-parse origin/main)", a
// backtick spelling) or a variable expansion ("$BASE", "${BASE}", "$1") —
// rather than the literal revision the text spells.
//
// The reset gt-63sz blocks is otherwise reachable without ever writing a
// remote-tracking ref where the matcher can read one:
// `git reset --soft "$(git rev-parse origin/main)"`, `git reset --soft $BASE`
// and `BASE=$(git rev-parse origin/main); git reset --soft $BASE` all reset
// onto the remote's tip, and all three were allowed (gt-tao2j). The
// nested-command walk does not close this: it runs the substitution as a
// command of its own, and `git rev-parse origin/main` is not a reset.
//
// Any "$" or backtick in the target counts, because the guard cannot tell what
// the shell resolves it to. Quoting is no help either way: "$BASE" and "$(…)"
// reach git expanded inside double quotes, while a single-quoted "$BASE"
// reaches it as a word no legitimate ref matches. Only the first non-flag
// argument is judged — a pathspec after it is not a reset target, and the
// remote-prefix scan above already covers a target that spells the ref out.
func gitResetTargetIsExpansion(target string) bool {
	return strings.ContainsAny(target, "$`")
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
	"cache":     true,
	"testcache": true,
	"modcache":  true,
	"fuzzcache": true,
}

const goCleanSharedCacheReason = "'go clean' with -cache/-testcache/-modcache/-fuzzcache wipes the Go build cache shared by every agent on this host"
const goCleanSharedCacheAlternative = "Alternative: use 'go test -count=1' for a cold run, or rebuild a single package; " +
	"if you believe the cache is corrupt, escalate with the evidence instead of clearing it."

// matchesGoCleanSharedCache blocks 'go clean' invocations carrying any of
// the shared-cache flags (goCleanSharedCacheFlags), in any order and
// combined with other clean flags (e.g. 'go clean -x -cache'). The go binary
// may be path-qualified (/usr/local/go/bin/go), and Go's flag package accepts
// one or two leading dashes and a =value suffix, so --modcache and
// -modcache=true wipe the cache too (gt-5jir5). tokens must
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
		if f == "clean" && goSubcommandBefore(tokens, i) {
			inClean = true
			continue
		}
		if !inClean {
			continue
		}
		if isGoCleanSharedCacheFlag(f) {
			return goCleanSharedCacheReason, goCleanSharedCacheAlternative
		}
	}
	return "", ""
}

// goBinaryBefore returns the index of the `go` binary whose subcommand word
// tokens[i] is — the relationship a 'go SUBCOMMAND' matcher needs, since Go's
// global -C dir flag (Go >= 1.20) may sit between the binary and the
// subcommand. Without the walk, `go -C /tmp clean -modcache` reaches `clean`
// with the directory before it, not `go`, and the invocation read as a plain
// `clean` (gt-t7p80). Naming the invocation back to the operator needs the
// binary's own position, so the index is returned rather than a yes/no.
//
// The walk back from tokens[i] consumes exactly what -C costs: the flag in
// either spelling, "-C dir" and "-C=dir", repeated for each -C on the line.
// Any other word ends the walk, so a `clean` that no `go` invocation owns is
// still not read as one of its subcommands. The binary is matched by
// isGoBinaryName, so a versioned toolchain (go1.22.0, gotip) owns its
// subcommands too. tokens must be lowercased, shell-aware tokens (see
// shellTokenize), which is why -C arrives as -c.
//
// What -C consumed is tested for being the binary only after it is known not
// to be a -C argument: a directory named `go` — `go -C /tmp/go test ./...` —
// has the binary's own basename, so testing the word first named the directory
// as the binary and the held invocation was spelled back as "/tmp/go test
// ./...", which is no command at all (gt-7cmjp).
func goBinaryBefore(tokens []string, i int) int {
	for j := i - 1; j >= 0; j-- {
		if isGoDirFlagValue(tokens[j]) {
			continue
		}
		if j > 0 && isGoDirFlag(tokens[j-1]) {
			j-- // tokens[j] is the directory a bare `-C` consumed
			continue
		}
		if isGoBinaryName(filepath.Base(tokens[j])) {
			return j
		}
		return -1
	}
	return -1
}

// goSubcommandBefore reports whether tokens[i] is the subcommand word of a
// `go` invocation that starts earlier in the token list (goBinaryBefore).
func goSubcommandBefore(tokens []string, i int) bool {
	return goBinaryBefore(tokens, i) >= 0
}

// findGoSubcommand returns the index of the subcommand word of the first
// `go SUB` invocation in a shell segment (tokens already lowercased), or -1.
// It is findInvocation's go case widened by the -C dir flag that may sit
// between the binary and the subcommand: findInvocation requires the
// subcommand to follow the binary directly, so a whole-module
// `go -C /tmp test ./...` reached it as no invocation at all and the
// full-suite HOLD never fired (gt-soohm).
func findGoSubcommand(tokens []string, sub string) int {
	for i, tok := range tokens {
		if tok == sub && goSubcommandBefore(tokens, i) {
			return i
		}
	}
	return -1
}

// goBinaryNamePattern matches every name the Go toolchain ships under: the
// plain `go` binary, a versioned release (go1.22.0, go1.21 — the names the
// download page and Homebrew install), a pre-release (go1.22rc1, go1.22beta1,
// go1.22alpha1 — the names golang.org/dl installs, whose suffix the release
// form alone did not cover), and the development toolchain `gotip`. tokens are
// lowercased, so the name arrives in lower case.
var goBinaryNamePattern = regexp.MustCompile(`^go(tip|\d+(\.\d+)*((alpha|beta|rc)\d+)?)?$`)

// isGoBinaryName reports whether name is a Go toolchain binary. A `clean` run
// through any of them wipes the same shared cache, so a versioned binary — a
// pre-release no less than a release, and the same name golang.org/dl
// installs — is not a reason to miss the subcommand (gt-y6mhx, gt-7cmjp).
func isGoBinaryName(name string) bool {
	return goBinaryNamePattern.MatchString(name)
}

// isGoDirFlag reports whether tok is go's -C dir flag spelled without a value
// ("-C", "--C"), the form whose directory is the NEXT token. The name is
// matched exactly, so `-c=` and a flag that merely starts with those letters
// are not read as it.
func isGoDirFlag(tok string) bool {
	return tok == "-c" || tok == "--c"
}

// isGoDirFlagValue reports whether tok carries go's -C dir flag and its
// directory in one word ("-C=/tmp", "--C=/tmp"), the spelling Go's flag
// package accepts alongside the bare form.
func isGoDirFlagValue(tok string) bool {
	name, _, hasValue := strings.Cut(tok, "=")
	return hasValue && isGoDirFlag(name)
}

// isGoCleanSharedCacheFlag reports whether tok is one of the shared-cache
// flags in any spelling Go's flag package accepts: -cache, --cache or
// -cache=true. A token without a leading dash is an argument, not a flag.
func isGoCleanSharedCacheFlag(tok string) bool {
	if !strings.HasPrefix(tok, "-") {
		return false
	}
	name := strings.TrimLeft(tok, "-")
	if eq := strings.IndexByte(name, '='); eq >= 0 {
		name = name[:eq]
	}
	return goCleanSharedCacheFlags[name]
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
// The invocation is located by its subcommand (goSubcommandBefore), not by a
// 'go' token abutting 'test', so the -C dir flag between the two hides
// nothing (gt-soohm).
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
	for i := range tokens {
		if tokens[i] == "test" && goSubcommandBefore(tokens, i) {
			if fullSuiteUncachedGoTest(tokens[i+1:]) {
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
			if isGoTestValueFlag(flag) && !hasValue && i+1 < len(rest) {
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

// idleGateCPUIdleThreshold is the percent of the host's CPU that must be
// idle for a full-suite start to be permitted; below it the start is HELD
// (exit 2, retryable). It is the barrier the rig's Host Hygiene directive
// sets for running a full suite on the operator's shared host, and the
// guard's job is to hold what that rule holds (gt-6rieh).
const idleGateCPUIdleThreshold = 25

// idleGateAlternative is the HOLD banner's suggested next step. The first
// sentence is true of every held command and is fixed; the second names the
// command that was actually held (heldAction), because advice to run a
// different target — a fixed "use GOFLAGS=-p=8 make test" — cost a polecat
// held on 'make build' a second full-module run instead of the one it wanted
// (gt-p7g2m). GOFLAGS=-p=8 caps go's build parallelism for either target.
func idleGateAlternative(heldAction string) string {
	return fmt.Sprintf("Wait 2 minutes and re-run this exact command; it will pass once the CPU is at least %d%% idle. ", idleGateCPUIdleThreshold) +
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
// its flags were written) or the whole-repo go form as written ('go test
// ./...', 'go -C /tmp test ./...'; see goWholeRepoInvocation). "" means the
// segment is not gated. tokens is original-case; matching is done on a
// lowercased copy so "Make Test" and "make test" are treated the same. cwd
// and cwdKnown locate a make invocation's tree, through makeTreeDir and
// makeActsOnWholeGoModule.
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
	if i := findGoSubcommand(lower, "test"); i >= 0 && wholeRepoArgFollows(tokens, i+1) {
		return goWholeRepoInvocation(tokens, lower, i)
	}
	if i := findGoSubcommand(lower, "build"); i >= 0 && wholeRepoArgFollows(tokens, i+1) {
		return goWholeRepoInvocation(tokens, lower, i)
	}
	return ""
}

// goWholeRepoInvocation spells a held whole-repo go invocation back to the
// operator as it was written, from the binary through the whole-repo argument
// at sub+1 — "go test ./..." for the plain form, and "go -C /tmp test ./..."
// when the -C dir flag sits between the two, so the advice line names the tree
// the command actually runs in (gt-soohm). tokens is original-case (what the
// operator typed); lower is its lowercased twin, whose indices are the same
// and which located the subcommand at sub.
func goWholeRepoInvocation(tokens, lower []string, sub int) string {
	return strings.Join(tokens[goBinaryBefore(lower, sub):sub+2], " ")
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

// actualHostCPUIdle returns the percent of the host's CPU that is idle
// (guardProcess.cpuIdle in production), from internal/hoststat's sample of
// the host. ok is false when the sample could not take the reading, which
// the gate reads as no evidence of a busy host. Only a command the gate
// matched reaches this sample, so a gated suite start pays for it and no
// other command does (gt-6rieh).
func actualHostCPUIdle() (cpuIdle float64, ok bool) {
	idle := hoststat.Sample(context.Background()).CPUIdle
	if idle == nil {
		return 0, false
	}
	return *idle, true
}

// printIdleGateHold prints the HOLD banner to stderr — distinct from
// printDangerousBlock's BLOCKED banner since this command is expected to
// succeed on retry, not to be avoided entirely. heldAction is the gated
// invocation evaluateIdleGate matched, named in the banner's advice line so
// the suggestion matches the command it held (gt-p7g2m).
func printIdleGateHold(w io.Writer, cpuIdle float64, command, heldAction string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(w, "║  ⏸  SUITE START HELD (host busy)                                 ║")
	fmt.Fprintln(w, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(w, "║  Command:   %-51s ║\n", truncateStr(command, 51))
	fmt.Fprintf(w, "║  CPU idle:  %-51s ║\n", fmt.Sprintf("%.1f%% (need >= %d%%)", cpuIdle, idleGateCPUIdleThreshold))
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
