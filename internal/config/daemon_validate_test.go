package config

import (
	"strings"
	"testing"
)

// ValidateScheduledSlingEntry refuses each entry the patrol cannot run, with
// the patrol's own text (gt-yjzepk.4).
func TestValidateScheduledSlingEntry(t *testing.T) {
	t.Parallel()

	valid := ScheduledSlingEntry{
		Name: "deep-review", Rig: "gastown", Formula: "mol-deep-review",
		IntervalStr: "1h", Dispatch: ScheduledDispatchSpec,
	}
	if err := ValidateScheduledSlingEntry(valid); err != nil {
		t.Fatalf("ValidateScheduledSlingEntry(valid) = %v, want it accepted", err)
	}

	cases := []struct {
		name   string
		mutate func(*ScheduledSlingEntry)
		want   string
	}{
		{
			name:   "a name that is not a label",
			mutate: func(e *ScheduledSlingEntry) { e.Name = "Deep Review" },
			want:   `name "Deep Review" must match`,
		},
		{
			name:   "no rig or formula",
			mutate: func(e *ScheduledSlingEntry) { e.Formula = "" },
			want:   "rig and formula are required",
		},
		{
			name:   "a negative interval",
			mutate: func(e *ScheduledSlingEntry) { e.IntervalStr = "-1h" },
			want:   "must be a positive Go duration",
		},
		{
			name:   "a gate_timeout that is not a duration",
			mutate: func(e *ScheduledSlingEntry) { e.GateTimeoutStr = "soon" },
			want:   `gate_timeout "soon" must be a positive Go duration`,
		},
		{
			name:   "run_agent without the spec dispatch",
			mutate: func(e *ScheduledSlingEntry) { e.Dispatch, e.RunAgent = "", "claude" },
			want:   `run_agent is valid only with dispatch "spec"`,
		},
		{
			name:   "an agent on a shadow entry",
			mutate: func(e *ScheduledSlingEntry) { e.Dispatch, e.Agent = ScheduledDispatchShadow, "claude" },
			want:   "slings nothing",
		},
		{
			name:   "a dispatch the patrol does not know",
			mutate: func(e *ScheduledSlingEntry) { e.Dispatch = "sling" },
			want:   `dispatch "sling"`,
		},
		{
			name:   "a var that cannot be written as a key=value line",
			mutate: func(e *ScheduledSlingEntry) { e.Vars = map[string]string{"k": "a\nb=c"} },
			want:   "cannot be written as a key=value line",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := valid
			tc.mutate(&e)
			if err := ValidateScheduledSlingEntry(e); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateScheduledSlingEntry = %v, want %q", err, tc.want)
			}
		})
	}
}

// ValidateDaemonPatrolConfig accepts a document the daemon can run, and refuses
// each kind of value the daemon's schema or patrol validators reject, with the
// validator's own text (gt-yjzepk.4).
func TestValidateDaemonPatrolConfig(t *testing.T) {
	t.Parallel()

	valid := func() *DaemonPatrolConfig {
		return &DaemonPatrolConfig{
			Type:                  "daemon-patrol-config",
			Version:               1,
			ContainerNameSetting:  "gt-town-daemon",
			ComposeProjectSetting: "gt-town",
			ContainerOffSetting:   []string{"landing_worker"},
			Patrols: &PatrolsConfig{
				TierSweep: &TierSweepConfig{Runner: &TierSweepRunnerConfig{
					Backend: "kubernetes", CPUs: 8, Memory: 24, Jobs: 4,
					OutputRoot: "/tmp/sweep", Context: "ctx", Dind: true,
				}},
				DiskMaintenance: &DiskMaintenanceConfig{Mode: DiskMaintenanceModeDryRun},
				ScheduledSlings: &ScheduledSlingsConfig{Entries: []ScheduledSlingEntry{{
					Name: "deep-review", Rig: "gastown", Formula: "mol-deep-review",
					IntervalStr: "1h", Dispatch: ScheduledDispatchSpec,
				}}},
			},
		}
	}

	if err := ValidateDaemonPatrolConfig(valid()); err != nil {
		t.Fatalf("ValidateDaemonPatrolConfig(valid) = %v, want it accepted", err)
	}
	if err := ValidateDaemonPatrolConfig(nil); err != nil {
		t.Errorf("ValidateDaemonPatrolConfig(nil) = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*DaemonPatrolConfig)
		want   string
	}{
		{
			name:   "a container_name without a compose_project",
			mutate: func(c *DaemonPatrolConfig) { c.ComposeProjectSetting = "" },
			want:   "set both or neither",
		},
		{
			name: "a scheduled entry with a bad interval",
			mutate: func(c *DaemonPatrolConfig) {
				c.Patrols.ScheduledSlings.Entries[0].IntervalStr = "soon"
			},
			want: `scheduled_slings[deep-review]: interval "soon" must be a positive Go duration`,
		},
		{
			name:   "a container_off name the audit does not classify",
			mutate: func(c *DaemonPatrolConfig) { c.ContainerOffSetting = []string{"mystery-step"} },
			want:   "mystery-step: in daemon.container_off but unknown to the audit table",
		},
		{
			name: "a dind runner under its memory floor",
			mutate: func(c *DaemonPatrolConfig) {
				c.Patrols.TierSweep.Runner.Memory = 5
			},
			want: "memory = 5: want at least 6 GB with dind",
		},
		{
			name: "a disk maintenance percent below the range",
			mutate: func(c *DaemonPatrolConfig) {
				low := 20
				c.Patrols.DiskMaintenance.CacheTrimEarlyPercent = &low
			},
			want: "cache_trim_early_percent = 20: want a percent from 50 to 95",
		},
		{
			name: "an actions retention window below one day",
			mutate: func(c *DaemonPatrolConfig) {
				zero := 0
				c.Patrols.DiskMaintenance.ActionsRetention = &DiskMaintenanceActionsRetention{Enabled: true, Days: &zero}
			},
			want: "actions_retention.days = 0: want from 1 to 36500",
		},
		{
			name: "an actions retention keep count below one",
			mutate: func(c *DaemonPatrolConfig) {
				zero := 0
				c.Patrols.DiskMaintenance.ActionsRetention = &DiskMaintenanceActionsRetention{Enabled: true, KeepNewest: &zero}
			},
			want: "actions_retention.keep_newest = 0: want at least 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tc.mutate(c)
			if err := ValidateDaemonPatrolConfig(c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateDaemonPatrolConfig = %v, want %q", err, tc.want)
			}
		})
	}
}

// ValidateDaemonPatrolConfig refuses an update min_interval the update patrol
// would refuse at run time, naming the key and the allowed range, so the
// config write is refused and nothing is saved (gt-wv6e29).
func TestValidateDaemonPatrolConfigUpdateMinIntervalRange(t *testing.T) {
	t.Parallel()

	update := func(minInterval string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{
			Type:    "daemon-patrol-config",
			Version: 1,
			Patrols: &PatrolsConfig{Update: &UpdateConfig{Enabled: true, MinInterval: minInterval}},
		}
	}

	refused := []struct {
		name        string
		minInterval string
	}{
		{"below the 30 minute floor", "5m"},
		{"above the 24 hour ceiling", "25h"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDaemonPatrolConfig(update(tc.minInterval))
			if err == nil {
				t.Fatalf("ValidateDaemonPatrolConfig(min_interval %q) accepted, want refused", tc.minInterval)
			}
			for _, want := range []string{UpdateMinIntervalKey, "30m0s to 24h0m0s"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ValidateDaemonPatrolConfig(min_interval %q) = %q, want it to contain %q", tc.minInterval, err, want)
				}
			}
		})
	}

	accepted := []string{"30m", "3h", "24h"}
	for _, minInterval := range accepted {
		t.Run("accepts "+minInterval, func(t *testing.T) {
			t.Parallel()
			if err := ValidateDaemonPatrolConfig(update(minInterval)); err != nil {
				t.Fatalf("ValidateDaemonPatrolConfig(min_interval %q) = %v, want accepted", minInterval, err)
			}
		})
	}
}

// ValidateDaemonPatrolConfig passes an update block with no min_interval, since
// the default applies, and refuses a min_interval that is not a duration,
// naming the key (gt-wv6e29).
func TestValidateDaemonPatrolConfigUpdateMinIntervalUnset(t *testing.T) {
	t.Parallel()

	withUpdate := func(u *UpdateConfig) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{
			Type:    "daemon-patrol-config",
			Version: 1,
			Patrols: &PatrolsConfig{Update: u},
		}
	}

	accepted := []struct {
		name string
		doc  *DaemonPatrolConfig
	}{
		{"no update block", &DaemonPatrolConfig{Type: "daemon-patrol-config", Version: 1, Patrols: &PatrolsConfig{}}},
		{"update block with no min_interval", withUpdate(&UpdateConfig{Enabled: true})},
		{"update block with an empty min_interval", withUpdate(&UpdateConfig{Enabled: true, MinInterval: ""})},
		{"update block with a blank min_interval", withUpdate(&UpdateConfig{Enabled: true, MinInterval: "   "})},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateDaemonPatrolConfig(tc.doc); err != nil {
				t.Fatalf("ValidateDaemonPatrolConfig = %v, want accepted", err)
			}
		})
	}

	t.Run("a min_interval that is not a duration", func(t *testing.T) {
		t.Parallel()
		err := ValidateDaemonPatrolConfig(withUpdate(&UpdateConfig{Enabled: true, MinInterval: "soon"}))
		if err == nil || !strings.Contains(err.Error(), UpdateMinIntervalKey) {
			t.Fatalf("ValidateDaemonPatrolConfig(min_interval \"soon\") = %v, want refused naming %s", err, UpdateMinIntervalKey)
		}
	})
}
