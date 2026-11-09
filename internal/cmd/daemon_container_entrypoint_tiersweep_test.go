package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// tierSweepPatrols is a town's daemon patrol settings with the tier sweep set
// as given: enabled or not, and the runner block it names (nil for none).
func tierSweepPatrols(enabled bool, runner *config.TierSweepRunnerConfig) *config.DaemonPatrolConfig {
	return &config.DaemonPatrolConfig{
		Patrols: &config.PatrolsConfig{
			TierSweep: &config.TierSweepConfig{Enabled: enabled, Runner: runner},
		},
	}
}

// podTownRig is a town started inside a pod: KUBERNETES_SERVICE_HOST is set.
func podTownRig(patrols *config.DaemonPatrolConfig) *entrypointRig {
	r := townRig()
	r.env[podEnv] = "10.0.0.1"
	r.patrols = patrols
	return r
}

// wantDaemonExec checks that a start which passes the checks ends with the
// daemon's exec, the last command the entrypoint runs.
func wantDaemonExec(t *testing.T, r *entrypointRig, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if len(r.calls) == 0 {
		t.Fatal("the start ran no commands, want the daemon exec")
	}
	last := r.calls[len(r.calls)-1]
	if want := []string{entrypointBin, "daemon", "run"}; strings.Join(last, " ") != strings.Join(want, " ") {
		t.Errorf("last command = %v, want %v", last, want)
	}
}

func TestEntrypointTierSweepRefusesAPodWithNoRegistry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		runner *config.TierSweepRunnerConfig
	}{
		{"a runner block with no registry", &config.TierSweepRunnerConfig{Backend: "kubernetes", Image: "installed"}},
		{"no runner block at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := podTownRig(tierSweepPatrols(true, tc.runner))

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), "daemon.patrols.tier_sweep.runner.image_registry") ||
				!strings.Contains(err.Error(), "nothing was started") {
				t.Fatalf("error = %v, want a refusal naming daemon.patrols.tier_sweep.runner.image_registry and ending nothing was started", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestEntrypointTierSweepStartsWhenTheRegistryIsSet(t *testing.T) {
	t.Parallel()
	r := podTownRig(tierSweepPatrols(true, &config.TierSweepRunnerConfig{
		Backend:       "kubernetes",
		Image:         "installed",
		ImageRegistry: "localhost:5001",
	}))

	wantDaemonExec(t, r, runContainerEntrypoint(r.entrypoint()))
}

func TestEntrypointTierSweepStartsWhenDisabledOrAbsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		patrols *config.DaemonPatrolConfig
	}{
		{"no daemon patrol settings", nil},
		{"settings with no tier_sweep block", &config.DaemonPatrolConfig{Patrols: &config.PatrolsConfig{}}},
		{"tier_sweep disabled with no registry", tierSweepPatrols(false, &config.TierSweepRunnerConfig{Backend: "kubernetes"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := podTownRig(tc.patrols)

			wantDaemonExec(t, r, runContainerEntrypoint(r.entrypoint()))
		})
	}
}

func TestEntrypointTierSweepIgnoresANonPodStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  bool
		env  string
	}{
		{"KUBERNETES_SERVICE_HOST unset", false, ""},
		{"KUBERNETES_SERVICE_HOST empty", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			if tc.set {
				r.env[podEnv] = tc.env
			}
			r.patrols = tierSweepPatrols(true, &config.TierSweepRunnerConfig{Backend: "docker"})

			wantDaemonExec(t, r, runContainerEntrypoint(r.entrypoint()))
		})
	}
}

func TestEntrypointTierSweepUnreadableSettingsInAPodIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unreadable settings", errors.New("permission denied")},
		{"settings that do not parse", errors.New("parse the daemon patrol settings: unexpected end of JSON input")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := podTownRig(nil)
			r.patrolErr = tc.err

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), daemon.PatrolConfigSource(entrypointTown)) ||
				!strings.Contains(err.Error(), "nothing was started") {
				t.Fatalf("error = %v, want an error naming the settings file %s and ending nothing was started",
					err, daemon.PatrolConfigSource(entrypointTown))
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

// The scratch install execs the daemon as a plain start does, so the tier
// sweep check runs before the install in a pod too: no registry is refused
// with no install run, and a registry installs and starts.
func TestEntrypointTierSweepRefusesTheScratchInstallWithNoRegistry(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.env[podEnv] = "10.0.0.1"
	r.patrols = tierSweepPatrols(true, &config.TierSweepRunnerConfig{Backend: "kubernetes", Image: "installed"})

	err := runContainerEntrypoint(r.entrypoint())

	if err == nil || !strings.Contains(err.Error(), "daemon.patrols.tier_sweep.runner.image_registry") ||
		!strings.Contains(err.Error(), "nothing was started") {
		t.Fatalf("error = %v, want a refusal naming daemon.patrols.tier_sweep.runner.image_registry and ending nothing was started", err)
	}
	if len(r.calls) != 0 || len(r.created) != 0 {
		t.Errorf("a refusal ran commands %v and created markers %v, want neither", r.calls, r.created)
	}
}

func TestEntrypointTierSweepStartsTheScratchInstallWhenTheRegistryIsSet(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.env[podEnv] = "10.0.0.1"
	r.patrols = tierSweepPatrols(true, &config.TierSweepRunnerConfig{
		Backend:       "kubernetes",
		Image:         "installed",
		ImageRegistry: "localhost:5001",
	})

	wantDaemonExec(t, r, runContainerEntrypoint(r.entrypoint()))
	if r.installCalls() != 1 {
		t.Errorf("install calls = %d, want 1: the scratch install still runs when the registry is set", r.installCalls())
	}
}
