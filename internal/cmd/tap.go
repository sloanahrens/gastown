package cmd

import (
	"github.com/spf13/cobra"
)

var tapCmd = &cobra.Command{
	Use:   "tap",
	RunE:  requireSubcommand,
	Short: "Claude Code hook handlers",
	Long: `Hook handlers for Claude Code PreToolUse and PostToolUse events.

These commands are called by Claude Code hooks to implement policies,
auditing, and input transformation. They tap into the tool execution
flow to guard, audit, inject, or check.

Subcommands:
  guard   - Block forbidden operations (PreToolUse, exit 2)
  audit   - Log/record tool executions (PostToolUse) [planned]
  inject  - Modify tool inputs (PreToolUse, updatedInput) [planned]
  check   - Validate after execution (PostToolUse) [planned]

Hook configuration in .claude/settings.json:
  {
    "PreToolUse": [{
      "matcher": "Bash|Monitor",
      "hooks": [{"command": "gt tap guard pr-workflow"}]
    }]
  }

Matcher matches the TOOL NAME only, and a guard that reads a shell command
goes on "Bash|Monitor" (gt-vx2mm) — a bare "Bash" lets a command run
through the Monitor tool reach the guard's target unguarded. A command
pattern like "Bash(gh pr create*)" belongs in a hook's "if" field, never
in "matcher" (gt-5ihs) — a pattern written into matcher never fires.
Built-in guards set no "if" at all: Claude Code's "if" evaluator resolves a
command it cannot statically analyze (a brace group holding a quoted
string, an argument-position $(...) substitution) as matching ANY pattern,
so an If-gated deny hook fires on unrelated commands (gt-3mp1). Guard
commands read tool_input.command off stdin and self-filter instead.

See ~/gt/docs/HOOKS.md for full documentation.`,
}

func init() {
	rootCmd.AddCommand(tapCmd)
}
