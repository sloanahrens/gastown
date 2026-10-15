package dashboard

// The Overseer panel's payload: whether the overseer session is alive, what it
// has done lately and what kinds of intervention it makes most. The page is
// read-only; the overseer writes the heartbeat and the journal with gt overseer
// beat and gt overseer log, and the daemon's silent-overseer rule watches the
// same heartbeat.
//
// The reader that fills this payload lives with the town's other readers,
// internal/cmd/dashboard_overseer.go: the open-question count is a bd read, and
// the dashboard package does not import internal/beads' typed client.

// The overseer's standing, read off its heartbeat.
const (
	// OverseerAlive is a heartbeat newer than the silence window.
	OverseerAlive = "alive"
	// OverseerSilent is a heartbeat older than the window.
	OverseerSilent = "silent"
	// OverseerNone is a town whose overseer has never beaten: there is no
	// last-active time to draw, and no claim that it is alive.
	OverseerNone = "none"
)

// OverseerLine is one journal line as the panel draws it.
type OverseerLine struct {
	AgeSec int64  `json:"age_sec"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Bead   string `json:"bead,omitempty"`
}

// OverseerCount is how many interventions of one kind the last seven days hold.
type OverseerCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// Overseer is the Overseer panel. Unavailable says a file could not be read,
// which the pane draws as itself and never as a silent overseer or an empty
// journal: "the overseer is quiet" and "its files could not be read" are
// different things to tell the operator.
type Overseer struct {
	Status string `json:"status"`
	// State and Session are the heartbeat's own words.
	State   string `json:"state,omitempty"`
	Session string `json:"session,omitempty"`
	// LastActiveSec is the heartbeat's age, or nil before the first beat.
	LastActiveSec *int64 `json:"last_active_sec,omitempty"`
	// SilentAfterSec is the window Status was judged against.
	SilentAfterSec int64 `json:"silent_after_sec"`
	// Journal is the newest lines, newest first.
	Journal []OverseerLine `json:"journal"`
	// Interventions counts the last seven days by kind, most frequent first.
	Interventions []OverseerCount `json:"interventions"`
	// OpenQuestions is the town's open overseer questions, or nil when the
	// count could not be read.
	OpenQuestions *int `json:"open_questions,omitempty"`
	Unavailable   bool `json:"unavailable,omitempty"`
}
