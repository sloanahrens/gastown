package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/constants"
)

func TestJevDedupePatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_dedupe") {
		t.Error("jev_dedupe must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_dedupe") {
		t.Error("jev_dedupe must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_dedupe") {
		t.Error("an explicit enabled=false must keep jev_dedupe off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_dedupe") {
		t.Error("an explicit enabled=true must turn jev_dedupe on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevDedupeTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevDedupe()

	if d.jevDedupeRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_dedupe"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev or hm.
func TestJevDedupeRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevDedupe: &JevDedupeConfig{Enabled: true}},
		},
	}

	d.runJevDedupe()

	if !strings.Contains(logs.String(), "jev_dedupe: error:") ||
		!strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevDedupeConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_dedupe":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevDedupe
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_dedupe = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}
