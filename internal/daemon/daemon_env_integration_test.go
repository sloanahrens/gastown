//go:build integration

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
)

// newDaemonForEnvWiring runs New on a town whose settings daemon.env sets
// HM_CONFIG to testStaleValue, while the process already holds testPodValue.
// containerMode is the value of the container marker; "" is outside the
// container. New's success path starts tmux and writes the process
// environment, so this runs in the integration tier. Each key New writes is
// set through t.Setenv first, so t.Setenv restores it at cleanup.
func newDaemonForEnvWiring(t *testing.T, containerMode string) *Daemon {
	t.Helper()
	town := t.TempDir()
	patrols := config.NewDaemonPatrolConfig()
	patrols.Env = map[string]string{"HM_CONFIG": testStaleValue}
	if err := SavePatrolConfig(town, patrols); err != nil {
		t.Fatalf("SavePatrolConfig: %v", err)
	}
	t.Setenv("HM_CONFIG", testPodValue)
	t.Setenv(config.ContainerModeEnv, containerMode)
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("PATH", os.Getenv("PATH"))
	for _, key := range config.DoltEndpointEnvKeys {
		t.Setenv(key, os.Getenv(key))
	}
	d, err := New(&Config{TownRoot: town, LogFile: filepath.Join(town, "daemon", "daemon.log"), PidFile: filepath.Join(town, "daemon", "daemon.pid")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(d.cancel)
	return d
}

// TestIntegrationNewContainerModeKeepsThePodValueAndRaisesTheShadowItem drives
// New itself in container mode: the pod's value stays in the process
// environment, the shadowed key lands on the daemon, and the
// config.env_shadowed collector the daemon runs names that key and no value.
// Dropping the containerModeFromEnv argument or the envShadowed field in New
// turns it red.
func TestIntegrationNewContainerModeKeepsThePodValueAndRaisesTheShadowItem(t *testing.T) {
	d := newDaemonForEnvWiring(t, config.SupervisorContainer)

	if got := os.Getenv("HM_CONFIG"); got != testPodValue {
		t.Errorf("HM_CONFIG = %q after New, want the pod's value kept", got)
	}
	if !slices.Equal(d.envShadowed, []string{"HM_CONFIG"}) {
		t.Fatalf("Daemon.envShadowed = %v, want [HM_CONFIG]", d.envShadowed)
	}
	c := collectorFor(t, d.attentionSources(time.Now()), attention.KindConfigEnvShadowed)
	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Summary, "HM_CONFIG") {
		t.Fatalf("items = %v, want one item naming HM_CONFIG", items)
	}
	for _, value := range []string{testPodValue, testStaleValue} {
		if strings.Contains(items[0].Summary, value) {
			t.Errorf("the item leaks a value %q: %q", value, items[0].Summary)
		}
	}
}

// TestIntegrationNewOutsideContainerModeAppliesTheSettingsValue: outside the
// container New applies the settings value as before and records no shadowed
// key, so the collector has nothing to raise.
func TestIntegrationNewOutsideContainerModeAppliesTheSettingsValue(t *testing.T) {
	d := newDaemonForEnvWiring(t, "")

	if got := os.Getenv("HM_CONFIG"); got != testStaleValue {
		t.Errorf("HM_CONFIG = %q after New, want the settings value applied", got)
	}
	if len(d.envShadowed) != 0 {
		t.Errorf("Daemon.envShadowed = %v, want none outside container mode", d.envShadowed)
	}
}
