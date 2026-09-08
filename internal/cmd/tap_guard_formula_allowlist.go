package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/formula"
)

var tapGuardFormulaAllowlistCmd = &cobra.Command{
	Use:   "formula-allowlist",
	Short: "Constrain dog sessions to their formula's declared command allowlist",
	Long: `Enforce a formula's declared command allowlist (PreToolUse hook).

Dog formulas may declare a command_allowlist in their TOML — the only shell
commands a session executing that formula may run. This guard reads the Bash
tool input from stdin (Claude Code hook protocol), resolves the current dog's
assigned formula from its kennel state (.dog.json), and blocks any command
with a segment outside the declared allowlist (gt-9iv, follow-up to gt-61x).

A small built-in baseline is always permitted alongside the declared entries:
dog lifecycle commands (gt hook/prime/dog done/nudge/escalate, bd basics) and
common read-only utilities (jq, grep, cat, ...).

The guard is a no-op (exit 0) when:
  - not running as a dog (GT_ROLE != dog)
  - the dog has no work assigned, or its work is not a formula
  - the formula declares no command_allowlist

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED (command outside the formula's allowlist)`,
	RunE: runTapGuardFormulaAllowlist,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardFormulaAllowlistCmd)
}

// dogAllowlistBaseline lists command prefixes every dog may always run,
// regardless of the formula's declared allowlist: session lifecycle, issue
// tracking, escalation, and harmless read-only utilities. Deliberately
// excludes anything that can mutate state outside beads rows (no gt dolt
// cleanup, no rm, no dolt CLI, no xargs/awk/sed).
var dogAllowlistBaseline = []string{
	// Dog lifecycle and communication (see internal/templates/roles/dog.md.tmpl).
	"gt hook", "gt prime", "gt dog done", "gt nudge", "gt escalate",
	"gt mail inbox", "gt mail read", "gt dolt status",
	// Beads issue tracking — dogs file and update beads directly.
	"bd show", "bd list", "bd ready", "bd create", "bd update", "bd close", "bd comments",
	// Read-only shell utilities.
	"jq", "grep", "head", "tail", "wc", "sort", "uniq", "cut", "cat",
	"echo", "printf", "date", "ls", "pwd", "sleep", "true", "false",
	"test", "[", "cd", "which", "command",
}

func runTapGuardFormulaAllowlist(cmd *cobra.Command, args []string) error {
	// Read hook input from stdin (Claude Code protocol).
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open, like the other tap guards
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}

	formulaName, declared := resolveDogFormulaAllowlist(os.Getenv("GT_ROLE"), os.Getenv("GT_DOG_NAME"), os.Getenv("GT_ROOT"))
	if len(declared) == 0 {
		return nil
	}

	entries := make([]string, 0, len(declared)+len(dogAllowlistBaseline))
	entries = append(entries, declared...)
	entries = append(entries, dogAllowlistBaseline...)

	if checkErr := formula.CheckCommandAllowed(command, entries); checkErr != nil {
		printAllowlistBlock(formulaName, declared, checkErr)
		return NewSilentExit(2)
	}
	return nil
}

// resolveDogFormulaAllowlist returns the current dog's formula name and its
// declared command allowlist, or ("", nil) when no allowlist applies.
//
// Resolution: GT_ROLE must be "dog"; the kennel is GT_ROOT/deacon/dogs/<name>;
// the assigned work comes from .dog.json; the work must resolve to a formula
// (rig > town > embedded precedence) that declares command_allowlist.
//
// Errors fail open (no constraint): a missing .dog.json or unparseable formula
// must not brick the dog. This is safe because the policy inputs themselves
// are protected — while the allowlist is active, no Bash command that could
// tamper with .dog.json or the formula files matches the allowlist.
func resolveDogFormulaAllowlist(role, dogName, townRoot string) (string, []string) {
	if role != "dog" || dogName == "" || townRoot == "" {
		return "", nil
	}

	statePath := filepath.Join(townRoot, "deacon", "dogs", dogName, ".dog.json")
	data, err := os.ReadFile(statePath) //nolint:gosec // G304: path derived from gt-managed env
	if err != nil {
		return "", nil
	}
	var state struct {
		Work string `json:"work"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Work == "" {
		return "", nil
	}

	content, err := formula.ResolveFormulaContent(state.Work, townRoot, "")
	if err != nil {
		return "", nil // work is a bead ID or plugin, not a formula
	}
	f, err := formula.Parse(content)
	if err != nil {
		return "", nil
	}
	// Resolve extends so inherited allowlist entries apply; on resolution
	// failure fall back to the formula's own declaration.
	if resolved, err := formula.Resolve(f, []string{filepath.Join(townRoot, ".beads", "formulas")}); err == nil {
		f = resolved
	}
	if len(f.CommandAllowlist) == 0 {
		return "", nil
	}
	return state.Work, f.CommandAllowlist
}

// printAllowlistBlock prints the block banner to stderr.
func printAllowlistBlock(formulaName string, declared []string, checkErr error) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ COMMAND OUTSIDE FORMULA ALLOWLIST                            ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  Formula: %-53s ║\n", truncateStr(formulaName, 53))
	fmt.Fprintf(os.Stderr, "║  %-63s ║\n", truncateStr(checkErr.Error(), 63))
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  This formula constrains sessions to its declared commands:      ║")
	for _, entry := range declared {
		fmt.Fprintf(os.Stderr, "║    - %-59s ║\n", truncateStr(entry, 59))
	}
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  A refusal IS the designed stop — do not work around it.         ║")
	fmt.Fprintln(os.Stderr, "║  If this command is genuinely needed, escalate instead:          ║")
	fmt.Fprintln(os.Stderr, "║    gt escalate -s HIGH \"<formula>: need <command> because ...\"   ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
