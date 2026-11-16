package daemon

import (
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
)

// seatOutboxSubmission is result.json carrying the branch a file-mode gt done
// pushed, the shape the host writes the READY TO LAND block from.
func seatOutboxSubmission() string {
	return `{"bead_id":"` + endBead + `","status":"done",` +
		`"submission":{"branch":"` + endBranch + `",` +
		`"head":"0123456789abcdef0123456789abcdef01234567",` +
		`"target":"main","submitted_at":"2026-11-16T18:40:48Z"}}`
}

// A seat's outbox says what it pushed, and the host the seat could not reach
// writes the READY TO LAND block and the ready label on the bead, so the
// landing worker finds it (gt-qk3ppr).
func TestNoBeadAccessSeatEndWritesTheReadyBlockFromTheOutbox(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox it applied")
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	want := "READY TO LAND\nBranch: " + endBranch + "\nHead: 0123456789abcdef0123456789abcdef01234567\n" +
		"Target: main\nWorker: " + endPolecat + "\nSubmitted: 2026-11-16T18:40:48Z"
	if !strings.Contains(issue.Notes, want) {
		t.Errorf("bead notes = %q, want the seat's block %q", issue.Notes, want)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead labels = %v, want %s", issue.Labels, land.LabelReadyToLand)
	}
}

// A submission brings a new head, so it faces om again: the rework and
// overseer-review labels are removed with the block, as gt done removes them
// (gt-g8t3m).
func TestNoBeadAccessSeatEndReadyBlockRetiresTheReviewLabels(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	if err := f.agentDB.Update(endBead, beads.UpdateOptions{
		AddLabels: []string{land.LabelRework, land.LabelOverseerReviewed},
	}); err != nil {
		t.Fatal(err)
	}
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end did not finish a seat whose outbox it applied")
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{land.LabelRework, land.LabelOverseerReviewed} {
		if beads.HasLabel(issue, label) {
			t.Errorf("bead labels = %v, want %s removed by the submission", issue.Labels, label)
		}
	}
}

// dropReadyLabelClient is the town's bead store writing the block but losing
// the label, the GH#1945 case the read-back exists for: a submission nothing
// picks up must be reported, not recorded as applied.
type dropReadyLabelClient struct {
	beads.Client
}

func (c dropReadyLabelClient) Update(id string, opts beads.UpdateOptions) error {
	opts.AddLabels = nil
	return c.Client.Update(id, opts)
}

// A ready write the store reported but did not persist is reported by the seat
// end: the seat's block is written, and the missing label is the failure the
// log names (gt-qk3ppr).
func TestNoBeadAccessSeatEndReportsAReadyLabelThatDidNotPersist(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return dropReadyLabelClient{Client: f.agentDB}
	}
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end kept a seat for a label its block was written without")
	}
	if !strings.Contains(logs.String(), "does not carry "+land.LabelReadyToLand+" after the write") {
		t.Errorf("log %q does not report the label the write lost", logs.String())
	}
}

// unreadableReadyBeadClient is the town's bead store writing the block but
// failing the read-back, the case the read-back exists for.
type unreadableReadyBeadClient struct {
	beads.Client
	updated bool
}

func (c *unreadableReadyBeadClient) Update(id string, opts beads.UpdateOptions) error {
	c.updated = true
	return c.Client.Update(id, opts)
}

func (c *unreadableReadyBeadClient) Show(id string) (*beads.Issue, error) {
	if c.updated {
		return nil, errors.New("dolt unavailable")
	}
	return c.Client.Show(id)
}

// A bead the store writes and then cannot read back is reported by the seat
// end: the label is not known to be there, so the submission is not taken as
// applied (gt-qk3ppr).
func TestNoBeadAccessSeatEndReportsAReadyWriteItCannotReadBack(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return &unreadableReadyBeadClient{Client: f.agentDB}
	}
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end kept a seat for a read-back its block was written before")
	}
	if !strings.Contains(logs.String(), "its outbox is not applied in full") || !strings.Contains(logs.String(), "reading "+endBead+" back") {
		t.Errorf("log %q does not report the read-back that failed", logs.String())
	}
}

// readyRefusingClient is the town's bead store refusing the ready write.
type readyRefusingClient struct {
	beads.Client
}

func (c readyRefusingClient) Update(id string, opts beads.UpdateOptions) error {
	if len(opts.AddLabels) > 0 {
		return errors.New("dolt unavailable")
	}
	return c.Client.Update(id, opts)
}

// A ready write the store refused is reported with the outbox's other failures
// and leaves the bead unlabelled, rather than reading as a submission
// (gt-qk3ppr).
func TestNoBeadAccessSeatEndReportsARefusedReadyWrite(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	seedSeatBead(t, f, "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return readyRefusingClient{Client: f.agentDB}
	}
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")

	landed := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end kept a seat whose ready write failed")
	}
	if !strings.Contains(logs.String(), "its outbox is not applied in full") || !strings.Contains(logs.String(), "dolt unavailable") {
		t.Errorf("log %q does not report the refused ready write", logs.String())
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead labels = %v, want no %s after a refused write", issue.Labels, land.LabelReadyToLand)
	}
}
