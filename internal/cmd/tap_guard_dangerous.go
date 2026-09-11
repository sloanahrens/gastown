package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
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
  - git reset --hard
  - git clean -f / git clean -fd
  - drop table/database
  - truncate table
  - find/bfs/fd/rg/grep -r/du/ls -R rooted at /, ~, $HOME, /Users, /System,
    /Library, or /opt (see gt-nqcy — an unbounded 'bfs /' froze a host)

The guard reads the tool input from stdin (Claude Code hook protocol)
and exits with code 2 to block dangerous operations.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
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
	{[]string{"git", "clean", "-f"}, "git clean -f deletes untracked files irreversibly"},
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

	if reason, alternative := evaluateDangerousCommand(command, 0); reason != "" {
		if alternative != "" {
			printDangerousBlockWithAlternative(reason, command, alternative)
		} else {
			printDangerousBlock(reason, command)
		}
		return NewSilentExit(2)
	}

	return nil
}

// maxDangerousNestDepth bounds nestedCommands recursion so a pathological
// input (e.g. deeply chained "eval eval eval ...") can't loop unboundedly.
const maxDangerousNestDepth = 3

// evaluateDangerousCommand runs every dangerous-pattern check against
// command and, up to maxDangerousNestDepth, recurses into any shell command
// it finds embedded as an argument (bash -c/sh -c/eval) or as a command
// substitution ($(...) / `...`). Without this, a wrapper like
// `bash -c "git reset --hard"` was invisible to every check: shlex
// collapses the quoted payload into a single token, so none of the
// fragment-based matchers (which require each fragment as its own token)
// ever fire on it. Quoted text that is NOT one of these shell-executing
// forms (a SQL string, a mail body, a jq/sed script) deliberately stays
// opaque — that is where this guard's real false positives have come from
// (mayor scope, gt-5ihs attempt 2, gt-wisp-db27 finding 4).
func evaluateDangerousCommand(command string, depth int) (reason, alternative string) {
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
	// Unbounded scans need the original-case tokens: ls -R (recursive) and
	// ls -r (reverse sort) mean different things, and lowercasing would
	// collapse that distinction.
	if r, alt := matchesUnboundedScan(tokens); r != "" {
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
	for _, n := range nested {
		if r, alt := evaluateDangerousCommand(n, depth+1); r != "" {
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

// stripHeredocBodies removes heredoc body text from command before any
// tokenization or pattern matching runs. A heredoc body is DATA being
// written to a file or piped to a command's stdin — the same class of
// risk as a quoted SQL string or a mail body, not shell syntax to
// evaluate — so scanning it for dangerous fragments produces false
// positives: "cat > note.md <<'EOF' ... git push --force ... EOF" blocked
// an ordinary file write because the DATA it wrote happened to mention a
// dangerous phrase (gt-mkrj). Strips from just after the "<<TAG" line
// through the line that is exactly TAG (optionally tab-indented, for the
// "<<-TAG" form); text outside any heredoc span is left untouched.
func stripHeredocBodies(command string) string {
	matches := heredocStartPattern.FindAllStringSubmatchIndex(command, -1)
	if matches == nil {
		return command
	}
	var b strings.Builder
	pos := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start < pos {
			continue // inside a previously-stripped heredoc body
		}
		allowIndent := command[m[2]:m[3]] == "-"
		tag := command[m[6]:m[7]]

		nl := strings.IndexByte(command[end:], '\n')
		if nl < 0 {
			// No body follows on a later line (e.g. the heredoc marker is
			// the last thing on the line with nothing after it) — nothing
			// to strip.
			b.WriteString(command[pos:end])
			pos = end
			continue
		}
		bodyStart := end + nl + 1
		b.WriteString(command[pos:bodyStart])
		pos = heredocTerminatorEnd(command, bodyStart, tag, allowIndent)
	}
	b.WriteString(command[pos:])
	return b.String()
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
func spaceOutShellOperators(command string) string {
	var b strings.Builder
	b.Grow(len(command) + 8)
	var quote rune
	escaped := false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
			} else if r == quote {
				quote = 0
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case '\\':
			escaped = true
			b.WriteRune(r)
		case '\'', '"':
			quote = r
			b.WriteRune(r)
		case ';', '&', '|':
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
			b.WriteRune(r)
		}
	}
	return b.String()
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

// matchesUnboundedScan blocks find/bfs/fd/rg/ag/du, "grep -r", and "ls -R"
// invocations whose root argument is broad enough to scan the whole
// filesystem (see scanRootDenylist). It returns a short reason for the
// fixed-width block banner and a longer alternative-tools suggestion to
// print separately, or ("", "") if the command is fine. tokens must be
// original-case, shell-aware tokens (see shellTokenize) — quoted text (a
// sed/jq script, a mail body) must arrive as one opaque token so a "//"
// appearing inside it is never mistaken for a bare root-path argument
// (gt-mkrj).
func matchesUnboundedScan(tokens []string) (reason, alternative string) {
	fields := tokens
	vars := shellVarAssignments(tokens)
	for i, f := range fields {
		base := strings.ToLower(f)
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}

		rest := fields[i+1:]
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

		for _, arg := range rest {
			resolved := resolveShellVar(arg, vars)
			if !isUnboundedScanRoot(resolved) {
				continue
			}
			reason = fmt.Sprintf("Unbounded scan (%s rooted at %s)", base, arg)
			alternative = "Alternative: brew --prefix, pkg-config, 'go env GOROOT'/'go env GOMODCACHE', " +
				"or a search rooted inside the repo/rig instead of the whole filesystem."
			return reason, alternative
		}
	}
	return "", ""
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
