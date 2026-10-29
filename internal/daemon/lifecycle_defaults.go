package daemon

import (
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// DefaultLifecycleConfig returns a DaemonPatrolConfig with sensible defaults
// for the six-stage Dolt lifecycle (CREATE → LIVE → CLOSE → DECAY → COMPACT → FLATTEN).
//
// All patrols are enabled with conservative intervals:
//   - Wisp Reaper (DECAY): every 30m, delete closed wisps after 7d
//   - Compactor Dog (COMPACT): every 24h, threshold 2000 commits (escalates only)
//   - Checkpoint Dog: every 10m, auto-commit dirty polecat worktrees
//   - Doctor Dog (health): every 5m
//   - JSONL Git Backup: every 15m
//   - Scheduled Maintenance: window 03:00; nightly backup to ~/gt-backups/dolt (7 kept),
//     then dolt_gc --full weekly, or on old-gen growth >20%
//   - Disk Maintenance: declared with window 04:30 and mode dry_run, and
//     disabled: it prunes the registry, so a town turns it on itself
//   - Main Branch Test: every 30m, 10m timeout per rig
//   - Dolt Remotes: every 15m, pushes databases that have a configured remote
func DefaultLifecycleConfig() *DaemonPatrolConfig {
	scrub := true
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
				// the daemon patrol config rather than inferred from code. It matches the
				// mol-dog-reaper formula default; shortening it is what swept the
				// town's agent beads in gt-2qzr.
				StaleIssueAgeStr: "720h", // 30 days
			},
			CompactorDog: &CompactorDogConfig{
				Enabled:     true,
				IntervalStr: "24h",
				Threshold:   DefaultCompactorCommitThreshold,
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
			ScheduledMaintenance: &ScheduledMaintenanceConfig{
				Enabled: true,
				Window:  "03:00",
			},
			// Declared but switched off: the patrol prunes the town's registry,
			// which an install must not start doing on its own. The key is here
			// so an installed gt writes it once and the operator flips enabled
			// and mode together (gt-dl4nv.15).
			DiskMaintenance: &DiskMaintenanceConfig{
				Enabled:  false,
				Interval: agentconfig.DiskMaintenanceIntervalDaily,
				Window:   agentconfig.DiskMaintenanceDefaultWindow,
				Mode:     agentconfig.DiskMaintenanceModeDryRun,
			},
			Handler: &PatrolConfig{
				Enabled: true,
			},
		},
	}
}

// lifecyclePatrol is one entry of the lifecycle patrol set: the config key that
// names it, whether a PatrolsConfig carries it, and how to copy its default in.
// EnsureLifecycleDefaults and MissingLifecyclePatrols both walk lifecyclePatrols,
// so "configured" means the same thing to the daemon and to gt doctor.
type lifecyclePatrol struct {
	name  string
	isSet func(p *PatrolsConfig) bool
	fill  func(p, d *PatrolsConfig)
}

// lifecyclePatrols is the set of patrols EnsureLifecycleDefaults fills when nil.
// Add a patrol here and the doctor check reports it missing too.
//
// disk_maintenance joins the set switched off, so a town that predates it reads
// as missing one patrol in gt doctor until the next gt up fills the key in; the
// fill cannot turn it on, and an operator who never enables it keeps a patrol
// that does nothing.
var lifecyclePatrols = []lifecyclePatrol{
	{"wisp_reaper", func(p *PatrolsConfig) bool { return p.WispReaper != nil }, func(p, d *PatrolsConfig) { p.WispReaper = d.WispReaper }},
	{"compactor_dog", func(p *PatrolsConfig) bool { return p.CompactorDog != nil }, func(p, d *PatrolsConfig) { p.CompactorDog = d.CompactorDog }},
	{"checkpoint_dog", func(p *PatrolsConfig) bool { return p.CheckpointDog != nil }, func(p, d *PatrolsConfig) { p.CheckpointDog = d.CheckpointDog }},
	{"doctor_dog", func(p *PatrolsConfig) bool { return p.DoctorDog != nil }, func(p, d *PatrolsConfig) { p.DoctorDog = d.DoctorDog }},
	{"jsonl_git_backup", func(p *PatrolsConfig) bool { return p.JsonlGitBackup != nil }, func(p, d *PatrolsConfig) { p.JsonlGitBackup = d.JsonlGitBackup }},
	{"scheduled_maintenance", func(p *PatrolsConfig) bool { return p.ScheduledMaintenance != nil }, func(p, d *PatrolsConfig) { p.ScheduledMaintenance = d.ScheduledMaintenance }},
	{"disk_maintenance", func(p *PatrolsConfig) bool { return p.DiskMaintenance != nil }, func(p, d *PatrolsConfig) { p.DiskMaintenance = d.DiskMaintenance }},
	{"handler", func(p *PatrolsConfig) bool { return p.Handler != nil }, func(p, d *PatrolsConfig) { p.Handler = d.Handler }},
}

// MissingLifecyclePatrols returns the config keys of the lifecycle patrols p
// does not set: exactly the ones EnsureLifecycleDefaults would fill in.
func MissingLifecyclePatrols(p *PatrolsConfig) []string {
	var missing []string
	for _, lp := range lifecyclePatrols {
		if !lp.isSet(p) {
			missing = append(missing, lp.name)
		}
	}
	return missing
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

	for _, lp := range lifecyclePatrols {
		if lp.isSet(config.Patrols) {
			continue
		}
		lp.fill(config.Patrols, defaults.Patrols)
		changed = true
	}

	return changed
}

// EnsureLifecycleConfigFile loads the patrol config from disk (or creates a new
// one if it doesn't exist), applies lifecycle defaults for any unconfigured
// patrols, and saves the result. Returns nil on success.
//
// This is the top-level function called by gt init and gt up.
//
// A daemon patrol config that does not parse is returned as an error and never
// rewritten (gt-fcxe9.10): only an absent one is created from defaults.
func EnsureLifecycleConfigFile(townRoot string) error {
	config, err := ReadPatrolConfig(townRoot)
	if err != nil {
		return err
	}
	if config == nil {
		config = DefaultLifecycleConfig()
		return SavePatrolConfig(townRoot, config)
	}

	if EnsureLifecycleDefaults(config) {
		return SavePatrolConfig(townRoot, config)
	}

	return nil // Already configured, nothing to do
}
