package dashboard

// The deep review's payload, drawn as a block of the Overseer panel: one line
// per configured schedule — every entry of the town's
// daemon.patrols.scheduled_slings — with when that schedule is next due and the
// newest run it filed, and the review's newest runs, which the Usage panel
// joins its own records to by bead id. The page is read-only: a run is
// triggered, paused and edited from a terminal.
//
// The reader that fills this payload lives with the town's other bd readers,
// internal/cmd/dashboard_deepreview.go, for the reason Questions' does: every bd
// read this page makes runs through internal/beads' typed client, which the
// dashboard package does not import.

// A deep-review run's status, off its bead.
const (
	// DeepReviewOpen is a run that has not closed: it is in flight, or its work
	// is still waiting to land.
	DeepReviewOpen = "open"
	// DeepReviewClosed is a run that has finished.
	DeepReviewClosed = "closed"
)

// DeepReviewRun is one deep-review run as the blocks that draw it need it.
//
// It is the same record whether it is a schedule's newest run or one of the
// review's newest over all, so a run carries what either reader draws: the
// block draws a schedule's Status, findings and digest, and the Usage panel
// keys the review's runs by ID and reads Findings.
type DeepReviewRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Findings counts the run's finding: note lines; Filed is those that named a
	// bead rather than "digest-only".
	Findings int `json:"findings"`
	Filed    int `json:"filed"`
	// Digest names the run's digest: the bead id and how many comments it
	// carries. The digest's text is never read into the page.
	Digest string `json:"digest,omitempty"`
	// Unparsed marks a run whose notes carried a finding: or queued: line
	// missing its fields. The run is still drawn, with the counts of the lines
	// that did parse.
	Unparsed bool `json:"unparsed,omitempty"`
}

// DeepReviewSchedule is one configured schedule as the block draws its row: the
// schedule's own name and when it is next due, and the newest run it filed. An
// Unavailable read is named on the DeepReview, not per schedule, since one
// schedule's list failing leaves the block with no picture of the review.
type DeepReviewSchedule struct {
	// Name is the scheduled_slings entry's name — the label its runs carry —
	// and the row's own subject.
	Name string `json:"name"`
	// Rig is the entry's rig, which the row's title names the schedule by.
	Rig string `json:"rig,omitempty"`
	// Next is the schedule's next-run text: "running" while one of its runs is
	// open, the gated line for a schedule that runs only when its gate says
	// there is something to review, "due now" once the interval has passed,
	// "not scheduled" when the schedule's interval is unusable, else
	// "in <duration>".
	Next string `json:"next"`
	// Last is the newest run the schedule filed, nil when it has filed none or
	// when its only beads are failed-sling records.
	Last *DeepReviewRun `json:"last,omitempty"`
}

// DeepReview is the schedules' standing and the review's newest runs.
// Unavailable says the read failed, which the block draws as itself and never
// as an empty list: "no deep review has run" and "the runs could not be read"
// are different things to tell the operator.
type DeepReview struct {
	// Schedules is one entry per configured schedule, in the order the town's
	// config lists them. A town with no scheduled_slings entry to draw — the
	// patrol off, no config, or a config that would not parse — carries none,
	// and the block says so in one line.
	Schedules []DeepReviewSchedule `json:"schedules"`
	// Paused is the reason the town holds dispatch, empty when it does not. A
	// held town starts no run whatever a schedule's next-run line says.
	Paused string `json:"paused,omitempty"`
	// Runs is the review's newest runs across every schedule, newest first,
	// which the Usage panel joins its own records to by bead id. The block
	// draws the schedules' Last runs, not this list.
	Runs        []DeepReviewRun `json:"runs"`
	Unavailable bool            `json:"unavailable,omitempty"`
}
