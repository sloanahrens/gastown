package townstatus

import (
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TownStatus represents the overall status of the workspace.
type TownStatus struct {
	Name     string         `json:"name"`
	Location string         `json:"location"`
	Overseer *OverseerInfo  `json:"overseer,omitempty"` // Human operator
	DND      *DNDInfo       `json:"dnd,omitempty"`      // Current agent DND status
	Daemon   *ServiceInfo   `json:"daemon,omitempty"`   // Daemon status
	Dolt     *DoltInfo      `json:"dolt,omitempty"`     // Dolt server status
	Tmux     *TmuxInfo      `json:"tmux,omitempty"`     // Tmux server status
	Agents   []AgentRuntime `json:"agents"`             // Global agents (the Mayor)
	Rigs     []RigStatus    `json:"rigs"`
	Summary  StatusSum      `json:"summary"`
	Slot     *SlotInfo      `json:"container_slot,omitempty"` // Container-suite gate slot (gt-bcsq)
	// LivenessUnknown lists sessions whose agent-liveness query failed. They
	// are shown as running (unknown is not dead) and named here so the
	// failure is visible (gt-fcxe9.1).
	LivenessUnknown []string `json:"liveness_unknown,omitempty"`
	// Health is the daemon's last health report (gt-s3rec.2); nil when
	// there is none. HealthLines is it rendered, every field — what
	// `gt status --line` prints as its one line.
	Health      *townhealth.Report `json:"health,omitempty"`
	HealthLines []string           `json:"-"`
}

// SlotInfo represents the town-level container-suite gate slot (see
// internal/slot). Only populated in the status output when the slot is
// currently held — an idle slot is not worth a line in every 'gt status'.
type SlotInfo struct {
	Role       string    `json:"role"`
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type ServiceInfo struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// DoltInfo represents the Dolt server status.
type DoltInfo struct {
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	Port          int    `json:"port"`
	Remote        bool   `json:"remote,omitempty"`
	DataDir       string `json:"data_dir,omitempty"`
	PortConflict  bool   `json:"port_conflict,omitempty"`  // Port taken by another town's Dolt
	ConflictOwner string `json:"conflict_owner,omitempty"` // --data-dir of the process holding the port
	// Stray lists Dolt servers on the machine that are not the town's own,
	// each with the command that clears it (gt-gyw5w). Empty when there are
	// none. Not read under --fast.
	Stray []StrayDoltInfo `json:"stray,omitempty"`
	// CommitsLastDay is the commits-per-day meter (gt-8z769.4): each
	// database's Dolt commits in the last 24h. Not read under --fast.
	CommitsLastDay []doltserver.DBCommits `json:"commits_last_day,omitempty"`
	// CommitsPerDayWarn is the per-database limit the meter is held to
	// (operational.dolt.commits_per_day_warn).
	CommitsPerDayWarn int `json:"commits_per_day_warn,omitempty"`
}

// StrayDoltInfo is one Dolt server process that is not the town's own, with
// the command that clears it. Port and DataDir are zero/empty when the
// process's argv named neither; the Remedy is then the pid alone.
type StrayDoltInfo struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port,omitempty"`
	DataDir string `json:"data_dir,omitempty"`
	Remedy  string `json:"remedy"`
}

// TmuxInfo represents the tmux server status.
type TmuxInfo struct {
	Socket       string `json:"socket"`                // Socket name derived from town name (e.g., "gt-test")
	SocketPath   string `json:"socket_path,omitempty"` // Full socket path (e.g., /tmp/tmux-501/gt-test)
	Running      bool   `json:"running"`               // Is the tmux server running?
	PID          int    `json:"pid,omitempty"`         // PID of the tmux server process
	SessionCount int    `json:"session_count"`         // Number of sessions
}

// OverseerInfo represents the human operator's identity and status.
type OverseerInfo struct {
	Name       string `json:"name"`
	Email      string `json:"email,omitempty"`
	Username   string `json:"username,omitempty"`
	Source     string `json:"source"`
	UnreadMail int    `json:"unread_mail"`
}

// DNDInfo represents Do Not Disturb status for the current agent context.
type DNDInfo struct {
	Enabled bool   `json:"enabled"`
	Level   string `json:"level"`
	Agent   string `json:"agent,omitempty"`
}

// AgentRuntime represents the runtime state of an agent.
type AgentRuntime struct {
	Name              string `json:"name"`                         // Display name (e.g., "mayor", "nux")
	Address           string `json:"address"`                      // Full address (e.g., "greenplace/nux")
	Session           string `json:"session"`                      // tmux session name
	Role              string `json:"role"`                         // Role type
	Running           bool   `json:"running"`                      // Is tmux session running?
	HasWork           bool   `json:"has_work"`                     // Has pinned work?
	WorkTitle         string `json:"work_title,omitempty"`         // Title of pinned work
	HookBead          string `json:"hook_bead,omitempty"`          // Pinned bead ID from agent bead
	State             string `json:"state,omitempty"`              // Agent state from agent bead
	NotificationLevel string `json:"notification_level,omitempty"` // Notification level (verbose, normal, muted)
	UnreadMail        int    `json:"unread_mail"`                  // Number of unread messages
	FirstSubject      string `json:"first_subject,omitempty"`      // Subject of first unread message
	AgentAlias        string `json:"agent_alias,omitempty"`        // Configured agent name (e.g., "claude-opus")
	AgentInfo         string `json:"agent_info,omitempty"`         // Runtime summary (e.g., "claude/opus")
	Paused            bool   `json:"paused,omitempty"`             // True when the pause marker file says paused (gt-ahik)
	PausedReason      string `json:"paused_reason,omitempty"`      // Reason from the pause marker (gt agent pause)
}

// RigStatus represents status of a single rig.
type RigStatus struct {
	Name         string          `json:"name"`
	Polecats     []string        `json:"polecats"`
	PolecatCount int             `json:"polecat_count"`
	Crews        []string        `json:"crews"`
	CrewCount    int             `json:"crew_count"`
	Hooks        []AgentHookInfo `json:"hooks,omitempty"`
	Agents       []AgentRuntime  `json:"agents,omitempty"` // Runtime state of all agents in rig
}

// AgentHookInfo represents an agent's hook (pinned work) status.
type AgentHookInfo struct {
	Agent    string `json:"agent"`              // Agent address (e.g., "greenplace/toast", "greenplace/crew/max")
	Role     string `json:"role"`               // Role type (polecat, crew)
	HasWork  bool   `json:"has_work"`           // Whether agent has pinned work
	Molecule string `json:"molecule,omitempty"` // Attached molecule ID
	Title    string `json:"title,omitempty"`    // Pinned bead title
}

// StatusSum provides summary counts.
type StatusSum struct {
	RigCount     int `json:"rig_count"`
	PolecatCount int `json:"polecat_count"`
	CrewCount    int `json:"crew_count"`
	ActiveHooks  int `json:"active_hooks"`
}
