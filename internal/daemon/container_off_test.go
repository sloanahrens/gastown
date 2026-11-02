package daemon

import (
	"context"
	"log"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// daemon.container_off is honoured only by the container daemon: the launchd
// daemon runs every name as before (gt-1oz0t).
func TestContainerOffSkipsOnlyInContainerMode(t *testing.T) {
	t.Parallel()
	cfg := &DaemonPatrolConfig{
		Patrols:             &PatrolsConfig{LandingWorker: &LandingWorkerConfig{Enabled: true}},
		ContainerOffSetting: []string{"landing_worker", "town-script"},
	}
	for _, container := range []bool{false, true} {
		d := &Daemon{patrolConfig: cfg, containerMode: container}
		if got := d.isPatrolActive("landing_worker"); got == container {
			t.Errorf("containerMode=%v: landing_worker active = %v", container, got)
		}
	}

	for _, container := range []bool{false, true} {
		townRoot := t.TempDir()
		logs := &lockedBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		bash := newFakeCLIFor(func(cliCall) cliReply {
			cancel()
			return cliReply{code: 1}
		})
		d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(logs, "", 0), ctx: ctx, execCmd: bash.run,
			patrolConfig: cfg, containerMode: container}
		writeTestScriptPlugin(t, townRoot, "town-script")
		d.dispatchPlugins(nil)
		if d.scripts != nil {
			d.scripts.wait() // only a started script initialises the runner
		}
		cancel()
		if ran := len(bash.recorded()); (ran == 0) != container {
			t.Errorf("containerMode=%v: the plugin ran %d time(s)", container, ran)
		}
	}
}

func TestContainerOffAccessorReadsTheSetting(t *testing.T) {
	t.Parallel()
	var none *config.DaemonPatrolConfig
	if len(none.ContainerOff()) != 0 {
		t.Error("a nil config lists names")
	}
	if got := (&config.DaemonPatrolConfig{ContainerOffSetting: []string{"a"}}).ContainerOff(); len(got) != 1 || got[0] != "a" {
		t.Errorf("ContainerOff = %v", got)
	}
}
