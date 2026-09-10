package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var tapListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available tap handlers",
	Long: `List all tap handlers (guards, audits, injectors, checks).

Shows both registered (from registry.toml) and built-in tap commands.

Examples:
  gt tap list               # Show all available handlers
  gt tap list --guards      # Show only guard handlers`,
	RunE: runTapList,
}

var tapListGuardsOnly bool

func init() {
	tapCmd.AddCommand(tapListCmd)
	tapListCmd.Flags().BoolVar(&tapListGuardsOnly, "guards", false, "Show only guard handlers")
}

// tapHandler describes a tap handler for display.
type tapHandler struct {
	name        string
	kind        string // guard, audit, inject, check
	description string
	event       string
	// conditions lists the "if" command-pattern(s) that route to this
	// handler. The actual Claude Code matcher is always the bare tool name
	// (e.g. "Bash") — a pattern here is never a valid matcher value
	// (gt-5ihs).
	conditions  []string
	implemented bool
}

func runTapList(cmd *cobra.Command, args []string) error {
	// Built-in handlers (implemented as Go commands)
	handlers := []tapHandler{
		{
			name:        "pr-workflow",
			kind:        "guard",
			description: "Block PR creation and feature branches",
			event:       "PreToolUse",
			conditions:  []string{"Bash(gh pr create*)", "Bash(git checkout -b*)", "Bash(git switch -c*)"},
			implemented: true,
		},
		{
			name:        "dangerous-command",
			kind:        "guard",
			description: "Block sudo, package installs, rm -rf, force push, hard reset, etc.",
			event:       "PreToolUse",
			// Self-filters on tool_input.command directly — no "if" needed.
			conditions:  nil,
			implemented: true,
		},
	}

	// Try to load registry for additional handlers
	townRoot, err := workspace.FindFromCwd()
	if err == nil {
		registry, err := LoadRegistry(townRoot)
		if err == nil {
			for name, def := range registry.Hooks {
				// Skip hooks already listed as built-in
				if isBuiltIn(name, handlers) {
					continue
				}

				kind := classifyHook(def.Command)
				if tapListGuardsOnly && kind != "guard" {
					continue
				}

				handlers = append(handlers, tapHandler{
					name:        name,
					kind:        kind,
					description: def.Description,
					event:       def.Event,
					conditions:  def.Matchers,
					implemented: def.Enabled,
				})
			}
		}
	}

	// Sort by kind then name
	sort.Slice(handlers, func(i, j int) bool {
		if handlers[i].kind != handlers[j].kind {
			return handlers[i].kind < handlers[j].kind
		}
		return handlers[i].name < handlers[j].name
	})

	if tapListGuardsOnly {
		var filtered []tapHandler
		for _, h := range handlers {
			if h.kind == "guard" {
				filtered = append(filtered, h)
			}
		}
		handlers = filtered
	}

	if len(handlers) == 0 {
		fmt.Println(style.Dim.Render("No tap handlers found"))
		return nil
	}

	fmt.Printf("\n%s Tap Handlers\n\n", style.Bold.Render("⚡"))

	// Group by kind
	byKind := make(map[string][]tapHandler)
	kindOrder := []string{"guard", "audit", "inject", "check", "hook"}
	for _, h := range handlers {
		byKind[h.kind] = append(byKind[h.kind], h)
	}

	for _, kind := range kindOrder {
		group := byKind[kind]
		if len(group) == 0 {
			continue
		}

		kindLabel := cases.Title(language.English).String(kind) + "s"
		fmt.Printf("%s %s\n", style.Bold.Render("▸"), kindLabel)

		for _, h := range group {
			statusIcon := "●"
			statusStyle := style.Success
			if !h.implemented {
				statusIcon = "○"
				statusStyle = style.Dim
			}

			fmt.Printf("  %s %s\n", statusStyle.Render(statusIcon), style.Bold.Render(h.name))
			fmt.Printf("    %s\n", h.description)
			conditions := strings.Join(h.conditions, ", ")
			if conditions == "" {
				conditions = "(self-filtering)"
			}
			fmt.Printf("    %s %s  %s %s\n",
				style.Dim.Render("event:"), h.event,
				style.Dim.Render("if:"), conditions)
		}
		fmt.Println()
	}

	return nil
}

func isBuiltIn(name string, handlers []tapHandler) bool {
	for _, h := range handlers {
		if h.name == name || h.name+"-guard" == name {
			return true
		}
	}
	return false
}

func classifyHook(command string) string {
	if strings.Contains(command, "guard") {
		return "guard"
	}
	if strings.Contains(command, "audit") {
		return "audit"
	}
	if strings.Contains(command, "inject") {
		return "inject"
	}
	if strings.Contains(command, "check") {
		return "check"
	}
	return "hook"
}
