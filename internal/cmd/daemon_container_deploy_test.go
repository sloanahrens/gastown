package cmd

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/templates"
)

// deploySupervisor offers a container town's start builder and the container its
// settings name to a deploy as its supervisor. The argv is the builder's own,
// built when the deploy asks.
type deploySupervisor struct {
	start     func() ([]string, error)
	container string
}

func (s deploySupervisor) StartArgv() ([]string, error) { return s.start() }

func (s deploySupervisor) ContainerName() string { return s.container }

// townDeploySupervisor is the deploy supervisor for town: the container
// supervisor's start builder, and the container name read from the same settings
// the builder reads.
func townDeploySupervisor(t *testing.T, c *daemonControl, town string) deploySupervisor {
	t.Helper()
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}
	cfg, err := c.townDaemonConfig(town)
	if err != nil {
		t.Fatalf("reading the town's daemon settings: %v", err)
	}
	return deploySupervisor{start: sup.start, container: cfg.ContainerName()}
}

// deployTestSpec is a spec for the scratch town's render, in the shape the
// renderer accepts.
func deployTestSpec() templates.ComposeSpec {
	return templates.ComposeSpec{
		Project:       "scratch",
		ContainerName: "scratch-daemon-1",
		Image:         daemonimage.ImageTag("0123abc"),
		TownRoot:      "/tmp/scratch-town",
		StopGrace:     templates.MinimumStopGrace,
		DoltHost:      "host.docker.internal",
	}
}

// The argv a deploy records is the argv the container supervisor's start
// builder returns for the same town settings, and the builder's own argv is
// the one the deploy runs (gt-qpeei.25 acceptance).
func TestDaemonDeploy_RecordsTheStartBuilderArgv(t *testing.T) {
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	sup := townDeploySupervisor(t, c, town)
	builderArgv, err := sup.StartArgv()
	if err != nil {
		t.Fatalf("start builder: %v", err)
	}
	want := []string{"docker", "--context", "desktop-linux", "compose", "-p", "scratch", "-f", compose, "up", "-d", "daemon"}
	if !reflect.DeepEqual(builderArgv, want) {
		t.Fatalf("start builder = %q, want %q", builderArgv, want)
	}

	var recorded []string
	d := daemonimage.Deployer{
		Run: func(_ context.Context, _ string, _, argv []string) error {
			recorded = append([]string(nil), argv...)
			return nil
		},
		Environ: []string{"PATH=/usr/bin:/bin"},
	}
	if err := d.Deploy(context.Background(), sup, compose, deployTestSpec()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !reflect.DeepEqual(recorded, builderArgv) {
		t.Errorf("the deploy ran %q, want the start builder's %q", recorded, builderArgv)
	}
}

// A town that does not set both container settings is refused by the start
// builder, and the deploy returns that refusal with the compose file untouched
// and no command run (gt-qpeei.25 acceptance).
func TestDaemonDeploy_UnsetSettingsAreTheBuildersRefusal(t *testing.T) {
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	sup := townDeploySupervisor(t, c, town)

	ran := false
	d := daemonimage.Deployer{Run: func(context.Context, string, []string, []string) error {
		ran = true
		return nil
	}}
	err := d.Deploy(context.Background(), sup, compose, deployTestSpec())
	if !errors.Is(err, errContainerSettingsUnset) {
		t.Errorf("Deploy error = %v, want the start builder's refusal", err)
	}
	if ran {
		t.Error("the deploy ran a command after the start builder refused")
	}
	got, err := os.ReadFile(compose)
	if err != nil {
		t.Fatalf("reading the compose file: %v", err)
	}
	if string(got) != "services: {}\n" {
		t.Errorf("the compose file = %q, want the town's own file unchanged", got)
	}
}

// A render for a container other than the one the town's settings name is
// refused with the compose file untouched and no command run: the start builder
// would recreate the town's container, not the one the render describes.
func TestDaemonDeploy_RefusesARenderForTheTownsOtherContainer(t *testing.T) {
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	sup := townDeploySupervisor(t, c, town)

	ran := false
	spec := deployTestSpec()
	spec.ContainerName = "live-daemon-1"
	d := daemonimage.Deployer{Run: func(context.Context, string, []string, []string) error {
		ran = true
		return nil
	}}
	if err := d.Deploy(context.Background(), sup, compose, spec); err == nil {
		t.Fatal("Deploy accepted a render for a container the town's settings do not name")
	}
	if ran {
		t.Error("the deploy ran a command for a render the town's settings refuse")
	}
	got, err := os.ReadFile(compose)
	if err != nil {
		t.Fatalf("reading the compose file: %v", err)
	}
	if string(got) != "services: {}\n" {
		t.Errorf("the compose file = %q, want the town's own file unchanged", got)
	}
}
