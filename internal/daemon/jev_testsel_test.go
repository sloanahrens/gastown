package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// jev_testsel is off unless daemon.json enables it: the battery pays for Jev
// calls on every landing.
func TestJevTestselPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_testsel") {
		t.Error("jev_testsel must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_testsel") {
		t.Error("jev_testsel must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevTestsel: &JevTestselConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_testsel") {
		t.Error("an explicit enabled=false must keep jev_testsel off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevTestsel: &JevTestselConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_testsel") {
		t.Error("an explicit enabled=true must turn jev_testsel on")
	}
}

// With the battery off, a rig's candidate gate is handed back as it was, so a
// landing runs exactly as it did before the battery existed.
func TestJevTestselCandidateIsUnwrappedWhenOff(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
	}
	gate := &land.CandidateGate{}

	got := d.jevTestselCandidate("gastown", gate)

	if got != land.Candidate(gate) {
		t.Errorf("the gate was wrapped with the battery off: %T", got)
	}
	if logs.Len() != 0 {
		t.Errorf("the gate logged with the battery off: %s", logs)
	}
}

// An enabled battery with no price refuses to start, says why, and leaves the
// rig's gate unwrapped, so the landing still runs.
func TestJevTestselCandidateNeedsAPriceAndKeepsTheGate(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevTestsel: &JevTestselConfig{Enabled: true}},
		},
	}
	gate := &land.CandidateGate{}

	got := d.jevTestselCandidate("gastown", gate)

	if got != land.Candidate(gate) {
		t.Errorf("the gate was wrapped without a price: %T", got)
	}
	if !strings.Contains(logs.String(), "jev_testsel: error:") || !strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}
