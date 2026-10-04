package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// dashQueueStore is what the queue reader needs from one beads store, so the
// join is testable without a database.
type dashQueueStore interface {
	ReadyAll() ([]*beads.Issue, error)
	Blocked() ([]*beads.Issue, error)
	List(beads.ListOptions) ([]*beads.Issue, error)
	Show(id string) (*beads.Issue, error)
}

var _ dashQueueStore = (*beads.Beads)(nil)

// dashQueueReader reads the work queue from the town's stores: the town store
// ("hq") and one per rig.
type dashQueueReader struct {
	townRoot string
	template specdispatch.Template                               // the shape the dispatcher lints against
	parked   func(rig string) bool                               // whether a store's rig is parked (nil: none is)
	stores   func() (map[string]dashQueueStore, []string, error) // store by name, in order
}

func newDashQueueReader(townRoot string) *dashQueueReader {
	r := &dashQueueReader{townRoot: townRoot, template: specdispatch.LoadTemplate(specdispatch.DefaultTemplatePath()),
		parked: func(rig string) bool { return rig != "hq" && IsRigParked(townRoot, rig) }}
	r.stores = func() (map[string]dashQueueStore, []string, error) {
		rigs, err := knownRigNames(townRoot)
		if err != nil {
			return nil, nil, err
		}
		names := append([]string{"hq"}, rigs...)
		out := map[string]dashQueueStore{}
		for _, n := range names {
			if dir := doltserver.FindRigBeadsDir(townRoot, n); dir != "" {
				out[n] = beads.NewWithBeadsDir(townRoot, dir)
			}
		}
		return out, names, nil
	}
	return r
}

// dashQueueRow is one issue as a queue row.
func dashQueueRow(rig string, i *beads.Issue) dashboard.QueueBead {
	row := dashboard.QueueBead{
		ID: i.ID, Rig: rig, Title: i.Title, Type: i.Type, Status: i.Status,
		Assignee: i.Assignee, Priority: i.Priority, Labels: i.Labels, BlockedBy: i.BlockedBy,
	}
	if t, err := time.Parse(time.RFC3339, i.CreatedAt); err == nil {
		row.CreatedAt = t
	}
	if len(row.BlockedBy) > 5 {
		row.BlockedBy = row.BlockedBy[:5]
	}
	return row
}

// dashShape runs the dispatcher's shape lint on a bead and names the verdict.
// The lint is a pure function of the bead's text, so it costs no subprocess.
func dashShape(t specdispatch.Template, i *beads.Issue) (shape, note string) {
	v := specdispatch.Lint(specdispatch.Spec{
		ID: i.ID, Title: i.Title, Type: i.Type, Status: i.Status, Assignee: i.Assignee, Priority: i.Priority,
		CreatedAt: i.CreatedAt, Labels: i.Labels, Description: i.Description, Design: i.Design, Notes: i.Notes,
		Acceptance: i.AcceptanceCriteria, Ephemeral: i.Ephemeral,
	}, t)
	switch {
	case v.Clean():
		return "ok", ""
	case v.Route == specdispatch.RoutePlanning:
		return "planning", v.Reason
	case v.Field == "not a work bead":
		return "other", v.Reason
	}
	return "fix", strings.TrimSpace(v.Field + ": " + v.Reason)
}

// read is the queue's whole reading. A store that cannot be read is named in
// Unreadable and contributes nothing; the other stores still show.
func (r *dashQueueReader) read(now time.Time) *dashboard.Queue {
	stores, names, err := r.stores()
	if err != nil {
		return nil
	}
	q := &dashboard.Queue{At: now}
	var ready, landing, blocked []dashboard.QueueBead
	for _, name := range names {
		st := stores[name]
		if st == nil {
			continue
		}
		rd, err1 := st.ReadyAll()
		bl, err2 := st.Blocked()
		ln, err3 := st.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
		if err1 != nil || err2 != nil || err3 != nil {
			q.Unreadable = append(q.Unreadable, name)
			continue
		}
		parked := r.parked != nil && r.parked(name)
		if parked {
			q.ParkedRigs = append(q.ParkedRigs, name)
		}
		for _, i := range rd {
			row := dashQueueRow(name, i)
			row.RigParked = parked
			row.Shape, row.ShapeNote = dashShape(r.template, i)
			if parked && row.Shape == "ok" {
				// well shaped, but the dispatcher does not serve a parked rig
				row.Shape, row.ShapeNote = "parked", "the rig is parked: the dispatcher does not serve it"
			}
			ready = append(ready, row)
		}
		for _, i := range bl {
			blocked = append(blocked, dashQueueRow(name, i))
		}
		for _, i := range ln {
			if beads.HasLabel(i, land.LabelReadyToLand) && beads.IssueStatus(i.Status).IsActionable() {
				landing = append(landing, dashQueueRow(name, i))
			}
		}
	}
	// A bead the landing worker is about to take is not also waiting to be built.
	inLanding := map[string]bool{}
	for _, b := range landing {
		inLanding[b.ID] = true
	}
	kept := ready[:0]
	for _, b := range ready {
		if !inLanding[b.ID] {
			kept = append(kept, b)
		}
	}
	for _, rows := range []*[]dashboard.QueueBead{&kept, &landing, &blocked} {
		dashboard.SortQueueRows(*rows)
	}
	q.Ready, q.ReadyTotal, q.ReadyRigs = dashboard.CapQueueRows(kept)
	q.Landing, q.LandingTotal, q.LandingRigs = dashboard.CapQueueRows(landing)
	q.Blocked, q.BlockedTotal, q.BlockedRigs = dashboard.CapQueueRows(blocked)
	sort.Strings(q.Unreadable)
	sort.Strings(q.ParkedRigs)
	return q
}

// detail reads one bead's text from the store named by rig. The rig must be one
// of the town's stores: it picks a directory, so an arbitrary name is refused.
func (r *dashQueueReader) detail(rig, id string) (*dashboard.BeadDetail, error) {
	stores, _, err := r.stores()
	if err != nil {
		return nil, err
	}
	st := stores[rig]
	if st == nil {
		return nil, fmt.Errorf("no store %q", rig)
	}
	i, err := st.Show(id)
	if err != nil || i == nil {
		return nil, fmt.Errorf("reading %s: %v", id, err)
	}
	d := &dashboard.BeadDetail{
		ID: i.ID, Rig: rig, Title: i.Title, Type: i.Type, Status: i.Status, Assignee: i.Assignee,
		Priority: i.Priority, Labels: i.Labels, Description: i.Description, Design: i.Design,
		Acceptance: i.AcceptanceCriteria, Notes: i.Notes, BlockedBy: i.BlockedBy, DependsOn: i.DependsOn,
	}
	if t, err := time.Parse(time.RFC3339, i.CreatedAt); err == nil {
		d.CreatedAt = t
	}
	if t, err := time.Parse(time.RFC3339, i.UpdatedAt); err == nil {
		d.UpdatedAt = t
	}
	for _, c := range i.Comments {
		dc := dashboard.DetailComment{Author: c.Author, Text: strings.TrimSpace(c.Text)}
		if t, err := time.Parse(time.RFC3339, c.CreatedAt); err == nil {
			dc.At = t
		}
		d.Comments = append(d.Comments, dc)
	}
	return d, nil
}
