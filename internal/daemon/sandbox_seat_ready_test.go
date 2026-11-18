package daemon

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

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

// A ready write the store reported but did not persist keeps the seat: the
// block is written, the label is not known to be, and a submission nothing
// picks up is the failure the seat waits on (gt-qk3ppr).
func TestNoBeadAccessSeatEndKeepsTheSeatForAReadyLabelThatDidNotPersist(t *testing.T) {
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
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end finished a seat whose ready label the write lost")
	}
	if !strings.Contains(logs.String(), "does not carry "+land.LabelReadyToLand+" after the write") {
		t.Errorf("log %q does not report the label the write lost", logs.String())
	}
	if !strings.Contains(logs.String(), outboxReadyReason) {
		t.Errorf("log %q does not name the ready write as the reason the seat is kept", logs.String())
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

// A bead the store writes and then cannot read back keeps the seat: the label
// is not known to be there, so the submission is not taken as applied and a
// later pass makes the write again (gt-qk3ppr).
func TestNoBeadAccessSeatEndKeepsTheSeatForAReadyWriteItCannotReadBack(t *testing.T) {
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
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end finished a seat whose ready write it could not read back")
	}
	if !strings.Contains(logs.String(), "its outbox is not applied in full") || !strings.Contains(logs.String(), "reading "+endBead+" back") {
		t.Errorf("log %q does not report the read-back that failed", logs.String())
	}
	if !strings.Contains(logs.String(), outboxReadyReason) {
		t.Errorf("log %q does not name the ready write as the reason the seat is kept", logs.String())
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

// A ready write the store refused is reported with the outbox's other failures,
// leaves the bead unlabelled, and keeps the seat: a submission recorded as
// applied but never marked ready is one nothing lands (gt-qk3ppr).
func TestNoBeadAccessSeatEndKeepsTheSeatForARefusedReadyWrite(t *testing.T) {
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
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landed) {
		t.Fatal("the seat end finished a seat whose ready write it refused")
	}
	if !strings.Contains(logs.String(), "its outbox is not applied in full") || !strings.Contains(logs.String(), "dolt unavailable") {
		t.Errorf("log %q does not report the refused ready write", logs.String())
	}
	if !strings.Contains(logs.String(), outboxReadyReason) {
		t.Errorf("log %q does not name the ready write as the reason the seat is kept", logs.String())
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead labels = %v, want no %s after a refused write", issue.Labels, land.LabelReadyToLand)
	}
}

// failOnceReadyReadClient is the town's bead store writing the ready block and
// failing the first read-back after it, then answering every later read: the
// store of a pass whose label is unknown.
type failOnceReadyReadClient struct {
	beads.Client
	wrote  bool
	failed bool
}

func (c *failOnceReadyReadClient) Update(id string, opts beads.UpdateOptions) error {
	if err := c.Client.Update(id, opts); err != nil {
		return err
	}
	c.wrote = true
	return nil
}

func (c *failOnceReadyReadClient) Show(id string) (*beads.Issue, error) {
	if c.wrote && !c.failed {
		c.failed = true
		return nil, errors.New("dolt unavailable")
	}
	return c.Client.Show(id)
}

// A seat kept because its ready write could not be read back is offered again,
// and the retry it drives writes no second block: the bead carries one READY TO
// LAND block for the submission and the label it was missing (gt-qk3ppr).
func TestNoBeadAccessSeatEndReadyRetryAppendsOneBlock(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	failOnce := &failOnceReadyReadClient{Client: f.agentDB}
	f.host.openRecoveryBeads = func([]string) beads.Client { return failOnce }

	if reason := f.host.applySeatOutbox(endRig, endPolecat, endBead); !strings.HasPrefix(reason, outboxReadyReason) {
		t.Fatalf("first pass reason = %q, want it kept for %q", reason, outboxReadyReason)
	}
	if !strings.Contains(logs.String(), "reading "+endBead+" back") {
		t.Fatalf("log %q does not report the read-back the first pass failed", logs.String())
	}
	if reason := f.host.applySeatOutbox(endRig, endPolecat, endBead); reason != "" {
		t.Fatalf("retry pass kept the seat: %s", reason)
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(issue.Notes, land.ReadyNoteMarker); got != 1 {
		t.Errorf("bead notes carry %d READY TO LAND blocks, want the one the submission wrote: %q", got, issue.Notes)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead labels = %v, want %s after the retry", issue.Labels, land.LabelReadyToLand)
	}
}

// refusedOnceReadyClient refuses the first ready write and answers every later
// one: the store of a pass whose write the bead never got.
type refusedOnceReadyClient struct {
	beads.Client
	refused bool
}

func (c *refusedOnceReadyClient) Update(id string, opts beads.UpdateOptions) error {
	if len(opts.AddLabels) > 0 && !c.refused {
		c.refused = true
		return errors.New("dolt unavailable")
	}
	return c.Client.Update(id, opts)
}

// A seat kept because the store refused the ready write is offered again, and
// the retry it drives writes the block and the label the bead never got: the
// submission is not dropped with the seat (gt-qk3ppr).
func TestNoBeadAccessSeatEndReadyRetryWritesARefusedBlock(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")
	f.host.d.logger = log.New(io.Discard, "", 0)
	refusedOnce := &refusedOnceReadyClient{Client: f.agentDB}
	f.host.openRecoveryBeads = func([]string) beads.Client { return refusedOnce }

	if reason := f.host.applySeatOutbox(endRig, endPolecat, endBead); !strings.HasPrefix(reason, outboxReadyReason) {
		t.Fatalf("first pass reason = %q, want it kept for %q", reason, outboxReadyReason)
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(issue.Notes, land.ReadyNoteMarker) || beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Fatalf("the refused first pass wrote a block or label anyway: %q %v", issue.Notes, issue.Labels)
	}
	if reason := f.host.applySeatOutbox(endRig, endPolecat, endBead); reason != "" {
		t.Fatalf("retry pass kept the seat: %s", reason)
	}
	issue, err = f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(issue.Notes, land.ReadyNoteMarker); got != 1 {
		t.Errorf("bead notes carry %d READY TO LAND blocks, want the one the retry wrote: %q", got, issue.Notes)
	}
	if !beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("bead labels = %v, want %s after the retry", issue.Labels, land.LabelReadyToLand)
	}
}

// unreadableBeforeReadyClient answers the outbox's own read of the bead and
// fails every later one: the store of a pass that cannot read the bead before
// the ready write.
type unreadableBeforeReadyClient struct {
	beads.Client
	shown int
}

func (c *unreadableBeforeReadyClient) Show(id string) (*beads.Issue, error) {
	c.shown++
	if c.shown > 1 {
		return nil, errors.New("dolt unavailable")
	}
	return c.Client.Show(id)
}

// A bead the store cannot read before the ready write gets no block: the store
// refuses rather than appending a block it could not check, and the seat is
// kept for a later pass to make the write (gt-qk3ppr).
func TestNoBeadAccessSeatEndKeepsTheSeatForABeadItCannotReadBeforeTheReadyWrite(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	seedSeatBead(t, f, "")
	writeSeatOutbox(t, f.town, seatOutboxSubmission(), "")
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.host.openRecoveryBeads = func([]string) beads.Client {
		return &unreadableBeforeReadyClient{Client: f.agentDB}
	}

	if reason := f.host.applySeatOutbox(endRig, endPolecat, endBead); !strings.HasPrefix(reason, outboxReadyReason) {
		t.Fatalf("applySeatOutbox reason = %q, want it kept for %q", reason, outboxReadyReason)
	}
	if !strings.Contains(logs.String(), "reading "+endBead+" before the ready write") {
		t.Errorf("log %q does not report the read that failed before the write", logs.String())
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(issue.Notes, land.ReadyNoteMarker) {
		t.Errorf("bead notes = %q, want no block written through a bead the store could not read", issue.Notes)
	}
}

// A bead whose last block is this submission's, whatever stamp it carries, is
// not given a second one: the retry writes the label, not another block. A bead
// whose last block names another head is a different submission and gets this
// one (gt-qk3ppr).
func TestNoBeadAccessSeatEndReadyWriteAddsOneBlockPerSubmission(t *testing.T) {
	t.Parallel()
	submitted := land.Work{
		Branch: endBranch, Head: "0123456789abcdef0123456789abcdef01234567",
		Target: "main", Worker: endPolecat,
	}
	stamped, err := time.Parse(time.RFC3339, "2026-11-16T18:40:48Z")
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := land.Work{Branch: endBranch, Head: "fedcba9876543210fedcba9876543210fedcba98", Target: "main", Worker: endPolecat}
	tests := []struct {
		name       string
		notes      string
		wantBlocks int
	}{
		{
			name:       "a block for this submission with another stamp is not repeated",
			notes:      land.FormatReadyNote(land.Work{Branch: submitted.Branch, Head: submitted.Head, Target: submitted.Target, Worker: submitted.Worker, Submitted: stamped.Add(time.Hour)}),
			wantBlocks: 1,
		},
		{
			name:       "a block that is not this submission's does not answer it",
			notes:      land.FormatReadyNote(elsewhere),
			wantBlocks: 2,
		},
		{
			name:       "a bead with no block gets this one",
			wantBlocks: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSeatEndFake(t)
			seedSeatBead(t, f, "")
			if tt.notes != "" {
				if err := f.agentDB.AppendNotes(endBead, tt.notes); err != nil {
					t.Fatal(err)
				}
			}
			store := &seatOutboxStore{bd: f.agentDB}
			note := land.FormatReadyNote(land.Work{Branch: submitted.Branch, Head: submitted.Head, Target: submitted.Target, Worker: submitted.Worker, Submitted: stamped})
			if err := store.MarkReadyToLand(endBead, note); err != nil {
				t.Fatalf("MarkReadyToLand() error = %v, want nil", err)
			}
			issue, err := f.agentDB.Show(endBead)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(issue.Notes, land.ReadyNoteMarker); got != tt.wantBlocks {
				t.Errorf("bead notes carry %d READY TO LAND blocks, want %d: %q", got, tt.wantBlocks, issue.Notes)
			}
			if !beads.HasLabel(issue, land.LabelReadyToLand) {
				t.Errorf("bead labels = %v, want %s", issue.Labels, land.LabelReadyToLand)
			}
		})
	}
}

// A note the store cannot read as a READY TO LAND block is refused with nothing
// written: a block it cannot check against the bead's own is one it must not
// append (gt-qk3ppr).
func TestNoBeadAccessSeatEndRefusesAReadyNoteItCannotRead(t *testing.T) {
	t.Parallel()
	f := newSeatEndFake(t)
	seedSeatBead(t, f, "")
	store := &seatOutboxStore{bd: f.agentDB}
	err := store.MarkReadyToLand(endBead, land.ReadyNoteMarker+"\nBranch: "+endBranch)
	if err == nil || !strings.Contains(err.Error(), land.ReadyNoteMarker) {
		t.Fatalf("MarkReadyToLand() error = %v, want it to name a block it cannot read", err)
	}
	issue, err := f.agentDB.Show(endBead)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(issue.Notes, land.ReadyNoteMarker) || beads.HasLabel(issue, land.LabelReadyToLand) {
		t.Errorf("the refused note was written anyway: %q %v", issue.Notes, issue.Labels)
	}
}
