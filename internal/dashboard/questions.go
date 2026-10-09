package dashboard

// The Questions panel's payload: the questions the overseer has filed for
// Sloan, so he can see what is waiting on him and answer it from a terminal.
// The page stays read-only — there is no form and no write path, and Sloan
// answers by commenting on the bead.
//
// The reader that fills this payload lives with the town's other bd readers,
// internal/cmd/dashboard_questions.go: every bd read this page makes runs
// through internal/beads' typed client, which the dashboard package does not
// import.

// A question's state, read from its newest comment.
const (
	// QuestionAwaiting is the overseer's own newest comment, or no comment at
	// all: the question is still Sloan's to answer.
	QuestionAwaiting = "awaiting"
	// QuestionAnswered is a newest comment that is not the overseer's, which is
	// Sloan's reply, and the overseer is acting on it.
	QuestionAnswered = "answered"
)

// QuestionRow is one open overseer question as the pane draws it.
type QuestionRow struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Question and Recommended are the description's ## Question and
	// ## Recommended answer sections, each as one line, or empty when the
	// description does not carry them.
	Question    string `json:"question,omitempty"`
	Recommended string `json:"recommended,omitempty"`
	// Held are the bead ids the ## Held work section names.
	Held []string `json:"held,omitempty"`
	// AgeSec is how long ago the overseer asked, off the bead's own creation
	// time, and is nil for a bead whose time the reader cannot parse: an age of
	// zero would read as "asked just now".
	AgeSec *int64 `json:"age_sec,omitempty"`
	// Comments is how many comments the bead carries.
	Comments int `json:"comments"`
	// State is awaiting while the newest comment is the overseer's own, and
	// answered once Sloan has replied.
	State string `json:"state"`
	// Malformed marks a description without both sections the pane draws. The
	// row is still shown, with the title alone: a question the operator cannot
	// answer from the page is still one he should know about.
	Malformed bool `json:"malformed,omitempty"`
}

// Questions is the town's open overseer questions, oldest first. Unavailable
// says the read failed, which the pane draws as itself and never as an empty
// list: "the overseer has asked nothing" and "the questions could not be read"
// are different things to tell the operator.
type Questions struct {
	Rows        []QuestionRow `json:"rows"`
	More        int           `json:"more,omitempty"`
	Unavailable bool          `json:"unavailable,omitempty"`
}
