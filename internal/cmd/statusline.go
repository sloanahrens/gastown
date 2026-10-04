package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	statusLineSession string
)

var statusLineCmd = &cobra.Command{
	Use:   "status-line",
	Short: "Output status line content for tmux (internal use)",
	Long: `Output formatted status line content for the tmux status bar.

Called internally by the tmux status-right configuration. Displays
the current rig, role, worker name, and active issue. Pass --session
to specify which tmux session to query.`,
	Hidden: true, // Internal command called by tmux
	RunE:   runStatusLine,
}

func init() {
	rootCmd.AddCommand(statusLineCmd)
	statusLineCmd.Flags().StringVar(&statusLineSession, "session", "", "Tmux session name")
}

func runStatusLine(cmd *cobra.Command, args []string) error {
	// Check E-stop first — prepend red indicator if active
	if townRoot, twErr := workspace.FindFromCwd(); twErr == nil {
		showEstop := false
		var info *estop.Info
		if estop.IsActive(townRoot) {
			showEstop = true
			info = estop.Read(townRoot)
		} else {
			// Check per-rig E-stop
			rigEnv := os.Getenv("GT_RIG")
			if rigEnv != "" && estop.IsRigActive(townRoot, rigEnv) {
				showEstop = true
				info = estop.ReadRig(townRoot, rigEnv)
			}
		}
		if showEstop {
			ts := ""
			if info != nil && !info.Timestamp.IsZero() {
				ts = info.Timestamp.Format("15:04")
			}
			fmt.Printf("#[bg=red,fg=white,bold] ESTOP %s #[default] ", ts)
		}
	}

	t := tmux.NewTmux()

	// Get session environment
	var polecat, crew, issue string

	if statusLineSession != "" {
		// Non-fatal: missing env vars are handled gracefully below
		polecat, _ = t.GetEnvironment(statusLineSession, "GT_POLECAT")
		crew, _ = t.GetEnvironment(statusLineSession, "GT_CREW")
		issue, _ = t.GetEnvironment(statusLineSession, "GT_ISSUE")
	} else {
		// Fallback to process environment
		polecat = os.Getenv("GT_POLECAT")
		crew = os.Getenv("GT_CREW")
		issue = os.Getenv("GT_ISSUE")
	}

	// Worker status line for the remaining roles.
	return runWorkerStatusLine(polecat, crew, issue)
}

// runWorkerStatusLine outputs status for crew or polecat sessions.
func runWorkerStatusLine(polecat, crew, issue string) error {
	// Determine agent type and identity
	var icon string
	if polecat != "" {
		icon = AgentTypeIcons[AgentPolecat]
	} else if crew != "" {
		icon = AgentTypeIcons[AgentCrew]
	}

	// Build status parts
	var parts []string
	currentWork := issue
	if currentWork != "" {
		if icon != "" {
			parts = append(parts, fmt.Sprintf("%s %s", icon, currentWork))
		} else {
			parts = append(parts, currentWork)
		}
	} else if icon != "" {
		parts = append(parts, icon)
	}

	// Output
	if len(parts) > 0 {
		fmt.Print(strings.Join(parts, " | ") + " |")
	}

	return nil
}

// isSessionWorking detects if a Claude Code session is actively working.
// Returns true if the ✻ symbol is visible in the pane (indicates Claude is processing).
// Returns false for idle sessions (showing ❯ prompt) or if state cannot be determined.
func isSessionWorking(t *tmux.Tmux, session string) bool {
	// Capture last few lines of the pane
	lines, err := t.CapturePaneLines(session, 5)
	if err != nil || len(lines) == 0 {
		return false
	}

	// Check all captured lines for the working indicator
	// ✻ appears in Claude's status line when actively processing
	for _, line := range lines {
		if strings.Contains(line, "✻") {
			return true
		}
	}

	return false
}
