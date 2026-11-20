package dashboard

import "time"

// The Dolt panel is the operator's view of the town's Dolt server: one section
// per reading, each a small table or the reason its reading failed. The hub
// carries it as State.Dolt; the readers that fill the sections are wired in
// Config.Dolt.

// DoltSection is one section of the Dolt panel: a key naming the reading, its
// title, an optional note, the failure that left it empty, and its table.
type DoltSection struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// Note is the section's one-line explanation, empty when the reading
	// needs none beyond its table.
	Note string `json:"note,omitempty"`
	// Err is the reason the reading failed. It is empty for a reading that
	// worked, including one that found nothing to show; a section with Err
	// carries no Head or Rows.
	Err string `json:"err,omitempty"`
	// Head is the table's column headers.
	Head []string `json:"head,omitempty"`
	// Rows is the table's body, each row one cell per Head entry.
	Rows [][]string `json:"rows,omitempty"`
}

// DoltPanel is the Dolt panel as one read produced it: the instant its
// readings were taken and their sections in the order they are carried.
type DoltPanel struct {
	At       time.Time     `json:"at,omitzero"`
	Sections []DoltSection `json:"sections"`
}
