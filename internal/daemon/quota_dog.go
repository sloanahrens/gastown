package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

const (
	defaultQuotaDogInterval = 5 * time.Minute
	// quotaDogTimeout is the maximum time allowed for a single rotation cycle.
	quotaDogTimeout = 2 * time.Minute
	// defaultQuotaResumeInterval mirrors quota_dog's cadence — the resume
	// nudge is cheap (one scan, no keychain work) so there's no reason to
	// run it less often.
	defaultQuotaResumeInterval = 5 * time.Minute
)

// QuotaDogConfig holds configuration for the quota_dog patrol.
type QuotaDogConfig struct {
	// Enabled controls whether the quota dog runs.
	Enabled bool `json:"enabled"`

	// IntervalStr is how often to run, as a string (e.g., "5m").
	IntervalStr string `json:"interval,omitempty"`
}

// quotaDogInterval returns the configured interval, or the default (5m).
func quotaDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.QuotaDog != nil {
		if config.Patrols.QuotaDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.QuotaDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultQuotaDogInterval
}

// runQuotaDog executes a quota rotation cycle by shelling out to `gt quota rotate`.
// The daemon is a thin ticker — `gt quota rotate` handles scanning for rate-limited
// sessions, planning account assignments, and executing keychain swaps + session restarts.
//
// This follows the daemon's "dumb scheduler" principle: the daemon schedules,
// existing commands do the work. No LLM or molecule needed — pure mechanical rotation.
func (d *Daemon) runQuotaDog() {
	if !d.isPatrolActive("quota_dog") {
		return
	}

	d.logger.Printf("quota_dog: starting rotation cycle")

	ctx, cancel := context.WithTimeout(d.ctx, quotaDogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.gtPath, "quota", "rotate", "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Non-fatal: rotation failure shouldn't crash the daemon.
		// Common expected failures: <2 accounts, no rate-limited sessions.
		stderrStr := stderr.String()
		if stderrStr != "" {
			d.logger.Printf("quota_dog: rotation failed (non-fatal): %v: %s", err, stderrStr)
		} else {
			d.logger.Printf("quota_dog: rotation failed (non-fatal): %v", err)
		}
		return
	}

	outStr := stdout.String()
	if outStr != "" && outStr != "[]\n" && outStr != "[]" {
		d.logger.Printf("quota_dog: rotation result: %s", outStr)
	} else {
		d.logger.Printf("quota_dog: no rate-limited sessions detected")
	}
}

// quotaResumeInterval returns the configured quota_resume interval, or the
// default (5m).
func quotaResumeInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.QuotaResume != nil {
		if config.Patrols.QuotaResume.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.QuotaResume.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultQuotaResumeInterval
}

// runQuotaResume executes a resume cycle by shelling out to `gt quota
// resume`. Unlike quota_dog, this needs no account pool — it only nudges
// sessions whose own session-limit reset has already passed, which is why
// it runs on its own always-on ticker instead of being folded into
// runQuotaDog (gt-749e: on a town with < 2 accounts, quota_dog's rotate
// path never reaches the resume nudge because it exits before scanning).
func (d *Daemon) runQuotaResume() {
	if !d.isPatrolActive("quota_resume") {
		return
	}

	ctx, cancel := context.WithTimeout(d.ctx, quotaDogTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, d.gtPath, "quota", "resume", "--json") //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		if stderrStr != "" {
			d.logger.Printf("quota_resume: cycle failed (non-fatal): %v: %s", err, stderrStr)
		} else {
			d.logger.Printf("quota_resume: cycle failed (non-fatal): %v", err)
		}
		return
	}

	var report struct {
		Limited int `json:"limited"`
		Resumed []struct {
			Session string `json:"session"`
			Error   string `json:"error,omitempty"`
		} `json:"resumed"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		d.logger.Printf("quota_resume: could not parse output: %v: %s", err, stdout.String())
		return
	}

	d.logger.Printf("quota: %d limited, %d resumed", report.Limited, len(report.Resumed))
}
