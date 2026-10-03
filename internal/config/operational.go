package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// doltLogLevels are the log_level values Dolt's config.yaml accepts. A value
// outside this set can make Dolt reject or mis-parse the file it is written
// into — Dolt expands ${...} anywhere in that file, comments included (gt-it0zt).
var doltLogLevels = []string{"trace", "debug", "info", "warning", "error", "fatal"}

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

// The Dolt server tunable accessors below return the configured value and
// whether the setting is present, because the compiled-in defaults live in
// internal/doltserver (which imports this package, so it cannot work the other
// way). A present-but-empty value is meaningful where an accessor says so.

// WaitTimeoutSecSetting returns the idle-session timeout in seconds and whether it is
// configured. A negative setting disables the override and resolves to 0.
func (d *DoltThresholds) WaitTimeoutSecSetting() (int, bool) {
	if d == nil || d.WaitTimeoutSec == nil {
		return 0, false
	}
	if *d.WaitTimeoutSec < 0 {
		return 0, true
	}
	return *d.WaitTimeoutSec, true
}

// TimeZoneSetting returns the configured `time_zone` server variable and whether it
// is set. A set-but-empty value skips the post-start SET GLOBAL.
func (d *DoltThresholds) TimeZoneSetting() (string, bool) {
	if d == nil || d.TimeZone == nil {
		return "", false
	}
	return *d.TimeZone, true
}

// EventSchedulerSetting returns the configured event_scheduler value and whether it
// is set.
func (d *DoltThresholds) EventSchedulerSetting() (string, bool) {
	if d == nil || d.EventScheduler == nil {
		return "", false
	}
	return *d.EventScheduler, true
}

// StatsEnabledSetting returns the configured dolt_stats_enabled value and whether it
// is set.
func (d *DoltThresholds) StatsEnabledSetting() (string, bool) {
	if d == nil || d.StatsEnabled == nil {
		return "", false
	}
	return *d.StatsEnabled, true
}

// AutoGCSetting returns the configured auto_gc value and whether it is set.
func (d *DoltThresholds) AutoGCSetting() (string, bool) {
	if d == nil || d.AutoGC == nil {
		return "", false
	}
	return *d.AutoGC, true
}

// UserSetting returns the configured Dolt user and whether it is set. An empty
// setting is unset: the default user holds.
func (d *DoltThresholds) UserSetting() (string, bool) {
	if d == nil || d.User == nil || *d.User == "" {
		return "", false
	}
	return *d.User, true
}

// LogLevelSetting returns the configured Dolt log level and whether it is set;
// an empty or unrecognized value is unset, so the default level holds, and an
// unrecognized one earns one warning naming the setting and the levels Dolt
// accepts (gt-it0zt).
func (d *DoltThresholds) LogLevelSetting() (string, bool) {
	if d == nil || d.LogLevel == nil || *d.LogLevel == "" {
		return "", false
	}
	level, ok := normalizeDoltLogLevel(*d.LogLevel)
	if !ok {
		fmt.Fprintf(os.Stderr, "warning: operational.dolt.log_level=%q is not a valid level, using the default (allowed: %s)\n",
			*d.LogLevel, strings.Join(doltLogLevels, ", "))
		return "", false
	}
	return level, true
}

// normalizeDoltLogLevel returns s as one of doltLogLevels, lowercased, and
// whether it matched. The whole string must match: it is destined for a YAML
// file, so surrounding whitespace or any other character disqualifies it rather
// than being trimmed (gt-it0zt).
func normalizeDoltLogLevel(s string) (string, bool) {
	level := strings.ToLower(s)
	for _, l := range doltLogLevels {
		if level == l {
			return l, true
		}
	}
	return "", false
}

// PasswordSetting returns the configured Dolt SQL password value and whether
// it is set. The value is a ${VAR} reference into settings/daemon.env, or a
// literal; ResolveDoltPassword turns it into the password. An empty setting is
// unset: the server takes no password.
func (d *DoltThresholds) PasswordSetting() (string, bool) {
	if d == nil || d.Password == nil || *d.Password == "" {
		return "", false
	}
	return *d.Password, true
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
