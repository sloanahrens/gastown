package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var directiveListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all directive files",
	Long: `List all directive files across town and rig levels.

Shows each directive file with its scope (town or rig) and whether any role
loads it. A file whose name is not a role name reaches no agent at all.

Examples:
  gt directive list`,
	RunE: runDirectiveList,
}

func init() {
	directiveCmd.AddCommand(directiveListCmd)
}

func runDirectiveList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	entries, err := listableDirectiveFiles(townRoot)
	if err != nil {
		return err
	}

	renderDirectiveList(cmd.OutOrStdout(), entries)
	return nil
}

// listableDirectiveFiles scans every level and drops the files with no
// content: an empty file carries no policy to report on. gt doctor still sees
// it, so nothing goes unmentioned.
func listableDirectiveFiles(townRoot string) ([]config.DirectiveFile, error) {
	files, err := config.ScanDirectiveFiles(townRoot, "")
	if err != nil {
		return nil, err
	}

	var entries []config.DirectiveFile
	for _, f := range files {
		if fileHasContent(f.Path) {
			entries = append(entries, f)
		}
	}
	return entries, nil
}

// renderDirectiveList prints the scan result. Unused files are listed rather
// than filtered: the list is where the operator learns they reach nobody.
func renderDirectiveList(w io.Writer, entries []config.DirectiveFile) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "No directive files found.")
		fmt.Fprintf(w, "\nUse 'gt directive edit <role>' to create one, or 'gt directive edit %s' for policy that applies to every role.\n",
			config.SharedDirectiveName)
		return
	}

	fmt.Fprintf(w, "Directive files (%d):\n\n", len(entries))
	fmt.Fprintf(w, "  %-10s %-12s %-26s %s\n", "SCOPE", "ROLE", "STATUS", "PATH")
	fmt.Fprintf(w, "  %-10s %-12s %-26s %s\n", "─────", "────", "──────", "────")

	var unused []string
	for _, e := range entries {
		status := "ok"
		switch {
		case e.Role == config.SharedDirectiveName:
			status = "every role"
		case e.Unused():
			status = "UNUSED (no such role)"
			unused = append(unused, e.Path)
		}
		fmt.Fprintf(w, "  %-10s %-12s %-26s %s\n", e.Scope, e.Role, status, e.Path)
	}

	if len(unused) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, style.Warning.Render("  Directive files that no role loads:"))
		for _, path := range unused {
			fmt.Fprintf(w, "    - %s\n", path)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  These are never rendered by gt prime. Rename each to a role name, or move shared policy into %s.md.\n",
			config.SharedDirectiveName)
	}
}
