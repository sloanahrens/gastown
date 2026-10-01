package townhealth

import (
	"fmt"
	"time"
)

// DefaultStaleAfter is how old the health file may be before a reader
// treats it as UNKNOWN: three default heartbeats and a margin.
const DefaultStaleAfter = 10 * time.Minute

// Settings is the operator config block for the health signal
// (settings/config.json operational.health). Every key is optional; an
// absent key keeps the compiled default. Durations are Go duration strings.
type Settings struct {
	// NotifyCommand is the operator's pager: the command the daemon runs
	// once for each crossing from green to red or unknown, with the
	// one-line signal appended as its last argument. It is a command line
	// split on spaces, not a shell string, so a pager that needs quoting,
	// a pipeline or a redirect is a script. Empty (the compiled default)
	// means the town announces nothing and the daemon files no notice bead
	// (gt-s3rec.3).
	NotifyCommand       string  `json:"notify_command,omitempty"`
	StaleAfter          string  `json:"stale_after,omitempty"`
	DoltSamples         int     `json:"dolt_samples,omitempty"`
	DoltLatencyDegraded string  `json:"dolt_latency_degraded,omitempty"`
	DoltLatencyRed      string  `json:"dolt_latency_red,omitempty"`
	ExecTaxDegraded     string  `json:"exec_tax_degraded,omitempty"`
	ExecTaxRed          string  `json:"exec_tax_red,omitempty"`
	HeartbeatDegraded   string  `json:"heartbeat_degraded,omitempty"`
	HeartbeatRed        string  `json:"heartbeat_red,omitempty"`
	TickDegradedFactor  float64 `json:"tick_degraded_factor,omitempty"`
	TickRedFactor       float64 `json:"tick_red_factor,omitempty"`
	LandingDegraded     string  `json:"landing_degraded,omitempty"`
	LandingRed          string  `json:"landing_red,omitempty"`
	EscalationDegraded  string  `json:"escalation_degraded,omitempty"`
	EscalationRed       string  `json:"escalation_red,omitempty"`
	SlotHolderDegraded  string  `json:"slot_holder_degraded,omitempty"`
	SlotHolderRed       string  `json:"slot_holder_red,omitempty"`
	BackupDegraded      string  `json:"backup_degraded,omitempty"`
	BackupRed           string  `json:"backup_red,omitempty"`
	NeedsHumanDegraded  string  `json:"needs_human_degraded,omitempty"`
	NeedsHumanRed       string  `json:"needs_human_red,omitempty"`
	SeatStallDegraded   string  `json:"seat_stall_degraded,omitempty"`
	SeatStallRed        string  `json:"seat_stall_red,omitempty"`
	SeatEvidence        string  `json:"seat_evidence,omitempty"`
	StewardErrorRate    float64 `json:"steward_error_rate,omitempty"`
	StewardMinJobs      int     `json:"steward_min_jobs,omitempty"`
}

// Resolve returns the thresholds and stale age s sets over the compiled
// defaults. A nil Settings is all defaults. A value that does not parse, or
// is negative, is an error naming its key: a broken threshold never
// silently becomes a default.
func (s *Settings) Resolve() (Thresholds, time.Duration, error) {
	th, stale := DefaultThresholds(), DefaultStaleAfter
	if s == nil {
		return th, stale, nil
	}
	durations := []struct {
		key string
		val string
		dst *time.Duration
	}{
		{"stale_after", s.StaleAfter, &stale},
		{"dolt_latency_degraded", s.DoltLatencyDegraded, &th.DoltLatency.Degraded},
		{"dolt_latency_red", s.DoltLatencyRed, &th.DoltLatency.Red},
		{"exec_tax_degraded", s.ExecTaxDegraded, &th.ExecTax.Degraded},
		{"exec_tax_red", s.ExecTaxRed, &th.ExecTax.Red},
		{"heartbeat_degraded", s.HeartbeatDegraded, &th.Heartbeat.Degraded},
		{"heartbeat_red", s.HeartbeatRed, &th.Heartbeat.Red},
		{"landing_degraded", s.LandingDegraded, &th.Landing.Degraded},
		{"landing_red", s.LandingRed, &th.Landing.Red},
		{"escalation_degraded", s.EscalationDegraded, &th.Escalation.Degraded},
		{"escalation_red", s.EscalationRed, &th.Escalation.Red},
		{"slot_holder_degraded", s.SlotHolderDegraded, &th.SlotHolder.Degraded},
		{"slot_holder_red", s.SlotHolderRed, &th.SlotHolder.Red},
		{"backup_degraded", s.BackupDegraded, &th.Backup.Degraded},
		{"backup_red", s.BackupRed, &th.Backup.Red},
		{"needs_human_degraded", s.NeedsHumanDegraded, &th.NeedsHuman.Degraded},
		{"needs_human_red", s.NeedsHumanRed, &th.NeedsHuman.Red},
		{"seat_stall_degraded", s.SeatStallDegraded, &th.SeatStall.Degraded},
		{"seat_stall_red", s.SeatStallRed, &th.SeatStall.Red},
		{"seat_evidence", s.SeatEvidence, &th.SeatEvidence},
	}
	for _, d := range durations {
		if d.val == "" {
			continue
		}
		v, err := time.ParseDuration(d.val)
		if err != nil || v < 0 {
			return th, stale, fmt.Errorf("operational.health.%s: invalid duration %q", d.key, d.val)
		}
		*d.dst = v
	}
	if s.DoltSamples < 0 || s.TickDegradedFactor < 0 || s.TickRedFactor < 0 {
		return th, stale, fmt.Errorf("operational.health: dolt_samples and tick factors must not be negative")
	}
	if s.StewardErrorRate < 0 || s.StewardErrorRate > 1 || s.StewardMinJobs < 0 {
		return th, stale, fmt.Errorf("operational.health: steward_error_rate must be between 0 and 1 and steward_min_jobs must not be negative")
	}
	if s.StewardErrorRate > 0 {
		th.StewardErrorRate = s.StewardErrorRate
	}
	if s.StewardMinJobs > 0 {
		th.StewardMinJobs = s.StewardMinJobs
	}
	if s.DoltSamples > 0 {
		th.DoltSamples = s.DoltSamples
	}
	if s.TickDegradedFactor > 0 {
		th.TickDegradedFactor = s.TickDegradedFactor
	}
	if s.TickRedFactor > 0 {
		th.TickRedFactor = s.TickRedFactor
	}
	if stale <= 0 {
		return th, stale, fmt.Errorf("operational.health.stale_after must be positive")
	}
	return th, stale, nil
}
