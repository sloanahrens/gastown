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

// startBuilderSupervisor offers a container town's start builder to a deploy
// as its supervisor: the argv is the builder's own, built when the deploy asks.
type startBuilderSupervisor func() ([]string, error)

func (b startBuilderSupervisor) StartArgv() ([]string, error) { return b() }

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
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, containerTownConfig("scratch-daemon-1", "scratch"))
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}
	builderArgv, err := sup.start()
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
	if err := d.Deploy(context.Background(), startBuilderSupervisor(sup.start), compose, deployTestSpec()); err != nil {
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
	t.Parallel()
	town := t.TempDir()
	compose := writeComposeFile(t, town)
	c, _ := stubContainerTown(t, &config.DaemonPatrolConfig{Supervisor: config.SupervisorContainer})
	sup, err := c.detectDaemonSupervisor(town)
	if err != nil || sup == nil {
		t.Fatalf("detectDaemonSupervisor = (%v, %v), want the container supervisor", sup, err)
	}

	ran := false
	d := daemonimage.Deployer{Run: func(context.Context, string, []string, []string) error {
		ran = true
		return nil
	}}
	err = d.Deploy(context.Background(), startBuilderSupervisor(sup.start), compose, deployTestSpec())
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
