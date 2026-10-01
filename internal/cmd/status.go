package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/townstatus"
	"golang.org/x/term"
)

var statusJSON bool
var statusFast bool
var statusWatch bool
var statusInterval int
var statusVerbose bool
var statusHealthLine bool

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"stat"},
	GroupID: GroupDiag,
	Short:   "Show overall town status",
	Long: `Display the current status of the Gas Town workspace.

Shows town name, registered rigs, polecats, and crew.

Use --fast to skip mail lookups for faster execution.
Use --watch to continuously refresh status at regular intervals.

Use --line for the town's one health line, read from the report the daemon
writes every heartbeat, with the verdict as the exit code: 0 green,
1 degraded, 2 red, 3 unknown (no report, or one older than
operational.health.stale_after, default 10m). A green line is the verdict,
the report's age and the day's landings; any other line lists the non-green
fields worst first as key=value, where [R] marks a RECORDED field and [?] an
UNKNOWN one. gt status prints every field.`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
	statusCmd.Flags().BoolVar(&statusFast, "fast", false, "Skip mail lookups for faster execution")
	statusCmd.Flags().BoolVarP(&statusWatch, "watch", "w", false, "Watch mode: refresh status continuously")
	statusCmd.Flags().IntVarP(&statusInterval, "interval", "n", 2, "Refresh interval in seconds")
	statusCmd.Flags().BoolVarP(&statusVerbose, "verbose", "v", false, "Show detailed multi-line output per agent")
	statusCmd.Flags().BoolVar(&statusHealthLine, "line", false, "Print the one health line; exit 0 green, 1 degraded, 2 red, 3 unknown")
	rootCmd.AddCommand(statusCmd)
}

// doltCommitMarker is the Services-line marker for databases over the
// commits-per-day limit, "" when none is.
func doltCommitMarker(info *townstatus.DoltInfo) string {
	over := doltserver.OverCommitBudget(info.CommitsLastDay, info.CommitsPerDayWarn)
	if len(over) == 0 {
		return ""
	}
	return fmt.Sprintf("⚠ commits/24h %s > %d", doltserver.FormatCommitCounts(over), info.CommitsPerDayWarn)
}

func runStatus(cmd *cobra.Command, args []string) error {
	if statusHealthLine {
		return runStatusHealthLine(cmd)
	}
	if statusWatch {
		return runStatusWatch(cmd, args)
	}
	return runStatusOnce(cmd, args)
}

// gatherTownStatus is the command's one call into the leaf package: the flags
// and the two caller-owned probes go in, the town's status comes out.
func gatherTownStatus() (townstatus.TownStatus, error) {
	return townstatus.Gather(townstatus.Options{
		Fast:        statusFast,
		Registry:    townRegistry(),
		DaemonProbe: daemon.IsRunning,
		DNDProbe:    detectCurrentDNDStatus,
	})
}

// validateStatusWatch rejects the flag combinations --watch cannot run with:
// --json, and an interval (seconds) that is not positive.
func validateStatusWatch(jsonOut bool, interval int) error {
	if jsonOut {
		return fmt.Errorf("--json and --watch cannot be used together")
	}
	if interval <= 0 {
		return fmt.Errorf("interval must be positive, got %d", interval)
	}
	return nil
}

func runStatusWatch(_ *cobra.Command, _ []string) error {
	if err := validateStatusWatch(statusJSON, statusInterval); err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	ticker := time.NewTicker(time.Duration(statusInterval) * time.Second)
	defer ticker.Stop()

	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	// Cache the last successful status to handle transient tmux/beads
	// failures. Watch mode spawns many tmux subprocesses per iteration;
	// under load the tmux server can intermittently fail, causing all
	// agents to appear as not running (empty bubbles).
	var cachedStatus *townstatus.TownStatus
	var cachedAt time.Time
	maxStale := time.Duration(statusInterval) * time.Second * 5

	for {
		var buf bytes.Buffer

		if isTTY {
			buf.WriteString("\033[H\033[2J") // ANSI: cursor home + clear screen
		}

		timestamp := time.Now().Format("15:04:05")
		header := fmt.Sprintf("[%s] gt status --watch (every %ds, Ctrl+C to stop)", timestamp, statusInterval)
		if isTTY {
			fmt.Fprintf(&buf, "%s\n\n", style.Dim.Render(header))
		} else {
			fmt.Fprintf(&buf, "%s\n\n", header)
		}

		status, err := gatherTownStatus()
		usedCache := false

		// On error, retry once before giving up.
		if err != nil {
			status, err = gatherTownStatus()
		}

		if err == nil {
			// Detect degraded results: zero running agents when we
			// previously had some. This indicates a transient tmux
			// failure rather than all agents legitimately stopping.
			running := townstatus.CountRunningAgents(status)
			if running == 0 && cachedStatus != nil &&
				townstatus.CountRunningAgents(*cachedStatus) > 0 {
				// Retry once to confirm.
				retry, retryErr := gatherTownStatus()
				if retryErr == nil &&
					townstatus.CountRunningAgents(retry) > 0 {
					status = retry
				} else if time.Since(cachedAt) < maxStale {
					status = *cachedStatus
					usedCache = true
				}
			}
		} else if cachedStatus != nil &&
			time.Since(cachedAt) < maxStale {
			// Complete failure even after retry — use cache.
			status = *cachedStatus
			usedCache = true
			err = nil
		}

		if err != nil {
			fmt.Fprintf(&buf, "Error: %v\n", err)
		} else {
			if !usedCache {
				statusCopy := status
				cachedStatus = &statusCopy
				cachedAt = time.Now()
			}
			if usedCache {
				staleNote := fmt.Sprintf(
					"(using cached data from %s)",
					cachedAt.Format("15:04:05"),
				)
				if isTTY {
					fmt.Fprintf(&buf, "%s\n",
						style.Dim.Render(staleNote))
				} else {
					fmt.Fprintf(&buf, "%s\n", staleNote)
				}
			}
			if err := outputStatusText(&buf, status); err != nil {
				fmt.Fprintf(&buf, "Error: %v\n", err)
			}
		}

		// Write the entire frame atomically to prevent the terminal from
		// rendering a blank screen between the clear and the content.
		_, _ = os.Stdout.Write(buf.Bytes())

		select {
		case <-sigChan:
			if isTTY {
				fmt.Println("\nStopped.")
			}
			return nil
		case <-ticker.C:
		}
	}
}

func runStatusOnce(_ *cobra.Command, _ []string) error {
	status, err := gatherTownStatus()
	if err != nil {
		return err
	}
	if statusJSON {
		return outputStatusJSON(status)
	}
	return outputStatusText(os.Stdout, status)
}

func outputStatusJSON(status townstatus.TownStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func outputStatusText(w io.Writer, status townstatus.TownStatus) error {
	// Header
	fmt.Fprintf(w, "%s %s\n", style.Bold.Render("Town:"), status.Name)
	fmt.Fprintf(w, "%s\n\n", style.Dim.Render(status.Location))

	// Health report, every field (gt-s3rec.2)
	if len(status.HealthLines) > 0 {
		fmt.Fprintf(w, "%s %s\n", style.Bold.Render("Health:"), status.HealthLines[0])
		for _, l := range status.HealthLines[1:] {
			fmt.Fprintln(w, l)
		}
		fmt.Fprintln(w)
	}

	// E-stop banner (if active)
	addEstopToStatus(w, status.Location)

	// Paused-agent banner (gt-ahik): surface sanctioned pauses with reasons
	addPausedToStatus(w, status.Location)

	// Overseer info
	if status.Overseer != nil {
		overseerDisplay := status.Overseer.Name
		if status.Overseer.Email != "" {
			overseerDisplay = fmt.Sprintf("%s <%s>", status.Overseer.Name, status.Overseer.Email)
		} else if status.Overseer.Username != "" && status.Overseer.Username != status.Overseer.Name {
			overseerDisplay = fmt.Sprintf("%s (@%s)", status.Overseer.Name, status.Overseer.Username)
		}
		fmt.Fprintf(w, "👤 %s %s\n", style.Bold.Render("Overseer:"), overseerDisplay)
		if status.Overseer.UnreadMail > 0 {
			fmt.Fprintf(w, "   📬 %d unread\n", status.Overseer.UnreadMail)
		}
		fmt.Fprintln(w)
	}

	// Current agent notification mode (DND)
	if status.DND != nil {
		icon := "🔔"
		state := "off"
		desc := "notifications normal"
		if status.DND.Enabled {
			icon = "🔕"
			state = "on"
			desc = "notifications muted"
		}
		fmt.Fprintf(w, "%s %s %s", icon, style.Bold.Render("DND:"), style.Bold.Render(state))
		if status.DND.Agent != "" {
			fmt.Fprintf(w, " %s", style.Dim.Render("("+status.DND.Agent+")"))
		}
		fmt.Fprintf(w, "\n   %s\n\n", style.Dim.Render(desc))
	}

	// Infrastructure services
	if status.Daemon != nil || status.Dolt != nil || status.Tmux != nil {
		fmt.Fprintf(w, "%s ", style.Bold.Render("Services:"))
		var parts []string
		if status.Daemon != nil {
			if status.Daemon.Running {
				parts = append(parts, fmt.Sprintf("daemon %s", style.Dim.Render(fmt.Sprintf("(PID %d)", status.Daemon.PID))))
			} else {
				parts = append(parts, fmt.Sprintf("daemon %s", style.Dim.Render("(stopped)")))
			}
		}
		if status.Dolt != nil {
			if status.Dolt.Remote {
				parts = append(parts, fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(remote :%d)", status.Dolt.Port))))
			} else if status.Dolt.Running {
				dataDir := status.Dolt.DataDir
				if home, err := os.UserHomeDir(); err == nil {
					dataDir = strings.Replace(dataDir, home, "~", 1)
				}
				parts = append(parts, fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(PID %d, :%d, %s)", status.Dolt.PID, status.Dolt.Port, dataDir))))
			} else if status.Dolt.PortConflict {
				parts = append(parts, fmt.Sprintf("dolt %s", style.Bold.Render(fmt.Sprintf("(stopped, :%d ⚠ port used by %s)", status.Dolt.Port, status.Dolt.ConflictOwner))))
			} else {
				parts = append(parts, fmt.Sprintf("dolt %s", style.Dim.Render(fmt.Sprintf("(stopped, :%d)", status.Dolt.Port))))
			}
			if marker := doltCommitMarker(status.Dolt); marker != "" {
				parts[len(parts)-1] += " " + style.Bold.Render(marker)
			}
		}
		if status.Tmux != nil {
			if status.Tmux.Running {
				parts = append(parts, fmt.Sprintf("tmux %s", style.Dim.Render(fmt.Sprintf("(-L %s, PID %d, %d sessions, %s)", status.Tmux.Socket, status.Tmux.PID, status.Tmux.SessionCount, status.Tmux.SocketPath))))
			} else {
				parts = append(parts, fmt.Sprintf("tmux %s", style.Dim.Render(fmt.Sprintf("(-L %s, no server)", status.Tmux.Socket))))
			}
		}
		fmt.Fprintf(w, "%s\n", strings.Join(parts, "  "))
		fmt.Fprintln(w)
	}

	// Container-suite gate slot (gt-bcsq): only one Docker-backed test suite
	// may run at a time townwide. Only shown while held.
	if status.Slot != nil {
		fmt.Fprintf(w, "🔒 %s %s %s\n\n",
			style.Bold.Render("Container suite running:"),
			status.Slot.Role,
			style.Dim.Render(fmt.Sprintf("(pid %d, age %s)", status.Slot.PID, time.Since(status.Slot.AcquiredAt).Round(time.Second))))
	}

	if len(status.LivenessUnknown) > 0 {
		fmt.Fprintf(w, "%s %s\n\n",
			style.Bold.Render("Agent liveness unknown (query failed, shown as running):"),
			strings.Join(status.LivenessUnknown, ", "))
	}

	// Role icons - uses centralized emojis from constants package
	roleIcons := map[string]string{
		constants.RoleMayor:   constants.EmojiMayor,
		constants.RoleCrew:    constants.EmojiCrew,
		constants.RolePolecat: constants.EmojiPolecat,
		// Legacy names for backwards compatibility
		"coordinator": constants.EmojiMayor,
	}

	// Global Agents (the Mayor)
	for _, agent := range status.Agents {
		icon := roleIcons[agent.Role]
		if icon == "" {
			icon = roleIcons[agent.Name]
		}
		if statusVerbose {
			fmt.Fprintf(w, "%s %s\n", icon, style.Bold.Render(capitalizeFirst(agent.Name)))
			renderAgentDetails(w, agent, "   ", nil, status.Location)
			fmt.Fprintln(w)
		} else {
			// Compact: icon + name on one line
			renderAgentCompact(w, agent, icon+" ", nil, status.Location)
		}
	}
	if !statusVerbose && len(status.Agents) > 0 {
		fmt.Fprintln(w)
	}

	if len(status.Rigs) == 0 {
		fmt.Fprintf(w, "%s\n", style.Dim.Render("No rigs registered. Use 'gt rig add' to add one."))
		return nil
	}

	// Rigs
	for _, r := range status.Rigs {
		// Rig header with separator
		fmt.Fprintf(w, "─── %s ───────────────────────────────────────────\n\n", style.Bold.Render(r.Name+"/"))

		// Group agents by role
		var crews, polecats []townstatus.AgentRuntime
		for _, agent := range r.Agents {
			switch agent.Role {
			case constants.RoleCrew:
				crews = append(crews, agent)
			case constants.RolePolecat:
				polecats = append(polecats, agent)
			}
		}

		// Crew
		if len(crews) > 0 {
			if statusVerbose {
				fmt.Fprintf(w, "%s %s (%d)\n", roleIcons[constants.RoleCrew], style.Bold.Render("Crew"), len(crews))
				for _, agent := range crews {
					renderAgentDetails(w, agent, "   ", r.Hooks, status.Location)
				}
				fmt.Fprintln(w)
			} else {
				fmt.Fprintf(w, "%s %s (%d)\n", roleIcons[constants.RoleCrew], style.Bold.Render("Crew"), len(crews))
				for _, agent := range crews {
					renderAgentCompact(w, agent, "   ", r.Hooks, status.Location)
				}
			}
		}

		// Polecats
		if len(polecats) > 0 {
			if statusVerbose {
				fmt.Fprintf(w, "%s %s (%d)\n", roleIcons[constants.RolePolecat], style.Bold.Render("Polecats"), len(polecats))
				for _, agent := range polecats {
					renderAgentDetails(w, agent, "   ", r.Hooks, status.Location)
				}
				fmt.Fprintln(w)
			} else {
				fmt.Fprintf(w, "%s %s (%d)\n", roleIcons[constants.RolePolecat], style.Bold.Render("Polecats"), len(polecats))
				for _, agent := range polecats {
					renderAgentCompact(w, agent, "   ", r.Hooks, status.Location)
				}
			}
		}

		// No agents
		if len(crews) == 0 && len(polecats) == 0 {
			fmt.Fprintf(w, "   %s\n", style.Dim.Render("(no agents)"))
		}
		fmt.Fprintln(w)
	}

	return nil
}

// renderAgentDetails renders full agent bead details
func renderAgentDetails(w io.Writer, agent townstatus.AgentRuntime, indent string, hooks []townstatus.AgentHookInfo, townRoot string) { //nolint:unparam // indent kept for future customization
	// Line 1: Agent bead ID + status
	// Per gt-zecmc: derive status from tmux (observable reality), not bead state.
	// "Discover, don't track" - agent liveness is observable from tmux session.
	sessionExists := agent.Running

	var statusStr string
	var stateInfo string

	if sessionExists {
		statusStr = style.Success.Render("running")
	} else {
		statusStr = style.Error.Render("stopped")
	}

	// Show non-observable states that represent intentional agent decisions.
	// These can't be discovered from tmux and are legitimately recorded in beads.
	beadState := agent.State
	switch beadState {
	case "stuck":
		// Agent escalated - needs help
		stateInfo = style.Warning.Render(" [stuck]")
	case "awaiting-gate":
		// Agent waiting for external trigger (phase gate)
		stateInfo = style.Dim.Render(" [awaiting-gate]")
	case "paused":
		// A paused bead state with no agentpause marker behind it (a
		// stale mirror). Overridden by the block below when a marker exists.
		stateInfo = style.Dim.Render(" [paused]")
	case "muted", "degraded":
		// Other intentional non-observable states
		stateInfo = style.Dim.Render(fmt.Sprintf(" [%s]", beadState))
		// Ignore observable states: "running", "idle", "dead", "done", "stopped", ""
		// These should be derived from tmux, not bead.
	}

	// Sanctioned pause (gt-ahik): driven by agent.Paused (the marker file,
	// the only source of truth), not beadState — the bead mirror can lag or
	// fail to sync, and a marker-only pause must still show here.
	if agent.Paused {
		if agent.PausedReason != "" {
			stateInfo = style.Dim.Render(fmt.Sprintf(" [paused: %s]", agent.PausedReason))
		} else {
			stateInfo = style.Dim.Render(" [paused]")
		}
	}

	// Build agent bead ID using canonical naming: prefix-rig-role-name
	agentBeadID := "gt-" + agent.Name
	if agent.Address != "" && agent.Address != agent.Name {
		// Use address for full path agents like gastown/crew/joe → gt-gastown-crew-joe
		addr := strings.TrimSuffix(agent.Address, "/") // Remove trailing slash for global agents
		parts := strings.Split(addr, "/")
		if len(parts) == 1 {
			// Global agent: mayor/ → hq-mayor
			agentBeadID = beads.AgentBeadIDWithPrefix(beads.TownBeadsPrefix, "", parts[0], "")
		} else if len(parts) >= 2 {
			rig := parts[0]
			prefix := beads.GetPrefixForRig(townRoot, rig)
			if parts[1] == constants.RoleCrew && len(parts) >= 3 {
				agentBeadID = beads.CrewBeadIDWithPrefix(prefix, rig, parts[2])
			} else if len(parts) == 2 {
				// polecat: rig/name
				agentBeadID = beads.PolecatBeadIDWithPrefix(prefix, rig, parts[1])
			}
		}
	}

	fmt.Fprintf(w, "%s%s %s%s\n", indent, style.Dim.Render(agentBeadID), statusStr, stateInfo)

	// Line 2: Agent runtime info
	if agent.AgentInfo != "" {
		fmt.Printf("%s  agent: %s\n", indent, agent.AgentInfo)
	}

	// Line 3: Hook bead (pinned work)
	hookStr := style.Dim.Render("(none)")
	hookBead := agent.HookBead
	hookTitle := agent.WorkTitle

	// Fall back to hooks array if agent bead doesn't have hook info
	if hookBead == "" && hooks != nil {
		for _, h := range hooks {
			if h.Agent == agent.Address && h.HasWork {
				hookBead = h.Molecule
				hookTitle = h.Title
				break
			}
		}
	}

	if hookBead != "" {
		if hookTitle != "" {
			hookStr = fmt.Sprintf("%s → %s", hookBead, truncateWithEllipsis(hookTitle, 40))
		} else {
			hookStr = hookBead
		}
	} else if hookTitle != "" {
		// Has title but no molecule ID
		hookStr = truncateWithEllipsis(hookTitle, 50)
	}

	fmt.Fprintf(w, "%s  hook: %s\n", indent, hookStr)

	// Line 4: Notification mode (DND)
	if agent.NotificationLevel == beads.NotifyMuted {
		fmt.Fprintf(w, "%s  notify: 🔕 muted (DND)\n", indent)
	}

	// Line 5: Mail (if any unread)
	if agent.UnreadMail > 0 {
		mailStr := fmt.Sprintf("📬 %d unread", agent.UnreadMail)
		if agent.FirstSubject != "" {
			mailStr = fmt.Sprintf("📬 %d unread → %s", agent.UnreadMail, truncateWithEllipsis(agent.FirstSubject, 35))
		}
		fmt.Fprintf(w, "%s  mail: %s\n", indent, mailStr)
	}
}

// renderAgentCompact renders a single-line agent status
func renderAgentCompact(w io.Writer, agent townstatus.AgentRuntime, indent string, hooks []townstatus.AgentHookInfo, _ string) {
	// Build status indicator (gt-zecmc: use tmux state, not bead state)
	statusIndicator := buildStatusIndicator(agent)

	// Get hook info
	hookBead := agent.HookBead
	hookTitle := agent.WorkTitle
	if hookBead == "" && hooks != nil {
		for _, h := range hooks {
			if h.Agent == agent.Address && h.HasWork {
				hookBead = h.Molecule
				hookTitle = h.Title
				break
			}
		}
	}

	// Build hook suffix
	hookSuffix := ""
	if hookBead != "" {
		if hookTitle != "" {
			hookSuffix = style.Dim.Render(" → ") + truncateWithEllipsis(hookTitle, 30)
		} else {
			hookSuffix = style.Dim.Render(" → ") + hookBead
		}
	} else if hookTitle != "" {
		hookSuffix = style.Dim.Render(" → ") + truncateWithEllipsis(hookTitle, 30)
	}

	// Mail indicator
	mailSuffix := ""
	if agent.UnreadMail > 0 {
		mailSuffix = fmt.Sprintf(" 📬%d", agent.UnreadMail)
	}

	// Agent runtime info
	agentSuffix := ""
	if agent.AgentInfo != "" {
		agentSuffix = " " + style.Dim.Render("["+agent.AgentInfo+"]")
	}

	// Print single line: name + status + agent-info + hook + mail
	fmt.Fprintf(w, "%s%-12s %s%s%s%s\n", indent, agent.Name, statusIndicator, agentSuffix, hookSuffix, mailSuffix)
}

// buildStatusIndicator creates the visual status indicator for an agent.
// Per gt-zecmc: uses tmux state (observable reality), not bead state.
// Non-observable states (stuck, awaiting-gate, muted, etc.) are shown as suffixes.
func buildStatusIndicator(agent townstatus.AgentRuntime) string {
	sessionExists := agent.Running

	// Base indicator from tmux state
	var indicator string
	if sessionExists {
		indicator = style.Success.Render("●")
	} else {
		indicator = style.Error.Render("○")
	}

	// Add non-observable state suffix if present
	beadState := agent.State
	switch beadState {
	case "stuck":
		indicator += style.Warning.Render(" stuck")
	case "awaiting-gate":
		indicator += style.Dim.Render(" gate")
	case "paused":
		// A bead mirror with no marker — see renderAgentDetails.
		indicator += style.Dim.Render(" paused")
	case "muted", "degraded":
		indicator += style.Dim.Render(" " + beadState)
		// Ignore observable states: running, idle, dead, done, stopped, ""
	}

	// Sanctioned pause (gt-ahik): driven by agent.Paused (the marker file),
	// not beadState — see the identical comment in renderAgentDetails.
	if agent.Paused {
		if agent.PausedReason != "" {
			indicator += style.Dim.Render(" paused: " + truncateWithEllipsis(agent.PausedReason, 24))
		} else {
			indicator += style.Dim.Render(" paused")
		}
	}

	if agent.NotificationLevel == beads.NotifyMuted {
		indicator += style.Dim.Render(" 🔕")
	}

	return indicator
}

// formatHookInfo formats the hook bead and title for display
func formatHookInfo(hookBead, title string, maxLen int) string {
	if hookBead == "" {
		return ""
	}
	if title == "" {
		return fmt.Sprintf(" → %s", hookBead)
	}
	title = truncateWithEllipsis(title, maxLen)
	return fmt.Sprintf(" → %s", title)
}

// truncateWithEllipsis shortens a string to maxLen, adding "..." if truncated
func truncateWithEllipsis(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// capitalizeFirst capitalizes the first letter of a string
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// detectCurrentDNDStatus returns DND status for the currently resolved role context.
// Returns nil when role context cannot be determined (e.g. outside agent context).
func detectCurrentDNDStatus(townRoot string) *townstatus.DNDInfo {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}

	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return nil
	}

	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		return nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil || level == "" {
		level = beads.NotifyNormal
	}

	return &townstatus.DNDInfo{
		Enabled: level == beads.NotifyMuted,
		Level:   level,
		Agent:   agentBeadID,
	}
}
