package daemon

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
)

// patrolReloadTown is a daemon over a temp town whose settings file holds a
// daemon section with the given container_off names. It starts the way New
// does: the stamp is taken before the config is read, and the daemon runs in
// container mode so container_off applies.
func patrolReloadTown(t *testing.T, containerOff ...string) (*Daemon, *lockedBuffer, *int) {
	t.Helper()
	townRoot := t.TempDir()
	settings := filepath.Join(townRoot, "settings", "config.json")
	writePatrolSettings(t, settings, containerOff)

	cfg, err := ReadPatrolConfig(townRoot)
	if err != nil || cfg == nil {
		t.Fatalf("start-up read: cfg=%v err=%v", cfg, err)
	}
	logs := &lockedBuffer{}
	reads := 0
	d := &Daemon{
		config:        &Config{TownRoot: townRoot},
		logger:        log.New(logs, "", 0),
		containerMode: true,
		patrolConfig:  cfg,
		patrolStamp:   takePatrolStamp(townRoot),
	}
	d.seams.readPatrolConfig = func(root string) (*DaemonPatrolConfig, error) {
		reads++
		return ReadPatrolConfig(root)
	}
	return d, logs, &reads
}

// writePatrolSettings writes settings/config.json with a daemon section whose
// container_off is containerOff.
func writePatrolSettings(t *testing.T, path string, containerOff []string) {
	t.Helper()
	off := containerOff
	if off == nil {
		off = []string{}
	}
	writeJSONFile(t, path, map[string]any{
		"type": "town-settings", "version": 1,
		"daemon": map[string]any{"type": "daemon-patrol-config", "version": 1, "container_off": off},
	})
}

// patrolConfigStep is the heartbeat step the daemon runs for the reload, looked
// up by name so the test drives the step the heartbeat runs.
func patrolConfigStep(t *testing.T) func(*Daemon) {
	t.Helper()
	for _, s := range heartbeatSteps {
		if s.name == "patrol-config" {
			return s.run
		}
	}
	t.Fatal("no patrol-config heartbeat step registered")
	return nil
}

// The edit to the daemon section is seen by the next heartbeat in the same
// daemon, with no restart: the container_off name that was held comes back.
func TestPatrolConfigReloadSeesDaemonSectionEditBetweenHeartbeats(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t, "hm-sync")
	step := patrolConfigStep(t)
	if d.isPatrolActive("hm-sync") {
		t.Fatal("hm-sync is active before the edit, so container_off did not apply at start")
	}

	writePatrolSettings(t, filepath.Join(d.config.TownRoot, "settings", "config.json"), nil)
	step(d)

	if !d.isPatrolActive("hm-sync") {
		t.Error("hm-sync is still held after the edit removed it from container_off: the reload did not swap the config in")
	}
}

// A file that does not parse keeps the config in force, names the file in one
// attention item, and logs the parse error once for the change.
func TestPatrolConfigBadFileKeepsLastGoodAndRaisesOneItem(t *testing.T) {
	t.Parallel()
	d, logs, _ := patrolReloadTown(t, "hm-sync")
	step := patrolConfigStep(t)
	before := d.patrol()

	settings := filepath.Join(d.config.TownRoot, "settings", "config.json")
	if err := os.WriteFile(settings, []byte(`{"daemon": { broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	step(d)
	step(d)

	if got := d.patrol(); got != before || len(got.ContainerOff()) != 1 || got.ContainerOff()[0] != "hm-sync" {
		t.Errorf("config in force after a bad file = %+v, want the last good config kept", got)
	}
	if d.isPatrolActive("hm-sync") {
		t.Error("hm-sync came back after a bad file: the daemon fell back to defaults")
	}
	if n := strings.Count(logs.String(), "did not reload"); n != 1 {
		t.Errorf("parse failure logged %d times across two heartbeats on one change, want once", n)
	}

	s := d.attentionSources(time.Now())
	items, err := collectorFor(t, s, attention.KindConfigParseFailed).collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want exactly one", len(items))
	}
	if got := attention.RuleFor(items[0]); got != attention.RuleConfigParseFailed {
		t.Errorf("rule = %q, want %q", got, attention.RuleConfigParseFailed)
	}
	if !strings.Contains(items[0].Summary, "config.json") {
		t.Errorf("summary %q does not name the file", items[0].Summary)
	}
}

// Once the file parses again the reload swaps the new config in and the item
// clears.
func TestPatrolConfigFixedFileClearsTheItem(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t, "hm-sync")
	step := patrolConfigStep(t)
	settings := filepath.Join(d.config.TownRoot, "settings", "config.json")
	if err := os.WriteFile(settings, []byte(`{ broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	step(d)

	writePatrolSettings(t, settings, []string{"hm-sync", "landing_worker"})
	step(d)

	if d.currentPatrolFailure() != nil {
		t.Error("the failure is still in force after the file parses")
	}
	items, err := collectorFor(t, d.attentionSources(time.Now()), attention.KindConfigParseFailed).collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d after the file parses, want none", len(items))
	}
	if got := d.patrol().ContainerOff(); len(got) != 2 {
		t.Errorf("container_off = %v, want the reloaded two names", got)
	}
}

// A file whose stamp did not change is not reparsed on the next heartbeat.
func TestPatrolConfigUnchangedFileIsNotReparsed(t *testing.T) {
	t.Parallel()
	d, _, reads := patrolReloadTown(t, "hm-sync")
	step := patrolConfigStep(t)

	step(d)
	step(d)
	if *reads != 0 {
		t.Errorf("unchanged file was parsed %d time(s) across two heartbeats, want none", *reads)
	}

	writePatrolSettings(t, filepath.Join(d.config.TownRoot, "settings", "config.json"), nil)
	step(d)
	if *reads != 1 {
		t.Errorf("edited file was parsed %d time(s), want exactly one", *reads)
	}
}

// A settings file that loses its daemon section while the daemon runs on a
// config keeps that config and raises the item: the section is not a reason to
// fall back to defaults.
func TestPatrolConfigGoneSectionKeepsConfigInForce(t *testing.T) {
	t.Parallel()
	d, _, _ := patrolReloadTown(t, "hm-sync")
	step := patrolConfigStep(t)

	writeJSONFile(t, filepath.Join(d.config.TownRoot, "settings", "config.json"), map[string]any{
		"type": "town-settings", "version": 1,
	})
	step(d)

	if got := d.patrol(); got == nil || len(got.ContainerOff()) != 1 {
		t.Errorf("config in force = %+v after the daemon section went away, want the last good config kept", got)
	}
	if d.currentPatrolFailure() == nil {
		t.Error("the missing config raised no failure")
	}
}
