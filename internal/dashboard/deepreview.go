package dashboard

// The Deep review panel's payload: when each scheduled deep review is next due,
// or why the town is holding them, and the last ten runs. The page is read-only
// — a run is triggered, paused and edited from a terminal.
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

// A deep-review run's kind: the schedule that filed it, named by its label.
const (
	// DeepReviewKindSlice is a run of the rotating slice schedule.
	DeepReviewKindSlice = "slice"
	// DeepReviewKindRisk is a run of the risk-path schedule.
	DeepReviewKindRisk = "risk"
	// DeepReviewKindLegacy is a run filed under the single schedule the review
	// ran as before the split; the label ages out as those runs leave the pane.
	DeepReviewKindLegacy = "legacy"
)

// DeepReviewRow is one deep-review run as the pane draws it.
type DeepReviewRow struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// Kind is the schedule that filed the run: one of the DeepReviewKind
	// values. Two schedules file runs under labels the pane cannot tell apart
	// by look, so the reader carries the kind alongside.
	Kind string `json:"kind"`
	// DurationSec is how long the run took, off the bead's own creation and
	// close times, and is nil for a run still open or whose times the reader
	// cannot parse: a duration of zero would read as "finished instantly".
	DurationSec *int64 `json:"duration_sec,omitempty"`
	// Findings counts the run's finding: note lines; Filed is those that named a
	// bead rather than "digest-only"; Queued counts its queued: lines.
	Findings int `json:"findings"`
	Filed    int `json:"filed"`
	Queued   int `json:"queued"`
	// Digest names the run's digest: the bead id and how many comments it
	// carries. The digest's text is never read into the page.
	Digest string `json:"digest,omitempty"`
	// Unparsed marks a run whose notes carried a finding:, queued: or cursor:
	// line missing its fields. The row is still drawn, with the counts of the
	// lines that did parse.
	Unparsed bool `json:"unparsed,omitempty"`
}

// DeepReviewNext is one configured schedule's next-run line: the schedule's own
// name, and what the pane draws beside it. The review runs on two schedules
// since the split, so one line could not say when either is next due.
type DeepReviewNext struct {
	// Name is the scheduled_slings entry's name, empty for the one entry a town
	// with no schedule at all carries.
	Name string `json:"name,omitempty"`
	// Text is "running" while one of the schedule's runs is open, the gated
	// line for a schedule that runs only when its gate says there is something
	// to review, "due now" once the interval has passed, "not scheduled" when
	// the schedule's interval is unusable, else "in <duration>".
	Text string `json:"text"`
}

// DeepReview is the schedules' standing and the last runs, newest first.
// Unavailable says the read failed, which the pane draws as itself and never as
// an empty list: "no deep review has run" and "the runs could not be read" are
// different things to tell the operator.
type DeepReview struct {
	// Next is one entry per configured schedule, in the order the pane draws
	// them: the schedules were split in two, and a single line could not say
	// when either is next due. A town with no schedule configured carries one
	// nameless entry saying so.
	Next []DeepReviewNext `json:"next,omitempty"`
	// Paused is the reason the town holds dispatch, empty when it does not. A
	// held town starts no run whatever Next says.
	Paused      string          `json:"paused,omitempty"`
	Rows        []DeepReviewRow `json:"rows"`
	Unavailable bool            `json:"unavailable,omitempty"`
}
