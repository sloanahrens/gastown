package dashboard

// The Deep review panel's payload: when the next scheduled deep review is due,
// or why the town is holding it, and the last ten runs. The page is read-only —
// a run is triggered, paused and edited from a terminal.
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

// DeepReviewRow is one deep-review run as the pane draws it.
type DeepReviewRow struct {
	ID     string `json:"id"`
	Status string `json:"status"`
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

// DeepReview is the schedule's standing and its last runs, newest first.
// Unavailable says the read failed, which the pane draws as itself and never as
// an empty list: "no deep review has run" and "the runs could not be read" are
// different things to tell the operator.
type DeepReview struct {
	// Next is when the next run is due as the pane draws it: "running" while a
	// run is open, "due now" once the interval has passed, "not scheduled" when
	// the schedule is missing or disabled, else "in <duration>".
	Next string `json:"next,omitempty"`
	// Paused is the reason the town holds dispatch, empty when it does not. A
	// held town starts no run whatever Next says.
	Paused      string          `json:"paused,omitempty"`
	Rows        []DeepReviewRow `json:"rows"`
	Unavailable bool            `json:"unavailable,omitempty"`
}
