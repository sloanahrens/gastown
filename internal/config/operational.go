package config

import (
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/townhealth"
)

// Compiled-in defaults for operational thresholds.
// These are the values used when no config override is provided.
// Each was previously a hardcoded const scattered across the codebase.

// Session defaults.
const (
	DefaultStartupNudgeVerifyDelay = 25 * time.Second
	DefaultStartupNudgeMaxRetries  = 2
)

// Nudge defaults.
const (
	DefaultNudgeMaxQueueDepth     = 50
	DefaultNudgeStaleClaimTimeout = 5 * time.Minute
	// DefaultNudgeMaxDeliveryAttempts bounds how many times a nudge may be
	// requeued after a failed injection before it is dropped. Without a bound,
	// a delivery path that errors on every poll tick (but still injects the
	// text) re-injects the same nudge indefinitely — see gt-tmlu.
	DefaultNudgeMaxDeliveryAttempts = 3
	// DefaultNudgeRequeueBackoff spaces out delivery retries so a failing
	// injection cannot re-inject at the poll interval (gt-tmlu).
	DefaultNudgeRequeueBackoff = 30 * time.Second
)

// Daemon defaults.
const (
	DefaultPolecatIdleSessionTimeout = 15 * time.Minute
	DefaultRecoveryHeartbeatInterval = 3 * time.Minute

	// Pressure check defaults — fully opt-in. All zero = disabled.
	// Configure in settings/config.json under operational.daemon to enable.
	// Example: {"pressure_cpu_threshold": 3.0, "pressure_mem_threshold_gb": 0.5}
	DefaultPressureCPUThreshold   = 0.0
	DefaultPressureMemThresholdGB = 0.0
	DefaultPressureMaxSessions    = 0
)

// Mail defaults.
const (
	DefaultMailReplyReminderDelay = 30 * time.Second
)

// Polecat recovery defaults.
const (
	DefaultRecoveryMaxBeadRespawns       = 3
	DefaultRecoveryHeartbeatStartupGrace = 5 * time.Minute
)

// Container-gate pool defaults (gt-yihz).
const (
	DefaultContainerGateSlots           = 1
	DefaultContainerGateReservedForGate = 0
	DefaultContainerGateYieldToGate     = true
	DefaultContainerGateMaxGateYield    = 30 * time.Minute
)

// Dolt defaults.
const (
	// DefaultDoltCommitsPerDayWarn is the D3 target: a few hundred commits a
	// day per database once bd commits once per invocation (gt-8z769).
	DefaultDoltCommitsPerDayWarn = 500
)

// LoadOperationalConfig loads operational config from a town root.
// Returns a valid (possibly empty) config — never nil, never errors.
// Callers can use accessor methods that return defaults for nil sub-configs.
func LoadOperationalConfig(townRoot string) *OperationalConfig {
	settingsPath := filepath.Join(townRoot, "settings", "config.json")
	ts, err := LoadOrCreateTownSettings(settingsPath)
	if err != nil || ts == nil || ts.Operational == nil {
		return &OperationalConfig{}
	}
	return ts.Operational
}

// --- Accessor methods ---
// Each method reads from config with fallback to the compiled-in default.
// Nil-safe: works when OperationalConfig or any sub-struct is nil.

// GetContainerGateConfig returns the container-gate pool thresholds, never nil.
func (c *OperationalConfig) GetContainerGateConfig() *ContainerGateThresholds {
	if c != nil && c.ContainerGate != nil {
		return c.ContainerGate
	}
	return &ContainerGateThresholds{}
}

// SlotsV returns the configured or default container-gate pool size.
func (g *ContainerGateThresholds) SlotsV() int {
	if g != nil && g.Slots != nil && *g.Slots >= 1 {
		return *g.Slots
	}
	return DefaultContainerGateSlots
}

// ReservedForGateV returns the configured or default number of slots
// reserved for gate-class roles.
func (g *ContainerGateThresholds) ReservedForGateV() int {
	if g != nil && g.ReservedForGate != nil && *g.ReservedForGate >= 0 {
		return *g.ReservedForGate
	}
	return DefaultContainerGateReservedForGate
}

// YieldToGateV reports whether new non-gate suites wait while a gate holds a
// gate-reserved slot (default true).
func (g *ContainerGateThresholds) YieldToGateV() bool {
	if g != nil && g.YieldToGate != nil {
		return *g.YieldToGate
	}
	return DefaultContainerGateYieldToGate
}

// MaxGateYieldD returns the configured or default cap on one acquisition's
// total yield to running gates. An invalid or non-positive value falls back to
// the default: a zero cap would silently turn yielding off.
func (g *ContainerGateThresholds) MaxGateYieldD() time.Duration {
	if g != nil && g.MaxGateYield != "" {
		if d, err := time.ParseDuration(g.MaxGateYield); err == nil && d > 0 {
			return d
		}
	}
	return DefaultContainerGateMaxGateYield
}

// GetDoltConfig returns the Dolt thresholds, never nil.
func (c *OperationalConfig) GetDoltConfig() *DoltThresholds {
	if c != nil && c.Dolt != nil {
		return c.Dolt
	}
	return &DoltThresholds{}
}

// CommitsPerDayWarnV returns the configured or default per-database
// commits-per-day warning threshold. A non-positive value falls back to the
// default: a zero threshold would warn on every database.
func (d *DoltThresholds) CommitsPerDayWarnV() int {
	if d != nil && d.CommitsPerDayWarn != nil && *d.CommitsPerDayWarn > 0 {
		return *d.CommitsPerDayWarn
	}
	return DefaultDoltCommitsPerDayWarn
}

// GetSessionConfig returns the session thresholds, never nil.
func (c *OperationalConfig) GetSessionConfig() *SessionThresholds {
	if c != nil && c.Session != nil {
		return c.Session
	}
	return &SessionThresholds{}
}

// StartupNudgeVerifyDelayD returns the configured or default startup nudge verify delay.
func (s *SessionThresholds) StartupNudgeVerifyDelayD() time.Duration {
	if s != nil {
		return ParseDurationOrDefault(s.StartupNudgeVerifyDelay, DefaultStartupNudgeVerifyDelay)
	}
	return DefaultStartupNudgeVerifyDelay
}

// StartupNudgeMaxRetriesV returns the configured or default startup nudge max retries.
func (s *SessionThresholds) StartupNudgeMaxRetriesV() int {
	if s != nil && s.StartupNudgeMaxRetries != nil {
		return *s.StartupNudgeMaxRetries
	}
	return DefaultStartupNudgeMaxRetries
}

// --- Nudge accessors ---

// GetNudgeConfig returns the nudge thresholds, never nil.
func (c *OperationalConfig) GetNudgeConfig() *NudgeThresholds {
	if c != nil && c.Nudge != nil {
		return c.Nudge
	}
	return &NudgeThresholds{}
}

// MaxQueueDepthV returns the configured or default max queue depth.
func (n *NudgeThresholds) MaxQueueDepthV() int {
	if n != nil && n.MaxQueueDepth != nil {
		return *n.MaxQueueDepth
	}
	return DefaultNudgeMaxQueueDepth
}

// StaleClaimThresholdD returns the configured or default stale claim threshold.
func (n *NudgeThresholds) StaleClaimThresholdD() time.Duration {
	if n != nil {
		return ParseDurationOrDefault(n.StaleClaimThreshold, DefaultNudgeStaleClaimTimeout)
	}
	return DefaultNudgeStaleClaimTimeout
}

// MaxDeliveryAttemptsV returns the configured or default cap on requeue
// attempts per nudge. A value <= 0 means "use the default".
func (n *NudgeThresholds) MaxDeliveryAttemptsV() int {
	if n != nil && n.MaxDeliveryAttempts != nil && *n.MaxDeliveryAttempts > 0 {
		return *n.MaxDeliveryAttempts
	}
	return DefaultNudgeMaxDeliveryAttempts
}

// RequeueBackoffD returns the configured or default delay applied before a
// requeued nudge becomes eligible for delivery again. An explicit "0s" (or
// "0") disables the spacing, so retries run at the poll interval; an invalid
// or negative value falls back to the default.
func (n *NudgeThresholds) RequeueBackoffD() time.Duration {
	if n != nil && n.RequeueBackoff != "" {
		if d, err := time.ParseDuration(n.RequeueBackoff); err == nil && d >= 0 {
			return d
		}
	}
	return DefaultNudgeRequeueBackoff
}

// --- Daemon accessors ---

// GetDaemonConfig returns the daemon thresholds, never nil.
func (c *OperationalConfig) GetDaemonConfig() *DaemonThresholds {
	if c != nil && c.Daemon != nil {
		return c.Daemon
	}
	return &DaemonThresholds{}
}

// PolecatIdleSessionTimeoutD returns the configured or default polecat idle session timeout.
// Polecats that have been idle (no hooked work, heartbeat state=idle) longer than this
// threshold are auto-killed to prevent API slot burn. Default 15 minutes — long enough
// for polecats to run gt done after completing work, short enough to prevent hour-long burns.
func (d *DaemonThresholds) PolecatIdleSessionTimeoutD() time.Duration {
	if d != nil {
		return ParseDurationOrDefault(d.PolecatIdleSessionTimeout, DefaultPolecatIdleSessionTimeout)
	}
	return DefaultPolecatIdleSessionTimeout
}

// RecoveryHeartbeatIntervalD returns the configured or default recovery heartbeat interval.
func (d *DaemonThresholds) RecoveryHeartbeatIntervalD() time.Duration {
	if d != nil {
		return ParseDurationOrDefault(d.RecoveryHeartbeatInterval, DefaultRecoveryHeartbeatInterval)
	}
	return DefaultRecoveryHeartbeatInterval
}

// PressureCPUThresholdV returns the configured or default CPU pressure threshold (load per core).
func (d *DaemonThresholds) PressureCPUThresholdV() float64 {
	if d != nil && d.PressureCPUThreshold != nil {
		return *d.PressureCPUThreshold
	}
	return DefaultPressureCPUThreshold
}

// PressureMemThresholdGBV returns the configured or default memory pressure threshold in GB.
func (d *DaemonThresholds) PressureMemThresholdGBV() float64 {
	if d != nil && d.PressureMemThresholdGB != nil {
		return *d.PressureMemThresholdGB
	}
	return DefaultPressureMemThresholdGB
}

// PressureMaxSessionsV returns the configured or default max concurrent sessions (0 = unlimited).
func (d *DaemonThresholds) PressureMaxSessionsV() int {
	if d != nil && d.PressureMaxSessions != nil {
		return *d.PressureMaxSessions
	}
	return DefaultPressureMaxSessions
}

// --- Mail accessors ---

// GetMailConfig returns the mail thresholds, never nil.
func (c *OperationalConfig) GetMailConfig() *MailThresholds {
	if c != nil && c.Mail != nil {
		return c.Mail
	}
	return &MailThresholds{}
}

// ReplyReminderDelayD returns the configured or default reply reminder delay.
// A zero duration means reply reminders are disabled.
func (m *MailThresholds) ReplyReminderDelayD() time.Duration {
	if m != nil {
		return ParseDurationOrDefault(m.ReplyReminderDelay, DefaultMailReplyReminderDelay)
	}
	return DefaultMailReplyReminderDelay
}

// --- Polecat recovery accessors ---

// GetRecoveryConfig returns the polecat recovery thresholds, never nil.
func (c *OperationalConfig) GetRecoveryConfig() *RecoveryThresholds {
	if c != nil && c.Recovery != nil {
		return c.Recovery
	}
	return &RecoveryThresholds{}
}

// MaxBeadRespawnsV returns the configured or default max bead respawns.
func (rt *RecoveryThresholds) MaxBeadRespawnsV() int {
	if rt != nil && rt.MaxBeadRespawns != nil {
		return *rt.MaxBeadRespawns
	}
	return DefaultRecoveryMaxBeadRespawns
}

// HeartbeatStartupGraceD returns the configured or default heartbeat startup grace period.
// A live polecat with assigned work but no heartbeat file older than this is flagged
// for review as possibly stuck at startup (e.g., auth 401). (gt-uk7)
func (rt *RecoveryThresholds) HeartbeatStartupGraceD() time.Duration {
	if rt != nil {
		return ParseDurationOrDefault(rt.HeartbeatStartupGrace, DefaultRecoveryHeartbeatStartupGrace)
	}
	return DefaultRecoveryHeartbeatStartupGrace
}

// GetHealthSettings returns the health signal's settings block; nil when
// absent, which townhealth.Settings.Resolve reads as all defaults.
func (c *OperationalConfig) GetHealthSettings() *townhealth.Settings {
	if c == nil {
		return nil
	}
	return c.Health
}
