package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/failcause"
	"github.com/steveyegge/gastown/internal/land"
)

// jev_failcause is off unless daemon.json enables it: the battery pays for Jev
// calls on every red candidate verdict.
func TestJevFailcausePatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_failcause") {
		t.Error("jev_failcause must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_failcause") {
		t.Error("jev_failcause must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevFailcause: &JevFailcauseConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_failcause") {
		t.Error("an explicit enabled=false must keep jev_failcause off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevFailcause: &JevFailcauseConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_failcause") {
		t.Error("an explicit enabled=true must turn jev_failcause on")
	}
}

// With the battery off, a rig's candidate gate is handed back as it was, so a
// landing runs exactly as it did before the battery existed.
func TestJevFailcauseCandidateIsUnwrappedWhenOff(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
	}
	gate := &land.CandidateGate{}

	got := d.jevFailcauseCandidate("gastown", gate)

	if got != land.Candidate(gate) {
		t.Errorf("the gate was wrapped with the battery off: %T", got)
	}
	if logs.Len() != 0 {
		t.Errorf("the gate logged with the battery off: %s", logs)
	}
}

// An enabled battery with no price refuses to start, says why, and leaves the
// rig's gate unwrapped, so the landing still runs.
func TestJevFailcauseCandidateNeedsAPriceAndKeepsTheGate(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevFailcause: &JevFailcauseConfig{Enabled: true}},
		},
	}
	gate := &land.CandidateGate{}

	got := d.jevFailcauseCandidate("gastown", gate)

	if got != land.Candidate(gate) {
		t.Errorf("the gate was wrapped without a price: %T", got)
	}
	if !strings.Contains(logs.String(), "jev_failcause: error:") || !strings.Contains(logs.String(), "usd_per_billion_tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// An enabled, priced battery wraps the gate in its shadow, which judges the red
// verdicts and returns the gate's result. The Jev key comes from a throwaway
// config home, never the operator's.
func TestJevFailcauseCandidateWrapsWhenPriced(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	if err := os.MkdirAll(filepath.Join(config, "gt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "gt", "jev.env"), []byte(jev.EnvKey+"=test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&bytes.Buffer{}, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevFailcause: &JevFailcauseConfig{Enabled: true, USDPerBillionTokens: 3}},
		},
	}

	got := d.jevFailcauseCandidate("gastown", &land.CandidateGate{})

	if _, ok := got.(failcause.Shadow); !ok {
		t.Errorf("the gate is %T, want the cause battery's shadow", got)
	}
}

// The daemon's CI-failure ledger is the one the cause battery's coverage rule
// reads: the same path, and the same retention the rule assumes.
func TestCIFailureLedgerMatchesTheCauseBattery(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if got, want := ciFailureLedgerPath(town), failcause.CIFailureLedgerPath(town); got != want {
		t.Errorf("the daemon writes the ledger at %s, the cause battery reads %s", got, want)
	}
	if ciFailureRetention != failcause.CIFailureRetention {
		t.Errorf("the daemon keeps the ledger for %s, the cause battery assumes %s", ciFailureRetention, failcause.CIFailureRetention)
	}
}
