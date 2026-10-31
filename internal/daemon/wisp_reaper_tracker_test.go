package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTrackerTown plants a town whose route table is routes and whose gastown
// rig names the Dolt database gastown in its metadata.
func writeTrackerTown(t *testing.T, routes string) string {
	t.Helper()
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{townBeads, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), []byte(`{"dolt_database":"gastown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

const trackerPassRoutes = `{"path":"gastown/mayor/rig","prefix":"gt-","backend":"tracker"}
{"path":"beads/mayor/rig","prefix":"be-"}
`

// trackerPass is a daemon over a town with the given route table, with the log
// it writes and the dog cycle a pass records its steps on.
type trackerPass struct {
	d     *Daemon
	log   *strings.Builder
	cycle *dogCycle
}

func newTrackerPass(t *testing.T, routes string) trackerPass {
	t.Helper()
	townRoot := writeTrackerTown(t, routes)
	var buf strings.Builder
	d := &Daemon{
		logger:    log.New(&buf, "", 0),
		config:    &Config{TownRoot: townRoot},
		dogFeedFn: func(string, map[string]interface{}) error { return nil },
	}
	return trackerPass{d: d, log: &buf, cycle: d.startDogCycle("wisp_reaper")}
}

// scanStep is how the scan step of the pass's cycle ended.
func (p trackerPass) scanStep(t *testing.T) dogStep {
	t.Helper()
	for _, s := range p.cycle.steps {
		if s.name == "scan" {
			return s
		}
	}
	t.Fatal("the pass recorded no scan step")
	return dogStep{}
}

// TestWispReaperPassSkipsATrackerRigAndLogsIt drives the daemon's pass with only
// a tracker rig's database configured, so the pass never opens a Dolt connection:
// the rig is logged as skipped and the pass ends on its scan step.
func TestWispReaperPassSkipsATrackerRigAndLogsIt(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)

	p.d.reapWispsInline(&WispReaperConfig{Databases: []string{"gastown"}}, 0, 0, 0, p.cycle)

	if !strings.Contains(p.log.String(), "wisp_reaper: gastown: skipped") {
		t.Errorf("log = %q, want a line that the tracker rig gastown was skipped", p.log.String())
	}
	if !strings.Contains(p.log.String(), "every database belongs to a tracker rig") {
		t.Errorf("log = %q, want the pass to say every database is the tracker's", p.log.String())
	}
	if s := p.scanStep(t); s.result != dogStepDone {
		t.Errorf("scan step = %+v, want done: a pass whose databases are all the tracker's has nothing to fail on", s)
	}
}

// TestWispReaperPassRefusesAnUnreadableRouteTable: a route table that does not
// parse hides the tracker rigs, so the pass fails its scan step and sweeps nothing.
func TestWispReaperPassRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, "{not json\n")

	p.d.reapWispsInline(&WispReaperConfig{Databases: []string{"gastown"}}, 0, 0, 0, p.cycle)

	if s := p.scanStep(t); s.result != dogStepFailed || s.reason != "route table unreadable" {
		t.Errorf("scan step = %+v, want it failed as an unreadable route table", s)
	}
	if strings.Contains(p.log.String(), "skipped") {
		t.Errorf("log = %q, want no skip line when the route table cannot be read", p.log.String())
	}
}

// TestSweepTargetsKeepsTheDatabasesTheTrackerDoesNotOwn: the bd database stays in
// the sweep and the tracker rig's database is left out of it.
func TestSweepTargetsKeepsTheDatabasesTheTrackerDoesNotOwn(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)

	kept, err := p.d.sweepTargets([]string{"gastown", "beads"}, nil)
	if err != nil {
		t.Fatalf("sweepTargets: %v", err)
	}
	if want := []string{"beads"}; !slices.Equal(kept, want) {
		t.Errorf("sweep keeps %v, want %v: the tracker rig's database must not reach the bd sweep", kept, want)
	}
}

// TestSweepTargetsRefusesAnEmptyDiscoveryWhenATrackerRigExists: an empty Dolt
// discovery is the same whether the town has tracker rigs or not, so it is a
// refusal, not a clean pass over the tracker's rigs.
func TestSweepTargetsRefusesAnEmptyDiscoveryWhenATrackerRigExists(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, trackerPassRoutes)

	kept, err := p.d.sweepTargets(nil, func() []string { return nil })
	if !errors.Is(err, errNoDatabases) {
		t.Fatalf("sweepTargets kept %v, err %v; want errNoDatabases when discovery finds nothing", kept, err)
	}
}

// TestSweepTargetsRefusesAnUnreadableRouteTable: a route table that does not
// parse hides the tracker rigs, so the sweep is refused rather than run.
func TestSweepTargetsRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	p := newTrackerPass(t, "{not json\n")

	kept, err := p.d.sweepTargets([]string{"gastown", "beads"}, nil)
	if err == nil || errors.Is(err, errNoDatabases) {
		t.Fatalf("sweepTargets kept %v, err %v; want an error for a route table that does not parse", kept, err)
	}
}
