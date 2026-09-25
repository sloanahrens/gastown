package daemon

import "time"

// DefaultLifecycleConfig returns a DaemonPatrolConfig with sensible defaults
// for the six-stage Dolt lifecycle (CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN).
//
// All patrols are enabled with conservative intervals:
//   - Wisp Reaper (DECAY): every 30m, delete closed wisps after 7d
//   - Compactor Dog (COMPACT): every 24h, threshold 2000 commits (escalates only)
//   - Checkpoint Dog: every 10m, auto-commit dirty polecat worktrees
//   - Doctor Dog (health): every 5m
//   - JSONL Git Backup: every 15m
//   - Dolt Filesystem Backup: every 15m
//   - Scheduled Maintenance (FLATTEN): daily at 03:00, threshold 1000
//   - Main Branch Test: every 30m, 10m timeout per rig
//   - Dolt Remotes: every 15m, pushes databases that have a configured remote
func DefaultLifecycleConfig() *DaemonPatrolConfig {
	threshold := 1000
	scrub := true
	skipWhenGateBusy := true
	return &DaemonPatrolConfig{
		Type:    "daemon-patrol-config",
		Version: 1,
		Patrols: &PatrolsConfig{
			WispReaper: &WispReaperConfig{
				Enabled:      true,
				IntervalStr:  "30m",
				MaxAgeStr:    "24h",
				DeleteAgeStr: "168h", // 7 days
				// Auto-close is stated explicitly so the threshold is visible in
				// daemon.json rather than inferred from code. It matches the
				// mol-dog-reaper formula default; shortening it is what swept the
				// town's agent beads in gt-2qzr.
				StaleIssueAgeStr: "720h", // 30 days
			},
			CompactorDog: &CompactorDogConfig{
				Enabled:     true,
				IntervalStr: "24h",
				Threshold:   defaultCompactorCommitThreshold,
			},
			CheckpointDog: &CheckpointDogConfig{
				Enabled:     true,
				IntervalStr: "10m",
			},
			DoctorDog: &DoctorDogConfig{
				Enabled:     true,
				IntervalStr: "5m",
			},
			JsonlGitBackup: &JsonlGitBackupConfig{
				Enabled:     true,
				IntervalStr: "15m",
				Scrub:       &scrub,
			},
			DoltBackup: &DoltBackupConfig{
				Enabled:     true,
				IntervalStr: "15m",
			},
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled:   true,
				Window:    "03:00",
				Interval:  "daily",
				Threshold: &threshold,
			},
			MainBranchTest: &MainBranchTestConfig{
				Enabled:          true,
				IntervalStr:      "60m",
				TimeoutStr:       "10m",
				SkipWhenGateBusy: &skipWhenGateBusy,
				// Written out rather than left nil so the bound on the yield
				// above is visible in a generated config, next to the knob
				// that turns it on (gt-lf2r).
				GateBusyStarveAfterStr: defaultGateBusyStarveAfter.String(),
			},
			// dolt_remotes defaults to disabled in IsPatrolEnabled (opt-in), so
			// a freshly generated daemon.json stays a no-op for towns whose
			// databases have no remote configured. A town that wants scheduled
			// remote pushes flips enabled to true.
			DoltRemotes: &DoltRemotesConfig{
				Enabled:  false,
				Interval: 15 * time.Minute,
			},
			Handler: &PatrolConfig{
				Enabled: true,
			},
		},
	}
}

// EnsureLifecycleDefaults populates missing patrol configuration with sensible
// defaults. It never overwrites existing user configuration — only fills in
// patrols that are nil (not yet configured).
//
// Returns true if any defaults were applied (caller should persist the config).
func EnsureLifecycleDefaults(config *DaemonPatrolConfig) bool {
	if config == nil {
		return false
	}

	defaults := DefaultLifecycleConfig()
	changed := false

	if config.Patrols == nil {
		config.Patrols = defaults.Patrols
		return true
	}

	p := config.Patrols
	d := defaults.Patrols

	if p.WispReaper == nil {
		p.WispReaper = d.WispReaper
		changed = true
	}
	if p.CompactorDog == nil {
		p.CompactorDog = d.CompactorDog
		changed = true
	}
	if p.CheckpointDog == nil {
		p.CheckpointDog = d.CheckpointDog
		changed = true
	}
	if p.DoctorDog == nil {
		p.DoctorDog = d.DoctorDog
		changed = true
	}
	if p.JsonlGitBackup == nil {
		p.JsonlGitBackup = d.JsonlGitBackup
		changed = true
	}
	if p.DoltBackup == nil {
		p.DoltBackup = d.DoltBackup
		changed = true
	}
	if p.ScheduledMaintenance == nil {
		p.ScheduledMaintenance = d.ScheduledMaintenance
		changed = true
	}
	if p.MainBranchTest == nil {
		p.MainBranchTest = d.MainBranchTest
		changed = true
	}
	if p.DoltRemotes == nil {
		p.DoltRemotes = d.DoltRemotes
		changed = true
	}
	if p.Handler == nil {
		p.Handler = d.Handler
		changed = true
	}

	return changed
}

// EnsureLifecycleConfigFile loads the patrol config from disk (or creates a new
// one if it doesn't exist), applies lifecycle defaults for any unconfigured
// patrols, and saves the result. Returns nil on success.
//
// This is the top-level function called by gt init and gt up.
func EnsureLifecycleConfigFile(townRoot string) error {
	config := LoadPatrolConfig(townRoot)
	if config == nil {
		config = DefaultLifecycleConfig()
		return SavePatrolConfig(townRoot, config)
	}

	if EnsureLifecycleDefaults(config) {
		return SavePatrolConfig(townRoot, config)
	}

	return nil // Already configured, nothing to do
}
