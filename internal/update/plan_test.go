package update

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeProbes is a town whose state each test sets directly.
type fakeProbes struct {
	installed, main string
	commits, events int
	backup          bool
	gcDue           bool
	gcReason        string
	published       bool
	queriedSHA      string

	errInstalled error
	errMain      error
	errArchive   error
	errBackup    error
	errGC        error
	errImage     error
}

func (p *fakeProbes) InstalledSHA(context.Context) (string, error) {
	if p.errInstalled != nil {
		return "", p.errInstalled
	}
	return p.installed, nil
}

func (p *fakeProbes) MainSHA(context.Context) (string, error) {
	if p.errMain != nil {
		return "", p.errMain
	}
	return p.main, nil
}

func (p *fakeProbes) ArchiveBehind(context.Context) (int, int, error) {
	if p.errArchive != nil {
		return 0, 0, p.errArchive
	}
	return p.commits, p.events, nil
}

func (p *fakeProbes) BackupToday(context.Context) (bool, error) {
	if p.errBackup != nil {
		return false, p.errBackup
	}
	return p.backup, nil
}

func (p *fakeProbes) GCDue(context.Context) (bool, string, error) {
	if p.errGC != nil {
		return false, "", p.errGC
	}
	return p.gcDue, p.gcReason, nil
}

func (p *fakeProbes) ImagePublished(_ context.Context, sha string) (bool, error) {
	p.queriedSHA = sha
	if p.errImage != nil {
		return false, p.errImage
	}
	return p.published, nil
}

// planFor builds a plan and fails the test on an error.
func planFor(t *testing.T, p Probes, o PlanOptions) []StepRecord {
	t.Helper()
	steps, _, err := BuildPlan(context.Background(), p, o)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return steps
}

// stepNamed returns the record for name and fails the test when the plan has
// none.
func stepNamed(t *testing.T, steps []StepRecord, name StepName) StepRecord {
	t.Helper()
	i := slices.IndexFunc(steps, func(s StepRecord) bool { return s.Name == name })
	if i < 0 {
		t.Fatalf("plan has no %s step", name)
	}
	return steps[i]
}

func TestPlanInstallWhenMainMoved(t *testing.T) {
	t.Parallel()
	const installed = "1111111111111111111111111111111111111111"
	const mainSHA = "2222222222222222222222222222222222222222"
	p := &fakeProbes{installed: installed, main: mainSHA, published: true, backup: true}

	steps, target, err := BuildPlan(context.Background(), p, PlanOptions{})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if target.SHA != mainSHA || target.InstalledSHA != installed {
		t.Errorf("target = %+v, want SHA %q and InstalledSHA %q", target, mainSHA, installed)
	}
	if p.queriedSHA != mainSHA {
		t.Errorf("ImagePublished asked about %q, want main's sha %q", p.queriedSHA, mainSHA)
	}

	wantOrder := []StepName{StepDrain, StepMaintenance, StepInstall, StepRestart, StepVerify, StepLift}
	if len(steps) != len(wantOrder) {
		t.Fatalf("plan has %d steps, want %d", len(steps), len(wantOrder))
	}
	for i, name := range wantOrder {
		if steps[i].Name != name {
			t.Errorf("step %d is %s, want %s", i, steps[i].Name, name)
		}
	}
	for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
		if s := stepNamed(t, steps, name); !s.WillRun {
			t.Errorf("%s WillRun = false, want true (reason %q)", name, s.Reason)
		}
	}

	reason := stepNamed(t, steps, StepInstall).Reason
	for _, sha := range []string{"11111111", "22222222"} {
		if !strings.Contains(reason, sha) {
			t.Errorf("install reason %q does not contain %q", reason, sha)
		}
	}
}

func TestPlanNothingToDo(t *testing.T) {
	t.Parallel()
	p := &fakeProbes{installed: "abc123", main: "abc123", published: true, backup: true}

	steps := planFor(t, p, PlanOptions{})
	if len(steps) != 6 {
		t.Fatalf("plan has %d steps, want 6", len(steps))
	}
	for _, s := range steps {
		if s.WillRun {
			t.Errorf("%s WillRun = true, want false", s.Name)
		}
		if s.Reason == "" {
			t.Errorf("%s has no reason", s.Name)
		}
	}
	if got := stepNamed(t, steps, StepInstall).Reason; got != "installed build equals main" {
		t.Errorf("install reason = %q, want %q", got, "installed build equals main")
	}
	if got := stepNamed(t, steps, StepMaintenance).Reason; got != "nothing is due" {
		t.Errorf("maintenance reason = %q, want %q", got, "nothing is due")
	}
	for _, name := range []StepName{StepDrain, StepRestart, StepVerify, StepLift} {
		if got := stepNamed(t, steps, name).Reason; got != "no install is planned" {
			t.Errorf("%s reason = %q, want %q", name, got, "no install is planned")
		}
	}
}

func TestPlanInstallForcedOrImageUnpublished(t *testing.T) {
	t.Parallel()
	const sha = "abc123"
	cases := []struct {
		name         string
		options      PlanOptions
		configure    func(*fakeProbes)
		wantContains string
	}{
		{
			name:         "forced",
			options:      PlanOptions{Force: true},
			configure:    func(*fakeProbes) {},
			wantContains: "forced",
		},
		{
			name:         "image unpublished",
			options:      PlanOptions{},
			configure:    func(p *fakeProbes) { p.published = false },
			wantContains: "not published",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProbes{installed: sha, main: sha, published: true, backup: true}
			tc.configure(p)

			install := stepNamed(t, planFor(t, p, tc.options), StepInstall)
			if !install.WillRun {
				t.Fatalf("install WillRun = false, want true (reason %q)", install.Reason)
			}
			if !strings.Contains(install.Reason, tc.wantContains) {
				t.Errorf("install reason %q does not contain %q", install.Reason, tc.wantContains)
			}
		})
	}
}

func TestPlanMaintenanceReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		configure    func(*fakeProbes)
		wantContains string
		wantExact    string
		wantNot      []string
	}{
		{
			name:         "commits behind",
			configure:    func(p *fakeProbes) { p.commits = 3 },
			wantContains: "archive",
			wantNot:      []string{"backup", "gc"},
		},
		{
			name:         "events behind",
			configure:    func(p *fakeProbes) { p.events = 2 },
			wantContains: "archive",
			wantNot:      []string{"backup", "gc"},
		},
		{
			name:         "backup missing",
			configure:    func(p *fakeProbes) { p.backup = false },
			wantContains: "backup",
			wantNot:      []string{"archive", "gc"},
		},
		{
			name:         "gc due",
			configure:    func(p *fakeProbes) { p.gcDue = true; p.gcReason = "the watermark is a day old" },
			wantContains: "gc",
			wantNot:      []string{"archive", "backup"},
		},
		{
			name:         "gc due without a reason",
			configure:    func(p *fakeProbes) { p.gcDue = true },
			wantContains: "a gc is due",
			wantExact:    "a gc is due",
			wantNot:      []string{"archive", "backup"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProbes{installed: "abc123", main: "abc123", published: true, backup: true}
			tc.configure(p)

			maintenance := stepNamed(t, planFor(t, p, PlanOptions{}), StepMaintenance)
			if !maintenance.WillRun {
				t.Fatalf("maintenance WillRun = false, want true (reason %q)", maintenance.Reason)
			}
			if !strings.Contains(maintenance.Reason, tc.wantContains) {
				t.Errorf("maintenance reason %q does not name %q", maintenance.Reason, tc.wantContains)
			}
			if tc.wantExact != "" && maintenance.Reason != tc.wantExact {
				t.Errorf("maintenance reason = %q, want %q", maintenance.Reason, tc.wantExact)
			}
			for _, other := range tc.wantNot {
				if strings.Contains(maintenance.Reason, other) {
					t.Errorf("maintenance reason %q names %q, which did not trigger", maintenance.Reason, other)
				}
			}
		})
	}
}

func TestPlanOnlyAndSkip(t *testing.T) {
	t.Parallel()
	// moved is a town whose installed build differs from main, so the install
	// chain is planned before Only and Skip are applied.
	moved := func() *fakeProbes {
		return &fakeProbes{installed: "11111111aaaa", main: "22222222bbbb", published: true, backup: true}
	}

	t.Run("only maintenance", func(t *testing.T) {
		t.Parallel()
		steps := planFor(t, moved(), PlanOptions{Only: map[StepName]bool{StepMaintenance: true}})
		for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
			s := stepNamed(t, steps, name)
			if s.WillRun {
				t.Errorf("%s WillRun = true, want false", name)
			}
			if s.Reason != "not selected by --only" {
				t.Errorf("%s reason = %q, want %q", name, s.Reason, "not selected by --only")
			}
		}
	})

	t.Run("only install", func(t *testing.T) {
		t.Parallel()
		steps := planFor(t, moved(), PlanOptions{Only: map[StepName]bool{StepInstall: true}})
		// Only keeps exactly the named steps; the install chain is not implied.
		if install := stepNamed(t, steps, StepInstall); !install.WillRun {
			t.Errorf("install WillRun = false, want true")
		}
		for _, name := range []StepName{StepDrain, StepMaintenance, StepRestart, StepVerify, StepLift} {
			s := stepNamed(t, steps, name)
			if s.WillRun {
				t.Errorf("%s WillRun = true, want false", name)
			}
			if s.Reason != "not selected by --only" {
				t.Errorf("%s reason = %q, want %q", name, s.Reason, "not selected by --only")
			}
		}
	})

	t.Run("skip install", func(t *testing.T) {
		t.Parallel()
		steps := planFor(t, moved(), PlanOptions{Skip: map[StepName]bool{StepInstall: true}})
		// drain and lift follow install, so they cannot run once it is skipped.
		for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
			s := stepNamed(t, steps, name)
			if s.WillRun {
				t.Errorf("%s WillRun = true, want false", name)
			}
			if s.Reason != "skipped by --skip" {
				t.Errorf("%s reason = %q, want %q", name, s.Reason, "skipped by --skip")
			}
		}
	})

	t.Run("skip drain", func(t *testing.T) {
		t.Parallel()
		steps := planFor(t, moved(), PlanOptions{Skip: map[StepName]bool{StepDrain: true}})
		drain := stepNamed(t, steps, StepDrain)
		if drain.WillRun {
			t.Errorf("drain WillRun = true, want false")
		}
		if drain.Reason != "skipped by --skip" {
			t.Errorf("drain reason = %q, want %q", drain.Reason, "skipped by --skip")
		}
		if install := stepNamed(t, steps, StepInstall); !install.WillRun {
			t.Errorf("install WillRun = false, want true")
		}
		lift := stepNamed(t, steps, StepLift)
		if lift.WillRun {
			t.Errorf("lift WillRun = true, want false when its drain is skipped")
		}
		if lift.Reason != "skipped by --skip" {
			t.Errorf("lift reason = %q, want %q", lift.Reason, "skipped by --skip")
		}
	})
}

func TestPlanOnlyFalseValueIgnored(t *testing.T) {
	t.Parallel()
	// A false entry selects nothing, so it must not turn on the Only filter.
	p := &fakeProbes{installed: "11111111aaaa", main: "22222222bbbb", published: true, backup: true}

	steps := planFor(t, p, PlanOptions{Only: map[StepName]bool{StepInstall: false}})
	for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
		if s := stepNamed(t, steps, name); !s.WillRun {
			t.Errorf("%s WillRun = false, want true: a false Only entry selects nothing", name)
		}
	}
	maintenance := stepNamed(t, steps, StepMaintenance)
	if maintenance.WillRun {
		t.Errorf("maintenance WillRun = true, want false (nothing is due)")
	}
	if maintenance.Reason != "nothing is due" {
		t.Errorf("maintenance reason = %q, want %q", maintenance.Reason, "nothing is due")
	}
}

func TestPlanSkipFalseValueIgnored(t *testing.T) {
	t.Parallel()
	// A false Skip entry skips nothing, so the install chain still runs.
	p := &fakeProbes{installed: "11111111aaaa", main: "22222222bbbb", published: true, backup: true}

	steps := planFor(t, p, PlanOptions{Skip: map[StepName]bool{StepInstall: false}})
	for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
		if s := stepNamed(t, steps, name); !s.WillRun {
			t.Errorf("%s WillRun = false, want true: a false Skip entry skips nothing (reason %q)", name, s.Reason)
		}
	}
}

func TestPlanEmptyShaIsAnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		probes *fakeProbes
		want   string
	}{
		{
			name:   "installed empty",
			probes: &fakeProbes{installed: "", main: "abc123", published: true, backup: true},
			want:   "InstalledSHA",
		},
		{
			name:   "installed blank",
			probes: &fakeProbes{installed: "   ", main: "abc123", published: true, backup: true},
			want:   "InstalledSHA",
		},
		{
			name:   "main empty",
			probes: &fakeProbes{installed: "abc123", main: "", published: true, backup: true},
			want:   "MainSHA",
		},
		{
			name:   "both empty",
			probes: &fakeProbes{installed: "", main: "", published: true, backup: true},
			want:   "empty sha",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			steps, _, err := BuildPlan(context.Background(), tc.probes, PlanOptions{})
			if err == nil {
				t.Fatalf("BuildPlan error = nil, want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if steps != nil {
				t.Errorf("steps = %v, want nil on error", steps)
			}
		})
	}
}

func TestPlanProbeErrorNamesTheProbe(t *testing.T) {
	t.Parallel()
	cause := errors.New("probe unavailable")
	cases := []struct {
		name string
		fail func(*fakeProbes)
		want string
	}{
		{"InstalledSHA", func(p *fakeProbes) { p.errInstalled = cause }, "InstalledSHA"},
		{"MainSHA", func(p *fakeProbes) { p.errMain = cause }, "MainSHA"},
		{"ImagePublished", func(p *fakeProbes) { p.errImage = cause }, "ImagePublished"},
		{"ArchiveBehind", func(p *fakeProbes) { p.errArchive = cause }, "ArchiveBehind"},
		{"BackupToday", func(p *fakeProbes) { p.errBackup = cause }, "BackupToday"},
		{"GCDue", func(p *fakeProbes) { p.errGC = cause }, "GCDue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &fakeProbes{installed: "abc123", main: "abc123", published: true, backup: true}
			tc.fail(p)

			_, _, err := BuildPlan(context.Background(), p, PlanOptions{})
			if err == nil {
				t.Fatalf("BuildPlan error = nil, want the %s failure", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			if !errors.Is(err, cause) {
				t.Errorf("error does not wrap the probe cause: %v", err)
			}
		})
	}
}
