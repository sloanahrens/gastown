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
	ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error)
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
	prefixes func(names []string) map[string]string              // beads prefix to store name
	verdicts func() (*specdispatch.Ledger, error)                // the dispatcher's verdict ledger (nil: none to read)
}

// storePrefixes maps each store's beads prefix back to the store's name, so a
// bead id the page is handed as text can be opened. The town's routes file
// records every rig's prefix; the town store itself is the "." route and its
// ids carry "hq" like the rest of the town's beads.
func storePrefixes(townRoot string, names []string) map[string]string {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	out := map[string]string{}
	routes, err := beads.LoadRoutes(beads.GetTownBeadsPath(townRoot))
	if err != nil {
		return out
	}
	for _, rt := range routes {
		prefix := strings.TrimSuffix(rt.Prefix, "-")
		rig := strings.SplitN(rt.Path, "/", 2)[0]
		if rig == "." || rig == "" {
			rig = "hq"
		}
		// A prefix a store the reader does not carry claims would only offer a
		// link nothing can open, so it stays out.
		if prefix != "" && known[rig] {
			out[prefix] = rig
		}
	}
	return out
}

func newDashQueueReader(townRoot string) *dashQueueReader {
	r := &dashQueueReader{townRoot: townRoot, template: specdispatch.LoadTemplate(specdispatch.DefaultTemplatePath()),
		parked:   func(rig string) bool { return rig != "hq" && IsRigParked(townRoot, rig) },
		prefixes: func(names []string) map[string]string { return storePrefixes(townRoot, names) },
		verdicts: func() (*specdispatch.Ledger, error) {
			return specdispatch.LoadLedger(specdispatch.LedgerPath(townRoot))
		}}
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

// addLiveMolecules adds to live the molecules a seat is running: the attached
// molecule of every work bead that is hooked or in progress and has an
// assignee. A bead that is closed, or that nobody holds, keeps its molecule out
// of the set, which is what makes the molecule's steps leaked (gt-xmys9). The
// assignee is the seat's claim on the bead; whether that seat's session is
// still up is not read here, the queue read having no per-seat probe to spend.
func addLiveMolecules(live map[string]bool, held []*beads.Issue) {
	for _, i := range held {
		if i.Assignee == "" {
			continue
		}
		if att := beads.ParseAttachmentFields(i); att != nil && att.AttachedMolecule != "" {
			live[att.AttachedMolecule] = true
		}
	}
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
	if t, err := time.Parse(time.RFC3339, i.UpdatedAt); err == nil {
		row.UpdatedAt = t
	}
	if len(row.BlockedBy) > 5 {
		row.BlockedBy = row.BlockedBy[:5]
	}
	return row
}

// dashShape runs the dispatcher's shape lint on a bead and names the verdict.
// It is LintShape, not Lint: the badge describes the bead's text, so it costs
// no subprocess and asks nothing of the git tree the target-path check reads
// (gt-7t1im).
func dashShape(t specdispatch.Template, i *beads.Issue) (shape, note string) {
	v := specdispatch.LintShape(specdispatch.Spec{
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

// dashVerdictTTL is how old the verdict ledger may be before the page must not
// present its verdicts as current: a held or stopped dispatcher stops writing,
// so its last tick's verdicts read as fresh would be a claim nothing backs
// (gt-ckncx).
const dashVerdictTTL = 5 * time.Minute

// dashVerdicts is one queue read's view of the dispatch ledger: the verdicts to
// overlay and when they were taken. An empty view (no ledger, or one with no
// tick in it) has a zero at and overlays nothing; an unreadable one overlays
// nothing either and is stale, because its verdicts are not ones the page can
// present as current.
type dashVerdicts struct {
	byBead map[string]specdispatch.Verdict
	at     time.Time
	stale  bool
}

// readVerdicts loads the dispatcher's last published verdicts for one queue
// read. A ledger this read cannot load overlays nothing and is reported stale:
// with the dispatcher's own record unread, the page must not offer the lint's
// shape as the dispatcher's truth. A ledger with no tick in it (no file yet, so
// nothing to report) overlays nothing and sets no field.
func (r *dashQueueReader) readVerdicts(now time.Time) dashVerdicts {
	if r.verdicts == nil {
		return dashVerdicts{}
	}
	l, err := r.verdicts()
	if err != nil {
		return dashVerdicts{stale: true}
	}
	if l == nil || l.SavedAt.IsZero() {
		return dashVerdicts{}
	}
	return dashVerdicts{byBead: l.Verdicts, at: l.SavedAt, stale: now.Sub(l.SavedAt) > dashVerdictTTL}
}

// dashOverlayVerdict puts the dispatcher's own last verdict on a ready row
// (gt-ckncx). The lint's "ok" says the bead's text is well formed, not that the
// dispatcher took it: a bead whose named file is absent from main, or that sits
// behind a bead in progress, lints clean and is still passed over. So a row the
// ledger says was skipped, refused or deferred stops wearing "ok" — it becomes
// "skipped", its note the tick's reason — and the page's dispatchable count is
// the dispatcher's truth rather than a second derivation. A row whose shape
// already decides against it ("fix", "held", "parked", "planning", "other")
// keeps that shape and still carries the verdict's fields, so the page can say
// why the dispatcher passed it over; a planning or failed class fills the
// fields and changes no shape.
func dashOverlayVerdict(row *dashboard.QueueBead, verdicts map[string]specdispatch.Verdict) {
	v, ok := verdicts[row.ID]
	if !ok {
		return
	}
	row.SkipClass, row.SkipReason = v.Class, v.Reason
	row.SkippedSince, row.SkipTicks = v.FirstAt, v.Ticks
	if row.Shape != "ok" {
		return
	}
	switch v.Class {
	case specVerdictSkipped, specVerdictRefused, specVerdictDeferred:
		row.Shape, row.ShapeNote = "skipped", v.Reason
	}
}

// read is the queue's whole reading. A store that cannot be read is named in
// Unreadable and contributes nothing; the other stores still show.
func (r *dashQueueReader) read(now time.Time) *dashboard.Queue {
	stores, names, err := r.stores()
	if err != nil {
		return nil
	}
	q := &dashboard.Queue{At: now}
	if r.prefixes != nil {
		q.Prefixes = r.prefixes(names)
	}
	// One ledger read per queue read: every ready row's overlay comes from the
	// same tick's verdicts, and a stale ledger is read here once rather than
	// per row.
	v := r.readVerdicts(now)
	q.VerdictsAt, q.VerdictsStale = v.at, v.stale
	var ready, landing, blocked []dashboard.QueueBead
	// The Rigs panel lists one row per known rig; hq is the town store, not a
	// rig, so it gets none. A store this read could not reach keeps its row and
	// leaves its counts unset, which the page shows as unavailable.
	var rigs []dashboard.Rig
	readable := map[string]bool{}
	addRig := func(name string, parked, read bool) {
		if name == "hq" {
			return
		}
		rigs = append(rigs, dashboard.Rig{Name: name, Parked: parked})
		readable[name] = read
	}
	// The work beads a seat holds say which molecules are live. One read per
	// store, not one per step, and pooled across the town so a molecule counts
	// as live wherever its work bead sits.
	live := map[string]bool{}
	heldOK := map[string]bool{}
	for _, name := range names {
		if st := stores[name]; st != nil {
			held, err := st.ListIssueStatuses(beads.IssueStatusHooked, beads.StatusInProgress)
			if err == nil {
				addLiveMolecules(live, held)
			}
			heldOK[name] = err == nil
		}
	}
	for _, name := range names {
		st := stores[name]
		parked := r.parked != nil && r.parked(name)
		if st == nil {
			addRig(name, parked, false)
			continue
		}
		rd, err1 := st.ReadyAll()
		bl, err2 := st.Blocked()
		ln, err3 := st.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
		if err1 != nil || err2 != nil || err3 != nil || !heldOK[name] {
			q.Unreadable = append(q.Unreadable, name)
			addRig(name, parked, false)
			continue
		}
		addRig(name, parked, true)
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
			} else if row.Assignee != "" && (row.Shape == "ok" || row.Shape == "fix") {
				// the dispatcher takes only unassigned beads: an assignee holds this
				// one whether or not it is shaped
				row.Shape, row.ShapeNote = "held", "assigned to "+row.Assignee+": the dispatcher takes only unassigned beads"
			}
			// The dispatcher's own last verdict outranks a shape derived from the
			// bead's text; an old verdict outranks nothing (gt-ckncx).
			if !v.stale {
				dashOverlayVerdict(&row, v.byBead)
			}
			ready = append(ready, row)
		}
		for _, i := range bl {
			if dashboard.IsLiveStep(i.ID, i.Type, i.Parent, live) {
				continue
			}
			row := dashQueueRow(name, i)
			// Split here, over the whole list, rather than off the capped rows
			// the page is given: the totals must add up (gt-vcr3z).
			row.Wisp = dashboard.IsLeakedStepWisp(i.ID, i.Type, i.Parent, live)
			if row.Wisp {
				q.BlockedWisps++
			} else {
				q.BlockedWork++
			}
			blocked = append(blocked, row)
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
	// The per-store totals the capping above just computed are exact, so the
	// rows take their counts from them rather than counting again.
	for i := range rigs {
		if !readable[rigs[i].Name] {
			continue
		}
		n, m := q.ReadyRigs[rigs[i].Name], q.LandingRigs[rigs[i].Name]
		rigs[i].Ready, rigs[i].Landing = &n, &m
	}
	q.Rigs = rigs
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
