package daemon

import (
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// A seat's outbox is copied out of its sandbox before either delete step of the
// seat end can remove that sandbox: the copy is the host's only copy of what
// the seat wrote (gt-c6tofd).
func TestNoBeadAccessSeatEndCopiesTheOutboxBeforeTheDelete(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	sessionName := f.host.sessionName(endRig, endPolecat)
	if err := session.WriteSandboxRecord(f.town, session.SandboxRecord{
		Session: sessionName, Rig: endRig, Polecat: endPolecat, Gateway: "k8s",
	}); err != nil {
		t.Fatal(err)
	}
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
	var order []string
	f.host.sandboxOutboxCopy = func(_ string, rec session.SandboxRecord) error {
		order = append(order, "copy")
		if rec.Session != sessionName || rec.Rig != endRig || rec.Polecat != endPolecat || rec.Gateway != "k8s" {
			t.Errorf("outbox copied from %+v, want the seat's sandbox record", rec)
		}
		return nil
	}
	f.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		order = append(order, "delete")
		return nil
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox was copied out")
	}
	if want := []string{"copy", "delete"}; len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("seat-end steps = %v, want %v", order, want)
	}
}

// A seat whose outbox cannot be copied out keeps its sandbox: the sandbox is
// the only copy of what the seat wrote, so nothing is deleted and the reason
// names the copy failure (gt-c6tofd).
func TestNoBeadAccessSeatEndFailedCopyOutKeepsTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	sessionName := f.host.sessionName(endRig, endPolecat)
	if err := session.WriteSandboxRecord(f.town, session.SandboxRecord{
		Session: sessionName, Rig: endRig, Polecat: endPolecat, Gateway: "k8s",
	}); err != nil {
		t.Fatal(err)
	}
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
	deleted := 0
	f.host.sandboxDeleter = func(string, session.SandboxRecord) error {
		deleted++
		return nil
	}
	f.host.sandboxOutboxCopy = func(string, session.SandboxRecord) error {
		return errors.New("connection reset")
	}
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end removed a seat whose outbox it could not copy out")
	}
	if deleted != 0 {
		t.Errorf("the sandbox was deleted %d times, want it kept", deleted)
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed, want it kept")
	}
	want := outboxNotCopiedReason + ": connection reset"
	if !strings.Contains(logs.String(), "kept: "+want) {
		t.Errorf("log %q does not name the copy failure %q", logs.String(), want)
	}
	path, err := keptSandboxSeatPath(f.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		t.Fatalf("reading the kept record: %v", err)
	}
	if kept.Reason != want {
		t.Errorf("kept record reason = %q, want %q", kept.Reason, want)
	}
}

// A seat that wrote no outbox ends as it did before the copy-out existed: the
// copy is asked, finds nothing, and the seat's directories go.
func TestNoBeadAccessSeatEndWithoutAnOutboxRemovesTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	// The sandbox is still there and errored, so this end deletes it: the copy
	// runs and finds nothing.
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxStatus = func(_, name string) (sandbox.Status, error) {
		return sandbox.Status{Name: name, Phase: "Error"}, nil
	}
	asked := 0
	f.host.sandboxOutboxCopy = func(string, session.SandboxRecord) error {
		asked++
		return nil // a sandbox with no outbox files: nothing to copy, no failure
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat that wrote no outbox")
	}
	if asked != 1 {
		t.Errorf("outbox copy asked %d times, want once", asked)
	}
	if dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory survived the end")
	}
}

// A gateway that already confirmed the sandbox gone has no outbox to read, so
// the copy is not asked, even for a seat with no record.
func TestNoBeadAccessSeatEndSkipsTheCopyWhenTheSandboxIsGone(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	f.host.sandboxOutboxCopy = func(string, session.SandboxRecord) error {
		t.Error("the outbox was copied from a sandbox the gateway said is gone")
		return nil
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose sandbox is gone")
	}
}

// An errored sandbox with no record is still deleted at the seat end, so its
// outbox is copied from the gateway the end measured the sandbox at, the record
// that delete builds when none names it.
func TestNoBeadAccessSeatEndCopiesThroughItsGatewayWithoutARecord(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	var got []session.SandboxRecord
	f.host.sandboxOutboxCopy = func(_ string, rec session.SandboxRecord) error {
		got = append(got, rec)
		return nil
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	f.host.endFinishedSandboxSeat(endRig, endPolecat, landed)

	if len(got) != 1 {
		t.Fatalf("outbox copied %d times, want once: %+v", len(got), got)
	}
	want := session.SandboxRecord{
		Session: f.host.sessionName(endRig, endPolecat), Rig: endRig, Polecat: endPolecat, Gateway: "gw1",
	}
	if got[0] != want {
		t.Errorf("outbox copied from %+v, want a record for the measured gateway %+v", got[0], want)
	}
}
