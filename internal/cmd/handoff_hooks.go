package cmd

import (
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/style"
)

// restartPlan is how a handoff or cycle respawns a session's pane.
type restartPlan struct {
	// Command is what the pane runs (buildRestartCommand).
	Command string
	// Hooks is the settings sync the respawn runs first; nil for a session
	// without a role, which has no managed settings to sync.
	Hooks *respawnHooks
}

// respawnHooks is the managed settings sync a respawn runs before the new
// agent starts, as every first start does (gt-4k3fj.8.3): a handoff or
// mol-step cycle otherwise starts the successor on whatever settings the
// predecessor started with, and never reports hooks:present|absent for it.
type respawnHooks struct {
	TownRoot    string
	Actor       string
	Session     string
	Role        string
	SettingsDir string
	WorkDir     string

	// sync and report are runtime.SyncSessionSettings and
	// runtime.ReportHooks when nil.
	sync   func(settingsDir, workDir, role string) (runtime.HooksStatus, error)
	report func(townRoot, actor, session string, s runtime.HooksStatus)
}

// syncSettings syncs the plan's managed settings and reports hooks:present or
// hooks:absent for the session. Non-fatal: a respawn without the guards still
// beats a dead pane, and the absent event is what surfaces it.
func (p restartPlan) syncSettings() {
	h := p.Hooks
	if h == nil {
		return
	}
	sync, report := h.sync, h.report
	if sync == nil {
		sync = runtime.SyncSessionSettings
	}
	if report == nil {
		report = runtime.ReportHooks
	}
	status, err := sync(h.SettingsDir, h.WorkDir, h.Role)
	report(h.TownRoot, h.Actor, h.Session, status)
	if err != nil {
		style.PrintWarning("could not ensure settings for %s: %v", h.Session, err)
	}
}
