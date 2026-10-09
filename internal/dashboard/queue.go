package dashboard

import (
	"errors"
	"regexp"
	"sort"
	"time"
)

// The work queue: the beads waiting to be built, waiting to land, or blocked,
// and, on request, one bead's full text. The lists come from one read per
// store per minute while a page is open; the text of a bead is read only when
// someone opens it, and never more than one at a time.

const (
	queueMaxRows     = 40 // rows kept per store per list; the totals stay exact
	detailTTL        = 30 * time.Second
	detailCacheMax   = 128
	detailDescMax    = 20000 // runes of description/design/acceptance kept
	detailNotesMax   = 8000
	detailCommentMax = 2000
	detailComments   = 10
)

// QueueBead is one row of the work queue.
type QueueBead struct {
	ID        string    `json:"id"`
	Rig       string    `json:"rig"` // the store the bead lives in; "hq" for the town store
	Title     string    `json:"title"`
	Type      string    `json:"type,omitempty"`
	Status    string    `json:"status,omitempty"`
	Assignee  string    `json:"assignee,omitempty"`
	Priority  int       `json:"priority"`
	Labels    []string  `json:"labels,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// UpdatedAt is the bead's last write: for a bead waiting to land, the label
	// write gt done makes is normally its last write, so this is when the wait
	// started, which is how the Landings pane ages it.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	BlockedBy []string  `json:"blocked_by,omitempty"`
	// RigParked is true when the store's rig is parked: the dispatcher does not
	// serve a parked rig, so none of its beads is dispatchable however well shaped.
	RigParked bool `json:"rig_parked,omitempty"`
	// Shape is the spec dispatcher's own verdict on the bead, from the same
	// lint it runs before it allocates a seat: "ok" (it would slot it), "fix"
	// (a required field is missing; ShapeNote names the first), "planning"
	// (it would go to the planner) or "other" (an epic or runtime record).
	// "parked" and "held" are the dispatcher's exclusions rather than the
	// lint's: the rig is parked, or an assignee already holds the bead. Empty
	// for rows the lint was not run on.
	Shape     string `json:"shape,omitempty"`
	ShapeNote string `json:"shape_note,omitempty"`
}

// Queue is the three lists. A list is capped at queueMaxRows rows, and its
// total says how many there really are.
type Queue struct {
	At           time.Time   `json:"at"`
	Ready        []QueueBead `json:"ready"`
	ReadyTotal   int         `json:"ready_total"`
	Landing      []QueueBead `json:"landing"`
	LandingTotal int         `json:"landing_total"`
	Blocked      []QueueBead `json:"blocked"`
	BlockedTotal int         `json:"blocked_total"`
	// The exact number of beads per store in each list, which the capped rows
	// above cannot give: the page's filter chips read these.
	ReadyRigs   map[string]int `json:"ready_rigs"`
	LandingRigs map[string]int `json:"landing_rigs"`
	BlockedRigs map[string]int `json:"blocked_rigs"`
	// Unreadable names the stores whose lists could not be read, so a store
	// that is down does not read as an empty queue.
	Unreadable []string `json:"unreadable,omitempty"`
	// ParkedRigs names the stores whose rigs are parked.
	ParkedRigs []string `json:"parked_rigs,omitempty"`
	// Prefixes maps each store's beads prefix to the store's name, so the page
	// can turn a bead id it is handed as text — one written in a report — into
	// the store that owns it and open it. A prefix no store claims is absent,
	// which leaves its ids as the plain text they were written as.
	Prefixes map[string]string `json:"prefixes,omitempty"`
	// Rigs is one row per known rig, in registry order, as the Rigs panel
	// draws it: the rig's park state and, for a store this read reached, its
	// ready and landing counts. The seat count is not the store's to know, so
	// the hub fills it and publishes the joined rows as State.Rigs; this field
	// is the join's input, not part of the page's payload.
	Rigs []Rig `json:"-"`
}

// SortQueueRows orders rows the way the dispatcher takes them: most urgent
// priority first, oldest first within a priority, id as the tiebreak.
func SortQueueRows(rows []QueueBead) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// CapQueueRows keeps the first queueMaxRows rows of each store, in the order
// given, and reports the true total per store and overall. Capping per store
// keeps one busy store from crowding the others out of the list.
func CapQueueRows(rows []QueueBead) (kept []QueueBead, total int, perRig map[string]int) {
	perRig = map[string]int{}
	shown := map[string]int{}
	kept = []QueueBead{}
	for _, r := range rows {
		perRig[r.Rig]++
		if shown[r.Rig] < queueMaxRows {
			shown[r.Rig]++
			kept = append(kept, r)
		}
	}
	return kept, len(rows), perRig
}

// DetailComment is one comment on a bead.
type DetailComment struct {
	Author string    `json:"author"`
	At     time.Time `json:"at,omitzero"`
	Text   string    `json:"text"`
}

// BeadDetail is what opening a queue row shows.
type BeadDetail struct {
	ID          string          `json:"id"`
	Rig         string          `json:"rig"`
	Title       string          `json:"title"`
	Type        string          `json:"type,omitempty"`
	Status      string          `json:"status,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	Priority    int             `json:"priority"`
	Labels      []string        `json:"labels,omitempty"`
	CreatedAt   time.Time       `json:"created_at,omitzero"`
	UpdatedAt   time.Time       `json:"updated_at,omitzero"`
	Description string          `json:"description,omitempty"`
	Design      string          `json:"design,omitempty"`
	Acceptance  string          `json:"acceptance,omitempty"`
	Notes       string          `json:"notes,omitempty"`
	BlockedBy   []string        `json:"blocked_by,omitempty"`
	DependsOn   []string        `json:"depends_on,omitempty"`
	Comments    []DetailComment `json:"comments,omitempty"`
	// Truncated is true when any text was cut to keep the response small.
	Truncated bool `json:"truncated,omitempty"`
}

// ClipText keeps at most max runes of s and reports whether it cut.
func ClipText(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]) + "…", true
}

// ClipDetail bounds every text field of d and records whether it cut any.
func ClipDetail(d *BeadDetail) {
	cut := func(s *string, max int) {
		var c bool
		*s, c = ClipText(*s, max)
		d.Truncated = d.Truncated || c
	}
	cut(&d.Description, detailDescMax)
	cut(&d.Design, detailDescMax)
	cut(&d.Acceptance, detailDescMax)
	cut(&d.Notes, detailNotesMax)
	if len(d.Comments) > detailComments {
		d.Comments = d.Comments[len(d.Comments)-detailComments:]
		d.Truncated = true
	}
	for i := range d.Comments {
		cut(&d.Comments[i].Text, detailCommentMax)
	}
}

var (
	beadIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	beadRigRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
)

// ValidBeadRef reports whether rig and id are shaped like a store name and a
// bead id. It is the only thing between a query string and a bd argument.
func ValidBeadRef(rig, id string) bool {
	return beadRigRe.MatchString(rig) && beadIDRe.MatchString(id)
}

var errDetailUnavailable = errors.New("bead unavailable")

type detailEntry struct {
	at time.Time
	d  *BeadDetail
}

// beadDetail returns one bead's text, from a 30-second cache or, one read at a
// time, from the reader. A failed read is not cached.
func (h *Hub) beadDetail(rig, id string) (*BeadDetail, error) {
	if h.cfg.Bead == nil || !ValidBeadRef(rig, id) {
		return nil, errDetailUnavailable
	}
	key := rig + "/" + id
	h.detailMu.Lock()
	defer h.detailMu.Unlock() // serializes reads: a flurry of clicks is one bd at a time
	now := h.cfg.Now()
	if e, ok := h.detailCache[key]; ok && now.Sub(e.at) < detailTTL {
		return e.d, nil
	}
	d, err := h.cfg.Bead(rig, id)
	if err != nil || d == nil {
		return nil, errDetailUnavailable
	}
	ClipDetail(d)
	if h.detailCache == nil {
		h.detailCache = map[string]detailEntry{}
	}
	if len(h.detailCache) >= detailCacheMax {
		oldest, oldestAt := "", now
		for k, e := range h.detailCache {
			if oldest == "" || e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(h.detailCache, oldest)
	}
	h.detailCache[key] = detailEntry{at: now, d: d}
	return d, nil
}
