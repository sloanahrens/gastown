package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/estop"
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
//
//testpolicy:allow parallel — XDG_CONFIG_HOME is process-wide, and pointing it at the throwaway home here would point every parallel test's Jev client there too
func TestJevFailcauseCandidateWrapsWhenPriced(t *testing.T) {
	config := t.TempDir()
	//testpolicy:allow no-env — the Jev client reads its key from XDG_CONFIG_HOME, so the throwaway config home must reach the whole process
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

// failcauseCountingAsker counts the Jev calls the battery makes. It answers
// with an error, which the battery records and ends the judging on.
type failcauseCountingAsker struct{ calls int }

func (a *failcauseCountingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	return nil, errors.New("fake asker: no answer")
}

// failedGate is a candidate gate whose verdict is red, with one failing Go test
// in its tail for the battery to judge.
type failedGate struct{}

const failcauseTail = "--- FAIL: TestSomething (0.00s)\nFAIL\texample.com/pkg\t0.010s\nFAIL\n"

func (failedGate) Run(context.Context, land.Repo, string, land.Work, string) land.CandidateResult {
	return land.CandidateResult{State: land.CandidateFailed, Tail: failcauseTail}
}

func (failedGate) Discard(context.Context, land.Work, land.CandidateResult) {}

// An E-stop raised before a red verdict is judged keeps the battery from asking
// Jev: the candidate's asker refuses the call, and the landing's verdict is
// still returned unchanged.
func TestJevFailcauseCandidateStopsUnderEstop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	asker := &failcauseCountingAsker{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(&bytes.Buffer{}, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevFailcause: &JevFailcauseConfig{Enabled: true, USDPerBillionTokens: 3}},
		},
		jevFailcauseOpen: func(string, failcause.Settings, io.Writer) (*failcause.Pass, error) {
			return &failcause.Pass{
				Asker:   asker,
				LogPath: filepath.Join(town, "failcause.jsonl"),
				Allows:  func() (bool, error) { return true, nil },
			}, nil
		},
	}
	if err := estop.Activate(town, estop.TriggerManual, "test E-stop before the judging"); err != nil {
		t.Fatal(err)
	}
	shadow := d.jevFailcauseCandidate("gastown", failedGate{}).(failcause.Shadow)
	// Run the judging inline so the test sees its outcome.
	shadow.Async = func(call func()) { call() }

	res := shadow.Run(context.Background(), nil, t.TempDir(), land.Work{BeadID: "gt-abc"}, "abc123")

	if res.State != land.CandidateFailed {
		t.Errorf("the verdict is %v, want the gate's red result returned unchanged", res.State)
	}
	if asker.calls != 0 {
		t.Errorf("the battery asked Jev %d time(s) under the E-stop, want none", asker.calls)
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
	if ciFailureRetention < failcause.WillRecurWindow {
		t.Errorf("the daemon keeps the ledger for %s, shorter than the %s will_recur window", ciFailureRetention, failcause.WillRecurWindow)
	}
}
