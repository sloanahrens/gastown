package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
// it finds embedded as an argument (bash -c/sh -c/eval). Without this, a
// wrapper like `bash -c "git reset --hard"` was invisible to every check:
// shlex collapses the quoted payload into a single token, so none of the
// fragment-based matchers (which require each fragment as its own token)
// ever fire on it (finding 4, gt-wisp-db27).
func evaluateDangerousCommand(command string, depth int) (reason, alternative string) {
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
	for _, nested := range nestedCommands(tokens, lowerTokens) {
		if r, alt := evaluateDangerousCommand(nested, depth+1); r != "" {
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
func nestedCommands(tokens, lowerTokens []string) []string {
	var nested []string
	for i, lt := range lowerTokens {
		if shellInvokers[lt] && i+1 < len(lowerTokens) && lowerTokens[i+1] == "-c" && i+2 < len(tokens) {
			nested = append(nested, strings.Join(tokens[i+2:], " "))
		}
		if lt == "eval" && i+1 < len(tokens) {
			nested = append(nested, strings.Join(tokens[i+1:], " "))
		}
	}
	return nested
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
	tokens, err := shlex.Split(command)
	if err != nil {
		return strings.Fields(command)
	}
	return tokens
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
			if !isUnboundedScanRoot(arg) {
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
	// A quoted argument collapses to one token with embedded spaces (e.g.
	// `dolt sql -q "DROP TABLE issues"` tokenizes to one token
	// "drop table issues"), which the exact-token check above never
	// matches — that made every DDL fragment pattern dead against realistic,
	// always-quoted SQL invocations (finding 4, gt-wisp-db27). Look for the
	// fragment as a whole word inside such multi-word tokens too.
	for _, tok := range tokens {
		if !strings.Contains(tok, " ") {
			continue
		}
		for _, word := range strings.Fields(tok) {
			if word == want {
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
