package townstatus

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/tmux"
)

// resolveAgentDisplay inspects the actual running process in the tmux session
// to determine what runtime and model are being used. Falls back to config
// when the session isn't running.
func resolveAgentDisplay(townSettings *config.TownSettings, role string, sessionName string, running bool) (alias, info string) {
	// Map legacy role names to config role names
	configRole := role
	switch role {
	case "coordinator":
		configRole = constants.RoleMayor
	}

	// Get alias from config
	if townSettings != nil {
		alias = townSettings.RoleAgents[configRole]
		if alias == "" {
			alias = townSettings.DefaultAgent
		}
	}

	// If session is running, inspect the actual process
	if running && sessionName != "" {
		if detected := detectRuntimeFromSession(sessionName); detected != "" {
			info = detected
			return alias, info
		}
	}

	// Fall back to config-based display
	if townSettings != nil && alias != "" {
		rc := townSettings.Agents[alias]
		if rc != nil {
			info = buildInfoFromConfig(rc)
		} else {
			info = alias
		}
	}
	return alias, info
}

// detectRuntimeFromSession inspects the actual process tree in a tmux session
// to determine what agent runtime and model are in use.
func detectRuntimeFromSession(sessionName string) string {
	// Get the PID of the shell process in the tmux pane
	t := tmux.NewTmux()
	pid, err := t.GetPanePID(sessionName)
	if err != nil || pid == "" {
		return ""
	}

	// Walk child processes to find the actual agent (not the shell)
	cmdline := findAgentCmdline(pid)
	if cmdline == "" {
		return ""
	}

	return parseRuntimeInfo(cmdline)
}

// findAgentCmdline checks the pane process itself and its descendants for a known agent.
// The pane PID may BE the agent (e.g., claude), or the agent may be a child (e.g., shell → claude).
// Also handles wrapper processes (node /path/to/claude).
func findAgentCmdline(panePid string) string {
	// Check the pane process itself first
	cmdline := readCmdline(panePid)
	if isAgentCmdline(cmdline) {
		return cmdline
	}

	// Walk children (shell → agent)
	childrenPath := "/proc/" + panePid + "/task/" + panePid + "/children"
	childrenBytes, err := os.ReadFile(childrenPath)
	if err != nil {
		return cmdline // return whatever the pane process is
	}

	children := strings.Fields(string(childrenBytes))
	for _, childPid := range children {
		childCmd := readCmdline(childPid)
		if isAgentCmdline(childCmd) {
			return childCmd
		}
		// Check grandchildren (cgroup-wrap → agent)
		gcPath := "/proc/" + childPid + "/task/" + childPid + "/children"
		gcBytes, err := os.ReadFile(gcPath)
		if err != nil {
			continue
		}
		for _, gcPid := range strings.Fields(string(gcBytes)) {
			gcCmd := readCmdline(gcPid)
			if isAgentCmdline(gcCmd) {
				return gcCmd
			}
		}
	}

	return cmdline // return pane process cmdline as fallback
}

// isAgentCmdline returns true if the cmdline contains a known agent,
// either as the main command or as the first arg of a wrapper (node/bun).
func isAgentCmdline(cmdline string) bool {
	if cmdline == "" {
		return false
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return false
	}
	base := filepath.Base(parts[0])
	if isKnownAgent(base) {
		return true
	}
	// Check if wrapper (node/bun) is running an agent
	if isAgentWrapper(base) && len(parts) > 1 {
		argBase := filepath.Base(parts[1])
		return isKnownAgent(argBase)
	}
	return false
}

// readCmdline reads /proc/<pid>/cmdline and returns it as a space-joined string.
func readCmdline(pid string) string {
	data, err := os.ReadFile("/proc/" + pid + "/cmdline")
	if err != nil || len(data) == 0 {
		return ""
	}
	// cmdline uses null bytes as separators
	return string(data)
}

// extractBaseName gets the base command name from a null-separated cmdline.
func extractBaseName(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return ""
	}
	return filepath.Base(parts[0])
}

// isKnownAgent returns true if the command is a recognized agent runtime.
func isKnownAgent(base string) bool {
	return config.IsKnownPreset(base)
}

// isAgentWrapper returns true if the command is a runtime wrapper (node, bun, etc.)
// that may host an agent as its first argument.
func isAgentWrapper(base string) bool {
	switch base {
	case "node", "bun", "npx", "bunx":
		return true
	}
	return false
}

// parseRuntimeInfo extracts "runtime/model" from a null-separated cmdline.
// Handles direct invocation (claude --model opus) and wrapper patterns (node /path/to/claude).
func parseRuntimeInfo(cmdline string) string {
	if cmdline == "" {
		return ""
	}
	parts := strings.Split(cmdline, "\x00")
	if len(parts) == 0 {
		return ""
	}

	// Find the actual agent command — skip wrappers (node, bun, cgroup-wrap)
	cmd := ""
	startIdx := 0
	for i, part := range parts {
		base := filepath.Base(part)
		if isKnownAgent(base) {
			cmd = base
			startIdx = i
			break
		}
	}
	if cmd == "" {
		cmd = filepath.Base(parts[0])
	}

	// Extract the model from flags
	for i := startIdx; i < len(parts); i++ {
		arg := parts[i]
		if (arg == "--model" || arg == "-m") && i+1 < len(parts) && parts[i+1] != "" {
			return cmd + "/" + parts[i+1]
		}
	}

	return cmd
}

// buildInfoFromConfig builds display info from a RuntimeConfig (fallback when not running).
func buildInfoFromConfig(rc *config.RuntimeConfig) string {
	if rc.Command == "" {
		return "claude"
	}
	cmd := filepath.Base(rc.Command)
	if cmd == "" {
		cmd = "claude"
	}
	if cmd == "cgroup-wrap" && len(rc.Args) > 0 {
		cmd = rc.Args[0]
	}

	model := ""
	for i, arg := range rc.Args {
		if (arg == "--model" || arg == "-m") && i+1 < len(rc.Args) {
			model = rc.Args[i+1]
			break
		}
	}

	if model != "" {
		return cmd + "/" + model
	}
	return cmd
}
