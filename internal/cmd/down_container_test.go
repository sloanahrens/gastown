package cmd

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// containerDownServices is gt down's town services for a container town whose
// supervisor is ctl. The pid path is poison: a container town's daemon is
// stopped through the supervisor, never by a pid (gt-qpeei.20).
func containerDownServices(t *testing.T, ctl *daemonControl) downServices {
	t.Helper()
	return downServices{
		supervisor:       ctl,
		daemonRunning:    func(string) (bool, int, error) { t.Error("read a pid for a container town"); return false, 0, nil },
		stopDaemon:       func(string) error { t.Error("signaled a pid for a container town"); return nil },
		doltRunning:      func(string) (bool, int, error) { return false, 0, nil },
		stopDolt:         func(string) error { return nil },
		killImposters:    func(string) error { return nil },
		findIdleMonitors: func(string) []int { return nil },
		findOrphans:      func(string) []int { return nil },
	}
}

// containerDownTown is a town that sets the container kind, with the compose
// file its supervisor drives.
func containerDownTown(t *testing.T) string {
	t.Helper()
	town := downTestTown(t, "")
	if err := daemon.SavePatrolConfig(town, containerTownConfig("scratch-daemon-1", "scratch")); err != nil {
		t.Fatalf("writing the town's daemon settings: %v", err)
	}
	writeComposeFile(t, town)
	return town
}

// gt down stops a container town's daemon through the container supervisor's
// compose command, and never signals the pid daemon.lock names: that number
// comes from the container's namespace and points at no daemon here.
func TestStopTownServices_ContainerTownStopsThroughSupervisor(t *testing.T) {
	t.Parallel()
	town := containerDownTown(t)
	ctl, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	ctl.dockerContext = "test-context"

	if !stopTownServices(town, containerDownServices(t, ctl), false) {
		t.Fatal("stopTownServices = failed, want the container's stop to count as stopped")
	}

	want := strings.Join([]string{
		"docker", "--context", "test-context", "compose", "-p", "scratch",
		"-f", filepath.Join(town, "daemon", "container", "compose.yml"), "stop", "daemon",
	}, " ")
	if got := calls.joined(); got != want {
		t.Errorf("supervisor argv = %q, want %q", got, want)
	}
}

// A dry run reports the stop it would make and runs nothing.
func TestStopTownServices_ContainerTownDryRunRunsNothing(t *testing.T) {
	t.Parallel()
	town := containerDownTown(t)
	ctl, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))

	if !stopTownServices(town, containerDownServices(t, ctl), true) {
		t.Error("stopTownServices --dry-run failed")
	}
	if got := calls.last(); got != nil {
		t.Errorf("dry run ran %v, want no supervisor command", got)
	}
}

// A supervisor stop that fails leaves the shutdown failed, and still signals
// nothing: the daemon may be up, and its pid is not this host's to signal.
func TestStopTownServices_ContainerTownStopFailure(t *testing.T) {
	t.Parallel()
	town := containerDownTown(t)
	ctl, calls := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	ctl.run = func(a []string) error {
		calls.argv = append(calls.argv, append([]string{}, a...))
		return errors.New("docker: cannot connect to the daemon")
	}

	if stopTownServices(town, containerDownServices(t, ctl), false) {
		t.Error("stopTownServices = ok, want a failure when the supervisor's stop fails")
	}
	if got := calls.last(); got == nil {
		t.Error("no supervisor command ran, want the container stop attempted")
	}
}

// A container town whose stop settings are incomplete stops nothing: acting on
// the defaults would reach the live container (gt-ix1kz).
func TestStopTownServices_ContainerTownUnsetSettingsStopNothing(t *testing.T) {
	t.Parallel()
	town := downTestTown(t, "")
	if err := daemon.SavePatrolConfig(town, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer}); err != nil {
		t.Fatalf("writing the town's daemon settings: %v", err)
	}
	ctl, calls := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})

	if stopTownServices(town, containerDownServices(t, ctl), false) {
		t.Error("stopTownServices = ok, want a refusal for a town that sets neither container_name nor compose_project")
	}
	if got := calls.last(); got != nil {
		t.Errorf("ran %v, want no docker command for a town with unset settings", got)
	}
}
