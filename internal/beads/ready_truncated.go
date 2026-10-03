package beads

import "fmt"

// ErrReadyTruncated is the sentinel every ready method returns when bd
// reported the page capped. A count that is not the whole board silently
// mis-sizes every consumer built on it (the 2026-09-21 patrol under-report
// that gt-59o9 chased down), so a full page has to be a loud answer, not a
// quiet one (gt-m7pq).
//
// Found is the row count returned; Cap is the limit that bound the query
// (100 — bd's default ready page, or the per-method cap); TrueCount is the
// board size bd reported, and may still sit below the real board if a
// process-level cap (BD_JSON_ENVELOPE off, BEADS_MAX_ROWS) bound it too —
// bd ready's own stderr note, when present, carries the authoritative
// number.
type ErrReadyTruncated struct {
	Found     int
	Cap       int
	TrueCount int
}

func (e *ErrReadyTruncated) Error() string {
	return fmt.Sprintf("ready query capped: %d rows returned against a limit of %d, at least %d exist",
		e.Found, e.Cap, e.TrueCount)
}
