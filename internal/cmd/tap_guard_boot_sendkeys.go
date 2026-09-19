package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardBootSendKeysCmd = &cobra.Command{
	Use:   "boot-sendkeys",
	Short: "Block raw tmux send-keys from the boot watchdog (PreToolUse hook)",
	Long: `Block raw tmux send-keys in the boot watchdog role (gt-3mp1).

The boot watchdog is an ephemeral agent whose job is booting the Deacon
after a town restart. Typing into the Deacon's tmux pane with a raw
"tmux send-keys" can leave text staged but unsubmitted in the TUI — the
Deacon then sits at a prompt with a half-written message and never sees
the instruction. Boot must use "gt nudge --mode=immediate deacon" instead.

This guard reads tool_input.command off stdin and inspects the actual
command, rather than relying on the settings.json "if" permission-glob
(formerly Bash(*tmux*send-keys*)) to decide when it runs. Claude Code's
"if" evaluator treats a command it cannot statically resolve — a brace
group containing a quoted string (echo x{"a"}y, any JSON/dict literal), or
an argument-position $(...) substitution — as matching ANY pattern,
including every leading-* glob (gt-3mp1). The glob therefore blocked
unrelated boot commands. Registering this guard unconditionally (no "if")
and doing the real check here means it blocks only a command that actually
invokes tmux send-keys, no matter what confused the outer evaluator — the
same shape patrol-loop, dangerous-command and pr-workflow use.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardBootSendKeys,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardBootSendKeysCmd)
}

func runTapGuardBootSendKeys(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}
	command := extractCommand(input)
	if command == "" {
		// Fail OPEN, unlike pr-workflow's deliberate fail-closed fallback:
		// an unparsable stdin here means "we don't know what command this
		// is", and the only block this guard can emit is "no Bash for the
		// boot role at all" — exactly the regression refinery rejected on
		// gt-wisp-4wgk. Nothing to match against, so allow.
		return nil
	}
	if commandInvokesRawTmuxSendKeys(command, 0) {
		printBootSendKeysBlock()
		return NewSilentExit(2)
	}
	return nil
}

// tmuxSendKeysSubcommand is the tmux subcommand this guard blocks. Matched
// as a whole token, so an unrelated argument that merely contains the text
// (a quoted mail body, a filename) never fires it.
const tmuxSendKeysSubcommand = "send-keys"

// commandInvokesRawTmuxSendKeys reports whether command — the text of a
// single Bash tool call — invokes tmux's send-keys subcommand, recursing
// into any shell command embedded as an argument (bash -c/sh -c/eval) so a
// wrapped invocation (bash -c "tmux send-keys ...") is caught too, the same
// nesting evaluateDangerousCommand walks. Heredoc bodies are stripped first:
// they are data being written or piped, not shell syntax to evaluate, and
// scanning them is where this family of guards has picked up false
// positives before (gt-mkrj).
func commandInvokesRawTmuxSendKeys(command string, depth int) bool {
	command = stripHeredocBodies(command)
	tokens := shellTokenize(command)
	if matchesRawTmuxSendKeys(tokens) {
		return true
	}
	if depth >= maxDangerousNestDepth {
		return false
	}
	lowerTokens := make([]string, len(tokens))
	for i, t := range tokens {
		lowerTokens[i] = strings.ToLower(t)
	}
	for _, nested := range nestedCommands(tokens, lowerTokens) {
		if commandInvokesRawTmuxSendKeys(nested, depth+1) {
			return true
		}
	}
	return false
}

// matchesRawTmuxSendKeys reports whether tokens invoke tmux's send-keys
// subcommand. tokens must be shell-aware tokens (see shellTokenize), so
// quoted text stays as the single token it is.
//
// The command is split into segments on shell separators and each segment
// is judged separately, so a tmux in one command and a send-keys in another
// ("tmux ls && echo send-keys") is not read as an invocation.
func matchesRawTmuxSendKeys(tokens []string) bool {
	start := 0
	for i := 0; i <= len(tokens); i++ {
		if i < len(tokens) && !shellCommandSeparators[tokens[i]] {
			continue
		}
		if segmentInvokesTmuxSendKeys(tokens[start:i]) {
			return true
		}
		start = i + 1
	}
	return false
}

// tmuxCommandWrappers are commands whose whole job is to run another
// command, so the command word behind one is still in command position.
// Without them "sudo tmux send-keys" would read as "sudo's args happen to
// mention tmux". Only the plain-prefix forms are covered (no wrapper flags):
// the boot role has no reason to reach for anything more exotic, and the
// dangerous-command guard blocks sudo outright.
var tmuxCommandWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "command": true, "exec": true,
	"nohup": true, "time": true, "nice": true, "setsid": true, "stdbuf": true,
}

// segmentInvokesTmuxSendKeys reports whether one command segment — the
// tokens between two shell separators — invokes tmux's send-keys.
//
// Only the command word is considered: the first token past any leading
// VAR=value assignments and command wrappers. Requiring command position is
// what keeps prose out. Tokens are opaque per shlex, but a message that
// mentions both words as separate arguments ("gt mail send ... -s tmux -m
// send-keys") would otherwise read as an invocation, and a false block is
// the exact failure this guard exists to stop (gt-3mp1). The cost is that a
// tmux buried in an argument — "$(tmux send-keys ...)" — is not caught;
// nesting behind a shell invoker is (see commandInvokesRawTmuxSendKeys).
func segmentInvokesTmuxSendKeys(segment []string) bool {
	i := 0
	for i < len(segment) {
		if isShellAssignment(segment[i]) {
			i++
			continue
		}
		if tmuxCommandWrappers[strings.ToLower(segment[i])] {
			i++
			continue
		}
		break
	}
	if i >= len(segment) || !isTmuxCommandToken(segment[i]) {
		return false
	}
	for _, arg := range segment[i+1:] {
		if strings.EqualFold(arg, tmuxSendKeysSubcommand) {
			return true
		}
	}
	return false
}

// isShellAssignment reports whether tok is a leading VAR=value environment
// assignment ("FOO=1 tmux send-keys ..."), which precedes the command word
// without being it.
func isShellAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range tok[:eq] {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// isTmuxCommandToken reports whether tok names the tmux binary — "tmux", or
// a path to it ("/opt/homebrew/bin/tmux", "./tmux") — so a fully-qualified
// invocation is blocked the same as a bare one.
func isTmuxCommandToken(tok string) bool {
	base := strings.ToLower(tok)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base == "tmux"
}

// printBootSendKeysBlock prints the block banner for a raw tmux send-keys
// command in the boot role. The wording is the one the hook used to carry
// inline (gt-3mp1) and points at the nudge command that replaces it.
func printBootSendKeysBlock() {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ BOOT RAW tmux send-keys BLOCKED                              ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(os.Stderr, "║  Boot must not use raw tmux send-keys; it can leave unsubmitted  ║")
	fmt.Fprintln(os.Stderr, "║  text staged in the Deacon TUI.                                  ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Use: gt nudge --mode=immediate deacon \"message\"                ║")
	fmt.Fprintln(os.Stderr, "║  (do not add --force)                                            ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
