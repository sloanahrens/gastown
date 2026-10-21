package daemon

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/drift"
)

func TestJevDriftPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_drift") {
		t.Error("jev_drift must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_drift") {
		t.Error("jev_drift must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_drift") {
		t.Error("an explicit enabled=false must keep jev_drift off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_drift") {
		t.Error("an explicit enabled=true must turn jev_drift on")
	}
}

// With the patrol off the trigger runs no pass: no single-flight guard is
// taken, no last-run record is written and nothing is logged.
func TestJevDriftTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
	}

	d.triggerJevDrift()

	if d.jevDriftRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_drift"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and names the missing price,
// without reaching Jev or git.
func TestJevDriftRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{Enabled: true}},
		},
	}

	d.runJevDrift()

	if !strings.Contains(logs.String(), "jev_drift: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// The pass is opened with the patrol's own settings, and a failure to open it
// is logged under the patrol's name.
func TestJevDriftRunOpensThePassWithItsSettings(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	var got drift.Settings
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDrift: &JevDriftConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
		jevDriftOpen: func(_ string, s drift.Settings, _ io.Writer) (*drift.Pass, error) {
			got = s
			return nil, errors.New("boom")
		},
	}

	d.runJevDrift()

	if got.MaxUSDPerDay != 2.5 || got.USDPerBillionTokens != 3 {
		t.Errorf("settings = %+v, want the 2.5 cap at 3 USD per billion tokens", got)
	}
	if !strings.Contains(logs.String(), "jev_drift: error: boom") {
		t.Errorf("log = %q, want the open failure under jev_drift", logs)
	}
}

func TestJevDriftConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_drift":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevDrift
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_drift = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}
