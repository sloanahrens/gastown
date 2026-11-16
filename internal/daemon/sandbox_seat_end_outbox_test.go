package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
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

// A sandbox record the host cannot read is not copied from this step's account:
// the copy is not asked, the log names the read failure, and the delete steps
// that follow re-read the record and keep the seat with their own reason, so no
// sandbox is deleted on a record this step could not read (gt-c6tofd).
func TestNoBeadAccessSeatEndUnreadableRecordIsNotCopied(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	sessionName := f.host.sessionName(endRig, endPolecat)
	registry := session.SandboxRegistryDir(f.town)
	if err := os.MkdirAll(registry, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(registry, sessionName+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.host.sandboxExists = func(string, string) (bool, error) { return true, nil }
	asked := 0
	f.host.sandboxOutboxCopy = func(string, session.SandboxRecord) error {
		asked++
		return nil
	}
	deleted := 0
	f.host.sandboxDeleter = func(string, session.SandboxRecord) error {
		deleted++
		return nil
	}
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end finished a seat whose sandbox record it could not read")
	}
	if asked != 0 {
		t.Errorf("the outbox was copied %d times from a record the host could not read, want no copy", asked)
	}
	if deleted != 0 {
		t.Errorf("the sandbox was deleted %d times on a record the host could not read, want it kept", deleted)
	}
	if !strings.Contains(logs.String(), "its outbox is not copied out") {
		t.Errorf("log %q does not name the record it could not read", logs.String())
	}
	path, err := keptSandboxSeatPath(f.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		t.Fatalf("reading the kept record: %v", err)
	}
	if strings.Contains(kept.Reason, outboxNotCopiedReason) {
		t.Errorf("kept reason = %q, want the delete step's own reason, not this step's read failure", kept.Reason)
	}
	if !strings.Contains(kept.Reason, "cannot be read") {
		t.Errorf("kept reason = %q, want the delete step's record read failure", kept.Reason)
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

// writeSeatOutbox writes a sandbox seat's outbox into the directory the host
// copies it to, the place the seat end reads it from. An empty argument is a
// file the seat did not write.
func writeSeatOutbox(t *testing.T, town, result, notes string) {
	t.Helper()
	dir := session.SandboxOutboxCopyDir(town, endRig, endPolecat)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct{ name, body string }{
		{"result.json", result},
		{"notes.md", notes},
	} {
		if file.body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, file.name), []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// seedSeatBead creates the bead the seat was given, so its outbox is applied to
// a bead that exists and carries the acceptance its ticks index.
func seedSeatBead(t *testing.T, f *seatEndFake, acceptance string) {
	t.Helper()
	if _, err := f.agentDB.Create(beads.CreateOptions{
		ID: endBead, Title: "the seat's task", Priority: 1, Acceptance: acceptance,
	}); err != nil {
		t.Fatal(err)
	}
}

// failingEscalationClient is the town's bead store refusing to create the
// escalation bead, every other write answering as the fake does.
type failingEscalationClient struct {
	beads.Client
	err error
}

func (c failingEscalationClient) Create(beads.CreateOptions) (*beads.Issue, error) {
	return nil, c.err
}

// failingListClient is the town's bead store refusing a listing, which is how
// the lookup for an already open escalation fails.
type failingListClient struct {
	beads.Client
	err error
}

func (c failingListClient) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, c.err
}

// seatOutboxEscalation is result.json carrying a HELP request, the shape a
// file-mode `gt escalate` writes: the description, then the severity line.
func seatOutboxEscalation() string {
	return `{"bead_id":"` + endBead + `","status":"done",` +
		`"escalation":{"text":"HELP: the acceptance list names a missing file\n\nSeverity: high"}}`
}

// A seat that ended with a HELP request in its outbox files it as one
// escalation bead, raised as the seat and about its bead (gt-s4gfph).
func TestNoBeadAccessSeatEndFilesTheOutboxEscalation(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxEscalation(), "")
	f.host.d.clock = clockwork.NewFakeClockAt(time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC))

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox it applied")
	}
	escalations, err := beads.ListEscalations(f.agentDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 1 {
		t.Fatalf("the outbox filed %d escalation beads, want one: %+v", len(escalations), escalations)
	}
	got := escalations[0]
	if got.Title != "HELP: the acceptance list names a missing file" {
		t.Errorf("escalation title = %q, want the request's first line", got.Title)
	}
	fields := beads.ParseEscalationFields(got.Description)
	for _, c := range []struct{ what, got, want string }{
		{"severity", fields.Severity, "high"},
		{"escalated_by", fields.EscalatedBy, endRig + "/polecats/" + endPolecat},
		{"escalated_at", fields.EscalatedAt, "2026-11-16T09:00:00Z"},
		{"related_bead", fields.RelatedBead, endBead},
		{"source", fields.Source, seatEscalationSource},
	} {
		if c.got != c.want {
			t.Errorf("escalation %s = %q, want %q", c.what, c.got, c.want)
		}
	}
	if !strings.Contains(fields.Reason, "HELP: the acceptance list names a missing file") {
		t.Errorf("escalation reason = %q, want the seat's request as its detail", fields.Reason)
	}
}

// A seat whose escalation the store did not file keeps its sandbox: the
// request is what the outbox exists for, and the sandbox is where a later pass
// reads it from (gt-s4gfph).
func TestNoBeadAccessSeatEndFailedEscalationKeepsTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	seedSeatBead(t, f, "## Acceptance\n\n- [ ] one\n")
	writeSeatOutbox(t, f.town, seatOutboxEscalation(), "the seat's note\n")
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return failingEscalationClient{Client: f.agentDB, err: errors.New("dolt unavailable")}
	}
	removed := 0
	f.host.removeSandboxSeatDirs = func(string, string, string) (bool, error) {
		removed++
		return true, nil
	}
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end removed a seat whose escalation it could not file")
	}
	if removed != 0 {
		t.Errorf("the seat's directories were removed %d times, want the seat kept", removed)
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed, want it kept")
	}
	want := outboxEscalationReason + ": filing the escalation: dolt unavailable"
	if !strings.Contains(logs.String(), "kept: "+want) {
		t.Errorf("log %q does not name the unraised escalation %q", logs.String(), want)
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

// A seat end that runs again after a failed escalation files the escalation
// alone: the notes and findings the earlier pass wrote are not written twice
// (gt-s4gfph).
func TestNoBeadAccessSeatEndRetriesOnlyTheEscalation(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	const result = `{"bead_id":"` + endBead + `","status":"done","findings":["the retry never resets its counter"],` +
		`"escalation":{"text":"HELP: nothing lands\n\nSeverity: high"}}`
	writeSeatOutbox(t, f.town, result, "the seat's note\n")
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return failingEscalationClient{Client: f.agentDB, err: errors.New("dolt unavailable")}
	}
	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the first pass finished a seat whose escalation it could not file")
	}

	// The store answers again, and the patrol offers the kept seat its end.
	f.host.openRecoveryBeads = func([]string) beads.Client { return f.agentDB }
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the second pass did not finish a seat whose escalation it filed")
	}

	escalations, err := beads.ListEscalations(f.agentDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 1 {
		t.Fatalf("the outbox filed %d escalation beads, want one: %+v", len(escalations), escalations)
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(issue.Notes, "the seat's note"); got != 1 {
		t.Errorf("the seat's note is on the bead %d times, want once: %q", got, issue.Notes)
	}
	comments, err := f.agentDB.Comments(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Errorf("the bead carries %d findings, want the seat's one: %+v", len(comments), comments)
	}
}

// An outbox is applied once however many times the seat end runs: a pass that
// reads the same bytes again writes no second note, finding, tick or escalation
// (gt-s4gfph).
func TestNoBeadAccessSeatEndAppliesAnOutboxOnce(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "## Acceptance\n\n- [ ] the fix holds\n")
	const result = `{"bead_id":"` + endBead + `","status":"done","ticked_boxes":[0],` +
		`"findings":["the retry never resets its counter"],` +
		`"escalation":{"text":"HELP: nothing lands\n\nSeverity: high"}}`
	writeSeatOutbox(t, f.town, result, "the seat's note\n")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox it applied")
	}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the second pass did not finish the same seat")
	}

	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(issue.Notes, "the seat's note"); got != 1 {
		t.Errorf("the seat's note is on the bead %d times, want once: %q", got, issue.Notes)
	}
	if !strings.Contains(issue.AcceptanceCriteria, "- [x] the fix holds") {
		t.Errorf("acceptance = %q, want the seat's ticked box", issue.AcceptanceCriteria)
	}
	comments, err := f.agentDB.Comments(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Errorf("the bead carries %d findings, want the seat's one: %+v", len(comments), comments)
	}
	escalations, err := beads.ListEscalations(f.agentDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 1 {
		t.Fatalf("two passes filed %d escalation beads, want one: %+v", len(escalations), escalations)
	}
}

// A second raise of one bead's request adds no escalation bead: one open
// escalation stands for the request, however many passes read the outbox
// (gt-s4gfph).
func TestNoBeadAccessSeatEndDoesNotFileTheEscalationTwice(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxEscalation(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox it applied")
	}
	// A record the host lost is the case the escalation's own key answers: the
	// next pass reads the same outbox as unapplied.
	path, err := appliedSeatOutboxPath(f.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the second pass did not finish the same seat")
	}
	escalations, err := beads.ListEscalations(f.agentDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 1 {
		t.Fatalf("two passes filed %d escalation beads, want one: %+v", len(escalations), escalations)
	}
}

// A seat that wrote notes but no result.json is reported: the notes are not
// applied, since nothing names the outcome they belong to, and the copy keeps
// them (gt-s4gfph).
func TestNoBeadAccessSeatEndNotesWithoutAResultAreReported(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, "", "the seat's note\n")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end kept a seat whose outbox holds no result.json")
	}
	if !strings.Contains(logs.String(), "holds notes.md but no result.json") {
		t.Errorf("log %q does not report the notes it applied nothing from", logs.String())
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Notes != "" {
		t.Errorf("notes = %q, want the bead untouched", issue.Notes)
	}
}

// A name a path cannot hold names no applied record: the write and the read
// both refuse it rather than reaching outside the runtime directory
// (gt-s4gfph).
func TestNoBeadAccessSeatEndRefusesAnUnnameableAppliedRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, err := appliedSeatOutboxPath(town, endRig, "../escape"); err == nil {
		t.Error("a polecat name that leaves the runtime directory named an applied record")
	}
	if _, _, err := readAppliedSeatOutbox(town, endRig, "../escape"); err == nil {
		t.Error("a polecat name that leaves the runtime directory read an applied record")
	}
}

// An applied record the host cannot read applies the outbox again, and says so:
// a repeated note is the lesser harm, a record taken as applied is not
// (gt-s4gfph).
func TestNoBeadAccessSeatEndReappliesAnUnreadableAppliedRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{
			name: "a record that is not JSON",
			write: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a directory where the record should be",
			write: func(t *testing.T, path string) {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			f.seedAgent()
			f.retire(intent.DesiredStop)
			f.seatDirs()
			seedSeatBead(t, f, "")
			writeSeatOutbox(t, f.town, seatOutboxEscalation(), "the seat's note\n")
			landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
			if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
				t.Fatal("the first pass did not finish the seat")
			}
			path, err := appliedSeatOutboxPath(f.town, endRig, endPolecat)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			tt.write(t, path)
			var logs strings.Builder
			f.host.d.logger = log.New(&logs, "", 0)

			if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
				t.Fatal("the second pass did not finish the seat")
			}
			if !strings.Contains(logs.String(), "its applied record cannot be read, so its outbox is applied again") {
				t.Errorf("log %q does not report the record it could not read", logs.String())
			}
			issue, err := f.agentDB.Show(endBead)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(issue.Notes, "the seat's note"); got != 2 {
				t.Errorf("the seat's note is on the bead %d times, want the outbox applied again: %q", got, issue.Notes)
			}
		})
	}
}

// An escalation the store cannot search for keeps the seat: the request may
// already be filed, and a keep is the answer to a question that did not answer
// (gt-s4gfph).
func TestNoBeadAccessSeatEndUnreadableEscalationLookupKeepsTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxEscalation(), "")
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return failingListClient{Client: f.agentDB, err: errors.New("dolt unavailable")}
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end finished a seat whose escalation lookup it could not read")
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed, want it kept")
	}
	path, err := keptSandboxSeatPath(f.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		t.Fatalf("reading the kept record: %v", err)
	}
	if !strings.HasPrefix(kept.Reason, outboxEscalationReason) {
		t.Errorf("kept record reason = %q, want %q", kept.Reason, outboxEscalationReason)
	}
}

// An outbox the validator refuses is reported and not applied, and it does not
// keep the seat: the sandbox holds the same bytes, so a later pass would refuse
// them the same way, and the copy stays on the host for an operator
// (gt-s4gfph).
func TestNoBeadAccessSeatEndRefusedOutboxIsNotApplied(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, `{"bead_id":"gt-someone-else","status":"done"}`, "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end kept a seat whose outbox belongs to another bead")
	}
	if !strings.Contains(logs.String(), "its outbox is not applied") {
		t.Errorf("log %q does not report the refused outbox", logs.String())
	}
	escalations, err := beads.ListEscalations(f.agentDB)
	if err != nil {
		t.Fatal(err)
	}
	if len(escalations) != 0 {
		t.Errorf("the refused outbox filed %d escalation beads, want none", len(escalations))
	}
}

// An outbox the host cannot read keeps the seat: nothing is applied from a read
// that failed, and the sandbox stays with the copy (gt-s4gfph).
func TestNoBeadAccessSeatEndUnreadableOutboxKeepsTheSeat(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	dirs := f.seatDirs()
	seedSeatBead(t, f, "")
	// result.json is a directory, so the read fails in a way that is not
	// "the seat wrote none".
	dir := session.SandboxOutboxCopyDir(f.town, endRig, endPolecat)
	if err := os.MkdirAll(filepath.Join(dir, "result.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end removed a seat whose outbox it could not read")
	}
	if !dirExists(t, dirs.PolecatDir) {
		t.Error("the seat directory was removed, want it kept")
	}
	path, err := keptSandboxSeatPath(f.town, endRig, endPolecat)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := readKeptSandboxSeatRecord(path)
	if err != nil {
		t.Fatalf("reading the kept record: %v", err)
	}
	if !strings.HasPrefix(kept.Reason, outboxNotReadReason) {
		t.Errorf("kept record reason = %q, want %q", kept.Reason, outboxNotReadReason)
	}
}
