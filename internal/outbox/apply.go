package outbox

import (
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// Store is the part of the bead store that Apply writes an outbox through. The
// host supplies the real store; the tests supply a fake.
type Store interface {
	// AppendNotes appends text to beadID's notes.
	AppendNotes(beadID, text string) error
	// SetTitle replaces beadID's title.
	SetTitle(beadID, title string) error
	// SetAcceptance replaces beadID's acceptance text.
	SetAcceptance(beadID, text string) error
	// AddComment adds a comment from author to beadID.
	AddComment(beadID, author, text string) error
	// SetStatus sets beadID's status to status.
	SetStatus(beadID, status string) error
	// Escalate raises an escalation on beadID from actor with text as its
	// request.
	Escalate(beadID, actor, text string) error
}

// ReadyStore is the part of the bead store that marks a bead ready to land,
// which is what a submitted outbox asks for. It is separate from Store because
// only a result that submitted a branch writes it: a store that handles an
// outbox but cannot write the block refuses the submission through Apply's
// error rather than dropping it (gt-qk3ppr).
type ReadyStore interface {
	// MarkReadyToLand appends note, a READY TO LAND block, to beadID's notes
	// and adds the label the landing worker picks the bead by, in one write,
	// as gt done marks its own submission.
	MarkReadyToLand(beadID, note string) error
}

// Seat is the host's identity for the seat an outbox is applied for. Actor is
// the address the seat's comments and escalation are written under. Worker is
// the bare name the READY TO LAND block's Worker line carries: the host names
// the seat, so a result.json cannot claim another worker's submission.
type Seat struct {
	Actor  string
	Worker string
}

// Apply writes the parts of r that change the bead through store: the seat's
// title, its notes, its acceptance (the block it replaced or the boxes it
// ticked), its findings, a deferred status, its escalation request and its
// submission. seat.Actor is the author every finding is commented under and the
// one the escalation is raised from.
//
// A deferred result sets the bead's status to deferred, after the other writes.
// A done result sets no status: the landing path owns the status of a task it
// lands.
//
// A done result that carries a submission marks the bead ready to land last,
// after the escalation: Apply writes the READY TO LAND block with branch, head
// and target from the submission, Worker from seat and Submitted from the
// seat's own stamp, using the builder gt done writes its block with
// (gt-kepije). That block and the ready label in the one store write are what
// the landing worker finds the bead by.
//
// The writes run in that order, and every one is attempted even after an
// earlier one failed, so one failed write neither hides the writes after it nor
// reads as success: Apply returns all the failures joined, and nil only when
// every write it made succeeded. A status other than StatusDone or
// StatusDeferred, which Validate does not return, is refused into that joined
// error with no status written: a hand-built Result must not skip the status
// and read as a clean run. An escalation whose text is empty is refused the
// same way, with no escalation raised, and so is a title or an acceptance that
// is empty: either would replace what the bead has with nothing. A submission
// whose branch, head or target is empty or that names a branch git would not
// accept or a head that is not a commit sha gets no block, and neither does one
// that arrives with a status other than StatusDone: a deferred seat submits
// nothing.
//
// r.Acceptance, when set, is the block that replaces the bead's, and r.Ticked
// holds zero-based indexes into it. When it is not set the ticks index into
// acceptance, the block the host read. The indexes are read in order by
// beads.AcceptanceBoxes: an index outside that list is an error and no
// acceptance text is written for it. The acceptance is written only when the
// replacement or the ticks change the block the host read, and a note only when
// r.Notes is not empty.
func Apply(store Store, beadID string, seat Seat, acceptance string, r Result) error {
	var errs []error
	if r.Title != nil {
		if strings.TrimSpace(*r.Title) == "" {
			errs = append(errs, fmt.Errorf("outbox: set title of %s: the result's title is empty", beadID))
		} else if err := store.SetTitle(beadID, *r.Title); err != nil {
			errs = append(errs, fmt.Errorf("outbox: set title of %s: %w", beadID, err))
		}
	}
	if r.Notes != "" {
		if err := store.AppendNotes(beadID, r.Notes); err != nil {
			errs = append(errs, fmt.Errorf("outbox: append notes to %s: %w", beadID, err))
		}
	}
	block := acceptance
	if r.Acceptance != nil {
		block = *r.Acceptance
	}
	if r.Acceptance != nil && strings.TrimSpace(block) == "" {
		errs = append(errs, fmt.Errorf("outbox: set acceptance of %s: the result's acceptance is empty, so the criteria are not replaced", beadID))
	} else if updated, err := ticked(block, r.Ticked); err != nil {
		errs = append(errs, fmt.Errorf("outbox: tick acceptance of %s: %w", beadID, err))
	} else if updated != acceptance {
		if err := store.SetAcceptance(beadID, updated); err != nil {
			errs = append(errs, fmt.Errorf("outbox: set acceptance of %s: %w", beadID, err))
		}
	}
	for i, finding := range r.Findings {
		if err := store.AddComment(beadID, seat.Actor, finding); err != nil {
			errs = append(errs, fmt.Errorf("outbox: add finding %d to %s: %w", i, beadID, err))
		}
	}
	switch r.Status {
	case StatusDone:
		// The landing path sets the status of a task it lands.
	case StatusDeferred:
		if err := store.SetStatus(beadID, string(StatusDeferred)); err != nil {
			errs = append(errs, fmt.Errorf("outbox: set status of %s: %w", beadID, err))
		}
	default:
		errs = append(errs, fmt.Errorf("outbox: set status of %s: result status %q is neither %q nor %q", beadID, r.Status, StatusDone, StatusDeferred))
	}
	if r.Escalation != nil {
		// An empty request is the one Escalation that Validate refuses; raising it
		// would create an escalation bead carrying no text (gt-5rmlks).
		if r.Escalation.Text == "" {
			errs = append(errs, fmt.Errorf("outbox: escalate on %s: escalation text is empty", beadID))
		} else if err := store.Escalate(beadID, seat.Actor, r.Escalation.Text); err != nil {
			errs = append(errs, fmt.Errorf("outbox: escalate on %s: %w", beadID, err))
		}
	}
	if r.Submission != nil {
		if err := MarkReady(store, beadID, seat, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MarkReady marks beadID ready to land for r's submission. It refuses, with the
// block unwritten, what Validate refuses and what a hand-built Result can still
// carry: a status other than StatusDone, an empty branch, head or target, and a
// branch or head git would not accept. The Worker line is seat.Worker, never a
// value from the outbox. The block is written through the store's ReadyStore: a
// store that cannot write it refuses the submission rather than dropping it.
//
// Apply calls it for its last write. A host that recorded an earlier pass as
// having left the write undone calls it to make just that one, with the same
// refusals and the same block (gt-qk3ppr).
func MarkReady(store Store, beadID string, seat Seat, r Result) error {
	if err := submissionProblem(r.Submission, r.Status); err != nil {
		return fmt.Errorf("outbox: mark %s ready to land: %w", beadID, err)
	}
	ready, ok := store.(ReadyStore)
	if !ok {
		return fmt.Errorf("outbox: mark %s ready to land: the store cannot write a %s block", beadID, land.ReadyNoteMarker)
	}
	note := land.FormatReadyNote(land.Work{
		Branch:    r.Submission.Branch,
		Head:      r.Submission.Head,
		Target:    r.Submission.Target,
		Worker:    seat.Worker,
		Submitted: r.Submission.SubmittedAt,
	})
	if err := ready.MarkReadyToLand(beadID, note); err != nil {
		return fmt.Errorf("outbox: mark %s ready to land: %w", beadID, err)
	}
	return nil
}

// ticked returns acceptance with the checkbox lines named by ticks ticked.
// ticks are zero-based indexes into the checkbox lines of acceptance, read in
// order by beads.AcceptanceBoxes, which carries each box's own line, so a box
// is always ticked where it sits. Only the " " of a box changes: every other
// byte is kept, and a box that is already ticked is left alone. An index outside
// the list is an error and no text is returned for the acceptance.
func ticked(acceptance string, ticks []int) (string, error) {
	boxes := beads.AcceptanceBoxes(acceptance)
	lines := strings.Split(acceptance, "\n")
	changed := false
	for _, i := range ticks {
		if i < 0 || i >= len(boxes) {
			return "", fmt.Errorf("tick index %d is not one of the %d checkbox lines", i, len(boxes))
		}
		box := boxes[i]
		if box.Checked {
			continue
		}
		line := lines[box.Line]
		lines[box.Line] = line[:box.At+1] + "x" + line[box.At+2:]
		boxes[i].Checked = true
		changed = true
	}
	if !changed {
		return acceptance, nil
	}
	return strings.Join(lines, "\n"), nil
}
