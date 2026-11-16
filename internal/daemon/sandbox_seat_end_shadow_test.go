package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
)

// gt-xoj72t: the sandbox seat end keeps deciding on the agent bead's hook, and
// reads the hook the seat's intent record names beside it so the shadow window
// measures the fact the decision moves onto the record. These tests drive the
// end, not the comparison helper, so an end that stopped asking for the record
// fails them.

// retiredHolding records the seat's intent record retired to stop while still
// naming workBead: the record whose work bead the shadow compares against the
// bead's hook. An empty workBead is the record a landing leaves.
func (f *seatEndFake) retiredHolding(workBead string) {
	f.t.Helper()
	seat := intent.Seat{Rig: endRig, Role: constants.RolePolecat, Name: endPolecat}
	if workBead != "" {
		if err := intent.MarkSubmitted(f.town, seat, workBead, "test", testEpoch); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := intent.Update(f.town, seat, func(r *intent.Record) error {
		r.Desired = intent.DesiredStop
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

// brokenRecord leaves a malformed intent record on the seat, the state a
// half-written file behind the shadow's read leaves.
func (f *seatEndFake) brokenRecord() {
	f.t.Helper()
	path := intent.Seat{Rig: endRig, Role: constants.RolePolecat, Name: endPolecat}.Path(f.town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// A seat whose intent record cannot be read is logged once and decides
// nothing: the shadow's read fails closed, so the seat end keeps the seat on
// the record it cannot read, exactly as it did before the shadow asked.
func TestAgentBeadRetireSeatEndShadowUnreadableRecord(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.brokenRecord()
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}

	for i := 0; i < 2; i++ {
		f.host.endFinishedSandboxSeat(endRig, endPolecat, landed)
	}
	want := "agentbead_shadow: " + endRig + "/" + endPolecat + " hook unreadable intent record"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Fatalf("logged the unreadable record %d time(s) over two end passes, want once:\n%s", got, logs.String())
	}
	if want := "kept: cannot read its intent record"; !strings.Contains(logs.String(), want) {
		t.Errorf("log does not keep the seat on the unreadable record (%q):\n%s", want, logs.String())
	}
}

// A seat whose intent record and agent bead name different hooks logs the
// disagreement once, however many passes offer the seat's end: the end is
// retried on every heartbeat and on every pending record, and the shadow
// window it feeds is days, so a line per pass would bury the disagreement the
// overseer reads for.
func TestAgentBeadRetireSeatEndShadowLogsHookOnce(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seatFields.HookBead = endBead
	f.seedAgent()
	f.retiredHolding("gt-intent")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}

	for i := 0; i < 3; i++ {
		f.host.endFinishedSandboxSeat(endRig, endPolecat, landed)
	}
	want := "agentbead_shadow: " + endRig + "/" + endPolecat + " hook intent=gt-intent bead=" + endBead
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Fatalf("logged the hook disagreement %d time(s) over three end passes, want once:\n%s", got, logs.String())
	}
}

// The seat end's decision is the bead's, with or without the intent record
// naming the same work bead: every keep and end below answers the same when the
// record agrees about the hook and when it names another bead. The shadow only
// measures; moving the decision onto the record is P2.2 of the plan, and until
// then a disagreement must not change what the seat end does.
func TestAgentBeadRetireSeatEndShadowKeepsDecision(t *testing.T) {
	t.Parallel()
	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	tests := []struct {
		name string
		// hook is the hook_bead the seat's agent bead carries: the value the
		// end decides on.
		hook    string
		cleanup polecat.CleanupStatus
		landing sandboxSeatLanding
		// wantRemoved is the decision both variants must reach; wantLine is
		// the line naming it, which both must log.
		wantRemoved bool
		wantLine    string
	}{
		{
			name: "a finished seat goes", landing: landed,
			wantRemoved: true, wantLine: "removed its host directories: the seat is finished",
		},
		{
			name: "a seat still holding its hook is kept", hook: endBead, landing: landed,
			wantLine: "kept: hook_bead=" + endBead + " is still set",
		},
		{
			name:     "an unlanded seat is kept",
			wantLine: "kept: no landing record proves its work landed (unlanded)",
		},
		{
			name: "a seat with unpushed work is kept", cleanup: polecat.CleanupUnpushed, landing: landed,
			wantLine: "kept: cleanup_status=" + string(polecat.CleanupUnpushed),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := func(workBead string) (bool, string) {
				f := newSeatEndFake(t)
				f.seatFields.HookBead = tt.hook
				if tt.cleanup != "" {
					f.seatFields.CleanupStatus = string(tt.cleanup)
				}
				f.seedAgent()
				f.retiredHolding(workBead)
				var logs strings.Builder
				f.host.d.logger = log.New(&logs, "", 0)
				removed := f.host.endFinishedSandboxSeat(endRig, endPolecat, tt.landing)
				return removed, logs.String()
			}

			removedAgree, logsAgree := run(tt.hook)
			removedDiffer, logsDiffer := run("gt-other")

			if removedAgree != removedDiffer || removedAgree != tt.wantRemoved {
				t.Fatalf("the seat end removed the seat %v with an agreeing record and %v with a differing one, want %v both times",
					removedAgree, removedDiffer, tt.wantRemoved)
			}
			for variant, logs := range map[string]string{"agreeing record": logsAgree, "differing record": logsDiffer} {
				if !strings.Contains(logs, tt.wantLine) {
					t.Errorf("with the %s the log does not name the decision %q:\n%s", variant, tt.wantLine, logs)
				}
			}
			if got := strings.Count(logsAgree, "agentbead_shadow:"); got != 0 {
				t.Errorf("logged %d shadow line(s) for a record naming the bead's own hook, want none:\n%s", got, logsAgree)
			}
			want := "agentbead_shadow: " + endRig + "/" + endPolecat + " hook intent=gt-other bead=" + tt.hook
			if got := strings.Count(logsDiffer, want); got != 1 {
				t.Fatalf("logged %d line(s) naming the differing record's hook, want once (%q):\n%s", got, want, logsDiffer)
			}
		})
	}
}
