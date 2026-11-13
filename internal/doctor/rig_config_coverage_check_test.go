package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// fakeRigTown answers the check's reads from memory. A rig with no entry in
// settings reads as having no settings file.
type fakeRigTown struct {
	rigs        []string
	rigsErr     error
	parked      map[string]bool
	parkErr     map[string]error
	daemon      *config.DaemonPatrolConfig
	daemonErr   error
	settings    map[string]*config.RigSettings
	settingsErr map[string]error
}

func (f fakeRigTown) reader() rigConfigReader {
	return rigConfigReader{
		rigs: func(string) ([]string, error) { return f.rigs, f.rigsErr },
		park: func(_, rig string) (*config.RigParked, error) {
			if err := f.parkErr[rig]; err != nil {
				return nil, err
			}
			if f.parked[rig] {
				return &config.RigParked{By: "test", Reason: "test"}, nil
			}
			return nil, nil
		},
		daemon: func(string) (*config.DaemonPatrolConfig, error) {
			if f.daemonErr != nil {
				return nil, f.daemonErr
			}
			if f.daemon == nil {
				return nil, config.ErrNotFound
			}
			return f.daemon, nil
		},
		settings: func(rigPath string) (*config.RigSettings, error) {
			rig := filepath.Base(rigPath)
			if err := f.settingsErr[rig]; err != nil {
				return nil, err
			}
			s, ok := f.settings[rig]
			if !ok {
				return nil, fmt.Errorf("%w: %s", config.ErrNotFound, rigPath)
			}
			return s, nil
		},
	}
}

// settledRigs gives each rig settings with both merge-queue keys set.
func settledRigs(rigs ...string) map[string]*config.RigSettings {
	out := make(map[string]*config.RigSettings, len(rigs))
	for _, rig := range rigs {
		out[rig] = &config.RigSettings{MergeQueue: &config.MergeQueueConfig{
			Forgejo:         &config.ForgejoConfig{},
			PostLandCommand: "make test",
		}}
	}
	return out
}

func runRigConfigCoverage(t *testing.T, f fakeRigTown) *CheckResult {
	t.Helper()
	check := &RigConfigCoverageCheck{reader: f.reader()}
	return check.Run(&CheckContext{TownRoot: t.TempDir()})
}

func daemonWith(mutate func(*config.PatrolsConfig)) *config.DaemonPatrolConfig {
	p := &config.PatrolsConfig{}
	mutate(p)
	return &config.DaemonPatrolConfig{Patrols: p}
}

func TestRigConfigCoverage_RequiredLists(t *testing.T) {
	t.Parallel()
	const (
		landing   = "daemon.patrols.landing_worker.rigs"
		scan      = "daemon.patrols.patrol_scan.rigs"
		worktrees = "daemon.patrols.patrol_scan.worktree_cleanup.rigs"
	)
	cases := []struct {
		name   string
		daemon *config.DaemonPatrolConfig
		parked map[string]bool
		want   []string
	}{
		{
			name: "landing worker list lacks unparked hm",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.LandingWorker = &config.LandingWorkerConfig{Enabled: true, Rigs: []string{"gastown"}}
			}),
			want: []string{"rig hm: not in " + landing},
		},
		{
			name: "patrol scan list lacks unparked hm",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.PatrolScan = &config.PatrolScanConfig{Enabled: true, Rigs: []string{"gastown"}}
			}),
			want: []string{"rig hm: not in " + scan},
		},
		{
			name: "worktree cleanup list lacks unparked hm",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.PatrolScan = &config.PatrolScanConfig{Enabled: true, WorktreeCleanup: &config.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}}}
			}),
			want: []string{"rig hm: not in " + worktrees},
		},
		{
			name: "empty worktree cleanup list covers only the default rig",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.PatrolScan = &config.PatrolScanConfig{Enabled: true, WorktreeCleanup: &config.WorktreeCleanupConfig{Enabled: true}}
			}),
			want: []string{"rig hm: not in " + worktrees},
		},
		{
			name: "empty landing worker list covers every rig",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.LandingWorker = &config.LandingWorkerConfig{Enabled: true}
			}),
		},
		{
			name: "parked rig missing from every list is not reported",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.LandingWorker = &config.LandingWorkerConfig{Enabled: true, Rigs: []string{"gastown"}}
				p.PatrolScan = &config.PatrolScanConfig{Enabled: true, Rigs: []string{"gastown"}, WorktreeCleanup: &config.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown"}}}
			}),
			parked: map[string]bool{"hm": true},
		},
		{
			name: "every list names hm",
			daemon: daemonWith(func(p *config.PatrolsConfig) {
				p.LandingWorker = &config.LandingWorkerConfig{Enabled: true, Rigs: []string{"gastown", "hm"}}
				p.PatrolScan = &config.PatrolScanConfig{Enabled: true, Rigs: []string{"gastown", "hm"}, WorktreeCleanup: &config.WorktreeCleanupConfig{Enabled: true, Rigs: []string{"gastown", "hm"}}}
			}),
		},
		{
			name:   "absent blocks are not checked",
			daemon: daemonWith(func(*config.PatrolsConfig) {}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := runRigConfigCoverage(t, fakeRigTown{
				rigs:     []string{"gastown", "hm"},
				parked:   tc.parked,
				daemon:   tc.daemon,
				settings: settledRigs("gastown", "hm"),
			})
			wantStatus := StatusOK
			if len(tc.want) > 0 {
				wantStatus = StatusError
			}
			if res.Status != wantStatus {
				t.Errorf("status = %s, want %s: %s", res.Status, wantStatus, res.Message)
			}
			if !slices.Equal(res.Details, tc.want) {
				t.Errorf("details = %q, want %q", res.Details, tc.want)
			}
		})
	}
}

func TestRigConfigCoverage_Settings(t *testing.T) {
	t.Parallel()
	const (
		noForgejo  = "rig hm: settings/config.json lacks merge_queue.forgejo"
		noPostLand = "rig hm: settings/config.json lacks merge_queue.post_land_command"
	)
	cases := []struct {
		name     string
		settings *config.RigSettings
		absent   bool
		readErr  error
		want     []string
	}{
		{name: "no settings file lacks both keys", absent: true, want: []string{noForgejo, noPostLand}},
		{
			name:     "forgejo set, post land command missing",
			settings: &config.RigSettings{MergeQueue: &config.MergeQueueConfig{Forgejo: &config.ForgejoConfig{}}},
			want:     []string{noPostLand},
		},
		{
			name:     "post land command set, forgejo missing",
			settings: &config.RigSettings{MergeQueue: &config.MergeQueueConfig{PostLandCommand: "make test"}},
			want:     []string{noForgejo},
		},
		{
			name:     "both keys set",
			settings: &config.RigSettings{MergeQueue: &config.MergeQueueConfig{Forgejo: &config.ForgejoConfig{}, PostLandCommand: "make test"}},
		},
		{
			name:    "unreadable settings file is one problem, the keys are not guessed",
			readErr: errors.New("settings unreadable"),
			want:    []string{"rig hm: settings/config.json cannot be read, merge_queue keys not checked: settings unreadable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := fakeRigTown{rigs: []string{"hm"}}
			switch {
			case tc.readErr != nil:
				f.settingsErr = map[string]error{"hm": tc.readErr}
			case !tc.absent:
				f.settings = map[string]*config.RigSettings{"hm": tc.settings}
			}
			res := runRigConfigCoverage(t, f)
			wantStatus := StatusOK
			if len(tc.want) > 0 {
				wantStatus = StatusError
			}
			if res.Status != wantStatus {
				t.Errorf("status = %s, want %s: %s", res.Status, wantStatus, res.Message)
			}
			if !slices.Equal(res.Details, tc.want) {
				t.Errorf("details = %q, want %q", res.Details, tc.want)
			}
		})
	}
}

// TestRigConfigCoverage_UnreadableInputsWarn: an input the check cannot read
// is a warning naming it, never a clean result for the rigs it would cover.
func TestRigConfigCoverage_UnreadableInputsWarn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		fake       fakeRigTown
		wantDetail string
	}{
		{
			name:       "registry unreadable",
			fake:       fakeRigTown{rigsErr: errors.New("registry boom")},
			wantDetail: "registry boom",
		},
		{
			name:       "registry name is not a directory name, never joined into a path",
			fake:       fakeRigTown{rigs: []string{"../escape"}},
			wantDetail: `registry names "../escape", which is not a rig directory name; not checked`,
		},
		{
			name:       "daemon config unreadable",
			fake:       fakeRigTown{rigs: []string{"hm"}, daemonErr: errors.New("daemon boom")},
			wantDetail: "daemon boom",
		},
		{
			name: "park state unreadable, rig not counted as covered",
			fake: fakeRigTown{
				rigs:     []string{"hm"},
				parkErr:  map[string]error{"hm": errors.New("park boom")},
				daemon:   daemonWith(func(p *config.PatrolsConfig) { p.LandingWorker = &config.LandingWorkerConfig{Enabled: true} }),
				settings: settledRigs("hm"),
			},
			wantDetail: "rig hm: park state unreadable, not checked: park boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := runRigConfigCoverage(t, tc.fake)
			if res.Status != StatusWarning {
				t.Errorf("status = %s, want Warning: %s", res.Status, res.Message)
			}
			if !slices.ContainsFunc(res.Details, func(d string) bool { return strings.Contains(d, tc.wantDetail) }) {
				t.Errorf("details = %q, want a line containing %q", res.Details, tc.wantDetail)
			}
		})
	}
}

// TestRigConfigCoverage_MalformedRegistryWarns drives the real registry read:
// a mayor/rigs.json that does not decode is a warning, not an empty town.
func TestRigConfigCoverage_MalformedRegistryWarns(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"rigs":`), 0o644); err != nil {
		t.Fatal(err)
	}
	check := &RigConfigCoverageCheck{reader: rigConfigReader{
		park: func(string, string) (*config.RigParked, error) { return nil, nil },
	}}
	res := check.Run(&CheckContext{TownRoot: town})
	if res.Status != StatusWarning {
		t.Fatalf("status = %s, want Warning: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "cannot read the rig registry") {
		t.Errorf("message = %q, want the registry named", res.Message)
	}
}

// TestRigConfigCoverage_InfoNeverFails: a list that omits an unparked rig is
// an info note. It is listed, and the check still passes.
func TestRigConfigCoverage_InfoNeverFails(t *testing.T) {
	t.Parallel()
	daemon := daemonWith(func(p *config.PatrolsConfig) {
		p.Handler = &config.PatrolConfig{Enabled: true, Rigs: []string{"gastown"}}
		p.TierSweep = &config.TierSweepConfig{Enabled: true, Rigs: []string{"gastown"}}
	})
	res := runRigConfigCoverage(t, fakeRigTown{
		rigs:     []string{"gastown", "hm"},
		daemon:   daemon,
		settings: settledRigs("gastown", "hm"),
	})
	if res.Status != StatusOK {
		t.Fatalf("status = %s, want OK (info never fails): %s", res.Status, res.Message)
	}
	want := []string{
		"info: rig hm: not in daemon.patrols.handler.rigs",
		"info: rig hm: not in daemon.patrols.tier_sweep.rigs",
	}
	if !slices.Equal(res.Details, want) {
		t.Errorf("details = %q, want %q", res.Details, want)
	}
}

// TestRigConfigCoverage_CredentialNeverPrinted reads real files through the
// default readers. The planted values live in settings and must reach no
// part of the result.
func TestRigConfigCoverage_CredentialNeverPrinted(t *testing.T) {
	t.Parallel()
	const (
		secretBots      = "PLANTED-SECRET-bots-7f3a"
		secretCommand   = "PLANTED-SECRET-command-91cc"
		secretTruncated = "PLANTED-SECRET-truncated-e0d2"
	)
	town := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mayor/rigs.json", `{"version":1,"rigs":{"gastown":{"git_url":"x"},"hm":{"git_url":"x"},"lean":{"git_url":"x"},"bad":{"git_url":"x"}}}`)
	write("mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"patrols":{"landing_worker":{"enabled":true,"rigs":["gastown"]}}}`)
	// hm: its forgejo block carries a bot token; it is missing from the
	// landing worker list and passes the settings keys.
	write("hm/settings/config.json", `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"bots":{"landing":"`+secretBots+`"}},"post_land_command":"make test"}}`)
	// lean: the token sits in the post-land command; forgejo is missing.
	write("lean/settings/config.json", `{"type":"rig-settings","version":1,"merge_queue":{"post_land_command":"make test --token=`+secretCommand+`"}}`)
	// bad: truncated JSON holding a token; the file cannot be read.
	write("bad/settings/config.json", `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"bots":{"landing":"`+secretTruncated+`"}`)

	check := &RigConfigCoverageCheck{reader: rigConfigReader{
		park: func(string, string) (*config.RigParked, error) { return nil, nil },
	}}
	res := check.Run(&CheckContext{TownRoot: town})

	if res.Status != StatusError {
		t.Fatalf("status = %s, want Error: %s %q", res.Status, res.Message, res.Details)
	}
	// The real reads ran: the daemon list and the settings keys are named.
	joined := strings.Join(res.Details, "\n")
	for _, want := range []string{
		"rig hm: not in daemon.patrols.landing_worker.rigs",
		"rig lean: settings/config.json lacks merge_queue.forgejo",
		"rig bad: settings/config.json cannot be read",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("details missing %q:\n%s", want, joined)
		}
	}
	all := strings.Join([]string{res.Message, res.FixHint, joined}, "\n")
	for _, secret := range []string{secretBots, secretCommand, secretTruncated} {
		if strings.Contains(all, secret) {
			t.Errorf("result prints planted credential %q:\n%s", secret, all)
		}
	}
}

// TestRigConfigCoverage_RealParkState leaves the park read at its default
// townconfig.ParkState and drives a real town tree: a registry park record
// keeps an omitted rig out of the findings, and the same rig without one is
// reported.
func TestRigConfigCoverage_RealParkState(t *testing.T) {
	t.Parallel()
	const parkedEntry = `"hm":{"git_url":"x","parked":{"since":"2026-09-01T00:00:00Z","by":"operator","reason":"test"}}`
	cases := []struct {
		name     string
		hm       string
		unparked int
		want     []string
	}{
		{name: "registry park record skips the rig", hm: parkedEntry, unparked: 1},
		{
			name:     "no park record reports the rig",
			hm:       `"hm":{"git_url":"x"}`,
			unparked: 2,
			want:     []string{"rig hm: not in daemon.patrols.landing_worker.rigs"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			write := func(rel, body string) {
				t.Helper()
				path := filepath.Join(town, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("mayor/town.json", `{"type":"town","version":2,"name":"gt"}`)
			write("mayor/rigs.json", `{"version":1,"rigs":{"gastown":{"git_url":"x"},`+tc.hm+`}}`)
			write("mayor/daemon.json", `{"type":"daemon-patrol-config","version":1,"patrols":{"landing_worker":{"enabled":true,"rigs":["gastown"]}}}`)
			const settings = `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{},"post_land_command":"make test"}}`
			write("gastown/settings/config.json", settings)
			write("hm/settings/config.json", settings)

			res := (&RigConfigCoverageCheck{}).Run(&CheckContext{TownRoot: town})
			wantStatus := StatusOK
			if len(tc.want) > 0 {
				wantStatus = StatusError
			}
			if res.Status != wantStatus {
				t.Errorf("status = %s, want %s: %s", res.Status, wantStatus, res.Message)
			}
			if !slices.Equal(res.Details, tc.want) {
				t.Errorf("details = %q, want %q", res.Details, tc.want)
			}
			if want := fmt.Sprintf("All %d unparked rig(s)", tc.unparked); wantStatus == StatusOK && !strings.Contains(res.Message, want) {
				t.Errorf("message = %q, want the count %q", res.Message, want)
			}
		})
	}
}

// TestRigConfigCoverage_InfoLists covers every other list of rig names the
// check reports as info: the scheduled slings' rigs, the retired
// main_branch_test block, and the patrol lists found by their block type.
func TestRigConfigCoverage_InfoLists(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*config.PatrolsConfig)
		want   []string
	}{
		{
			name: "scheduled sling entry names its rig",
			mutate: func(p *config.PatrolsConfig) {
				p.ScheduledSlings = &config.ScheduledSlingsConfig{Enabled: true, Entries: []config.ScheduledSlingEntry{
					{Name: "audit", Rig: "gastown", Formula: "mol-audit", IntervalStr: "24h"},
				}}
			},
			want: []string{"info: rig hm: not in daemon.patrols.scheduled_slings.entries[].rig"},
		},
		{
			name: "retired main_branch_test block still names its rigs",
			mutate: func(p *config.PatrolsConfig) {
				p.MainBranchTest = json.RawMessage(`{"enabled":true,"rigs":["gastown"]}`)
			},
			want: []string{"info: rig hm: not in daemon.patrols.main_branch_test.rigs"},
		},
		{
			name: "main_branch_test block that does not decode names no rig",
			mutate: func(p *config.PatrolsConfig) {
				p.MainBranchTest = json.RawMessage(`{"rigs":"gastown"}`)
			},
		},
		{
			name: "patrol found by its block type",
			mutate: func(p *config.PatrolsConfig) {
				p.RebuildGT = &config.PatrolConfig{Enabled: true, Rigs: []string{"gastown"}}
			},
			want: []string{"info: rig hm: not in daemon.patrols.rebuild_gt.rigs"},
		},
		{
			name: "patrol block with no rigs names no rig",
			mutate: func(p *config.PatrolsConfig) {
				p.ResourceSampler = &config.PatrolConfig{Enabled: true}
			},
		},
		{
			name: "block that is not a patrol config contributes no list",
			mutate: func(p *config.PatrolsConfig) {
				p.DoltServer = &config.DoltServerConfig{Enabled: true}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := runRigConfigCoverage(t, fakeRigTown{
				rigs:     []string{"gastown", "hm"},
				daemon:   daemonWith(tc.mutate),
				settings: settledRigs("gastown", "hm"),
			})
			if res.Status != StatusOK {
				t.Fatalf("status = %s, want OK (info never fails): %s %q", res.Status, res.Message, res.Details)
			}
			if !slices.Equal(res.Details, tc.want) {
				t.Errorf("details = %q, want %q", res.Details, tc.want)
			}
		})
	}
}
