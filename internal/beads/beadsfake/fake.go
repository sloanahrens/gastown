// Package beadsfake is an in-memory beads database for unit tests. Its
// behavior is pinned to real bd by RunClientContract, which runs against the
// fake in the unit tier and against bd on a Dolt test database in the
// integration tier.
//
// A Fake is one database: there is no prefix routing between databases, no
// town, and every issue is issue_type "task". What it models is listed on
// each method; anything a test needs beyond that belongs in the contract
// first.
package beadsfake

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

var _ beads.Client = (*Fake)(nil)

// Dependency relations the fake records. They are bd's dependency_type
// values.
const (
	depBlocks      = "blocks"
	depParentChild = "parent-child"
)

// defaultCloseReason is what bd records when an issue is closed without one.
const defaultCloseReason = "Closed"

type edge struct {
	to  string
	typ string
}

type record struct {
	issue    beads.Issue
	seq      int    // creation order
	deps     []edge // what this issue depends on
	comments []beads.Comment
	children int // children minted from this ID
}

// Fake is an in-memory beads database. The zero value is not usable; call New.
type Fake struct {
	mu     sync.Mutex
	prefix string
	actor  string
	clock  clockwork.Clock
	// own is the default clock when no WithClock was given. It moves one
	// second at every write, as timestamps on a real database move on.
	own *clockwork.FakeClock
	seq    int
	issues map[string]*record

	// The maintenance surface (admin.go).
	config   map[string]string
	dropped  map[string]bool
	sql      func(query string) ([][]string, error)
	sqlLog   []string
	inits    []beads.InitOptions
	failures map[string]error
}

// Option configures a Fake.
type Option func(*Fake)

// WithPrefix sets the issue ID prefix (default "gt").
func WithPrefix(p string) Option { return func(f *Fake) { f.prefix = p } }

// WithActor sets the name recorded as created_by and comment author
// (default "tester").
func WithActor(a string) Option { return func(f *Fake) { f.actor = a } }

// WithClock sets the clock timestamps come from. The default is a fake clock
// at Epoch that the Fake advances one second at every write; a clock given
// here is only read.
func WithClock(c clockwork.Clock) Option { return func(f *Fake) { f.clock, f.own = c, nil } }

// Epoch is the default fake clock's start.
var Epoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// New returns an empty database.
func New(opts ...Option) *Fake {
	own := clockwork.NewFakeClockAt(Epoch)
	f := &Fake{
		prefix: "gt",
		actor:  "tester",
		clock:  own,
		own:    own,
		issues: map[string]*record{},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func (f *Fake) now() string { return f.clock.Now().UTC().Format(time.RFC3339) }

// tick moves the Fake's own clock one second, at the start of every write.
// Callers hold f.mu.
func (f *Fake) tick() {
	if f.own != nil {
		f.own.Advance(time.Second)
	}
}

func notFound(id string) error { return fmt.Errorf("%s: %w", id, beads.ErrNotFound) }

// snapshot returns a copy of r's issue with the fields show derives
// (labels, parent, dependencies) filled from the store. Callers hold f.mu.
func (f *Fake) snapshot(r *record) *beads.Issue {
	is := r.issue
	is.Labels = append([]string(nil), r.issue.Labels...)
	is.Dependencies = nil
	for _, e := range r.deps {
		dep := beads.IssueDep{ID: e.to, DependencyType: e.typ}
		if t, ok := f.issues[e.to]; ok {
			dep.Title, dep.Status, dep.Priority, dep.Type = t.issue.Title, t.issue.Status, t.issue.Priority, t.issue.Type
			dep.CloseReason = t.issue.CloseReason
		}
		is.Dependencies = append(is.Dependencies, dep)
		if e.typ == depParentChild {
			is.Parent = e.to
		}
	}
	is.DependencyCount = len(r.deps)
	is.Comments = nil
	is.Metadata = append([]byte(nil), r.issue.Metadata...)
	return &is
}

// ordered returns every record in bd list's order: priority first (0 is
// highest), then newest first. Callers hold f.mu.
func (f *Fake) ordered() []*record {
	out := make([]*record, 0, len(f.issues))
	for _, r := range f.issues {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].issue.Priority != out[j].issue.Priority {
			return out[i].issue.Priority < out[j].issue.Priority
		}
		return out[i].seq > out[j].seq
	})
	return out
}

// Show returns the issue, or an error wrapping beads.ErrNotFound. Its
// Dependencies list what it depends on, with dependency_type "blocks" or
// "parent-child", and Parent names its parent.
func (f *Fake) Show(id string) (*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return nil, notFound(id)
	}
	return f.snapshot(r), nil
}

// ShowMultiple returns the issues that exist among ids; missing IDs are left
// out without an error, as bd does.
func (f *Fake) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		if r, ok := f.issues[id]; ok {
			out[id] = f.snapshot(r)
		}
	}
	return out, nil
}

func statusMatches(status, filter string) bool {
	switch filter {
	case "":
		return status != string(beads.StatusClosed)
	case "all":
		return true
	default:
		return status == filter
	}
}

func hasLabel(is *beads.Issue, label string) bool {
	for _, l := range is.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// List returns the issues matching opts, newest first. Ephemeral selects the
// wisps instead of the issues. Status "" leaves out closed issues and "all"
// keeps them; Type is read as the label "gt:<Type>"; Priority -1 means any.
// Rig is ignored: a Fake is one database.
func (f *Fake) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	label := opts.Label
	if label == "" && opts.Type != "" {
		label = "gt:" + opts.Type
	}
	var out []*beads.Issue
	for _, r := range f.ordered() {
		is := f.snapshot(r)
		switch {
		case is.Ephemeral != opts.Ephemeral,
			!statusMatches(is.Status, opts.Status),
			label != "" && !hasLabel(is, label),
			opts.Priority >= 0 && is.Priority != opts.Priority,
			opts.Parent != "" && is.Parent != opts.Parent,
			opts.Assignee != "" && is.Assignee != opts.Assignee,
			opts.NoAssignee && is.Assignee != "":
			continue
		}
		out = append(out, is)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

// ListByAssignee returns assignee's issues in every status.
func (f *Fake) ListByAssignee(assignee string) ([]*beads.Issue, error) {
	return f.List(beads.ListOptions{Status: "all", Assignee: assignee, Priority: -1})
}

// GetAssignedIssue returns assignee's open issue, else in_progress, else
// hooked; nil, nil when there is none.
func (f *Fake) GetAssignedIssue(assignee string) (*beads.Issue, error) {
	issues, err := f.ListByAssignee(assignee)
	if err != nil {
		return nil, err
	}
	for _, status := range []string{"open", "in_progress", beads.StatusHooked} {
		for _, is := range issues {
			if is.Status == status {
				return is, nil
			}
		}
	}
	return nil, nil
}

func inStatuses(status string, statuses []beads.IssueStatus) bool {
	for _, s := range statuses {
		if s != "" && string(s) == status {
			return true
		}
	}
	return false
}

// ListIssueStatuses returns the issues (not wisps) in any of statuses.
func (f *Fake) ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beads.Issue
	for _, r := range f.ordered() {
		if !r.issue.Ephemeral && inStatuses(r.issue.Status, statuses) {
			out = append(out, f.snapshot(r))
		}
	}
	return out, nil
}

// ListAssignedIssueStatuses returns assignee's issues and wisps in any of
// statuses.
func (f *Fake) ListAssignedIssueStatuses(assignee string, statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	if assignee == "" {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beads.Issue
	for _, r := range f.ordered() {
		if r.issue.Assignee == assignee && inStatuses(r.issue.Status, statuses) {
			out = append(out, f.snapshot(r))
		}
	}
	return out, nil
}

func dispatchable(is *beads.Issue) bool {
	for _, t := range constants.NonDispatchableBeadTypes {
		if is.Type == t {
			return false
		}
	}
	for _, l := range constants.NonDispatchableBeadLabels {
		if hasLabel(is, l) {
			return false
		}
	}
	return true
}

// blocked reports whether r depends, through "blocks", on an issue that is
// not closed. Callers hold f.mu.
func (f *Fake) blocked(r *record) bool { return len(f.openBlockers(r)) > 0 }

// Ready returns the open issues that nothing open blocks, leaving out the
// town's bookkeeping labels and types (constants.NonDispatchableBead*).
// Parent-child links do not block. Only direct "blocks" dependencies are
// modeled.
func (f *Fake) Ready() ([]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beads.Issue
	for _, r := range f.ordered() {
		is := f.snapshot(r)
		if is.Ephemeral || is.Status != string(beads.StatusOpen) || f.blocked(r) || !dispatchable(is) {
			continue
		}
		out = append(out, is)
	}
	return out, nil
}

// Children returns the issues and wisps whose parent is parentID.
func (f *Fake) Children(parentID string) ([]*beads.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beads.Issue
	for _, r := range f.ordered() {
		for _, e := range r.deps {
			if e.typ == depParentChild && e.to == parentID {
				out = append(out, f.snapshot(r))
				break
			}
		}
	}
	return out, nil
}

// Comments returns an issue's comments, oldest first, or an error for a
// missing issue.
func (f *Fake) Comments(id string) ([]beads.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return nil, notFound(id)
	}
	return append([]beads.Comment(nil), r.comments...), nil
}

// Create creates an issue. IDs are "<prefix>-f<n>", a child's is
// "<parent>.<n>" and a wisp's "<prefix>-wisp-<n>". Priority -1 means bd's
// default, 2. A flag-like title is refused with beads.ErrFlagTitle, as
// *beads.Beads refuses it.
func (f *Fake) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if beads.IsFlagLikeTitle(opts.Title) {
		return nil, fmt.Errorf("refusing to create bead: %w (got %q)", beads.ErrFlagTitle, opts.Title)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick()
	f.seq++
	id := fmt.Sprintf("%s-f%d", f.prefix, f.seq)
	var deps []edge
	if opts.Parent != "" {
		p, ok := f.issues[opts.Parent]
		if !ok {
			return nil, notFound(opts.Parent)
		}
		p.children++
		id = fmt.Sprintf("%s.%d", opts.Parent, p.children)
		deps = append(deps, edge{to: opts.Parent, typ: depParentChild})
	}
	if opts.Ephemeral && opts.Parent == "" {
		id = fmt.Sprintf("%s-wisp-%d", f.prefix, f.seq)
	}
	labels := opts.Labels
	switch {
	case len(labels) > 0:
	case opts.Label != "":
		labels = []string{opts.Label}
	case opts.Type != "":
		labels = []string{"gt:" + opts.Type}
	}
	priority := opts.Priority
	if priority < 0 {
		priority = 2
	}
	actor := opts.Actor
	if actor == "" {
		actor = f.actor
	}
	now := f.now()
	r := &record{
		seq:  f.seq,
		deps: deps,
		issue: beads.Issue{
			ID:          id,
			Title:       opts.Title,
			Description: opts.Description,
			Status:      string(beads.StatusOpen),
			Priority:    priority,
			Type:        "task",
			CreatedAt:   now,
			CreatedBy:   actor,
			UpdatedAt:   now,
			Labels:      sortedSet(labels),
			Ephemeral:   opts.Ephemeral,
		},
	}
	f.issues[id] = r
	return f.snapshot(r), nil
}

func sortedSet(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// openChildren counts id's children that are not closed. Callers hold f.mu.
func (f *Fake) openChildren(id string) int {
	n := 0
	for _, r := range f.issues {
		for _, e := range r.deps {
			if e.typ == depParentChild && e.to == id && r.issue.Status != string(beads.StatusClosed) {
				n++
			}
		}
	}
	return n
}

// errClaimHeld is bd's refusal to reassign another actor's live claim.
func errClaimHeld(id, holder string) error {
	return fmt.Errorf("cannot reassign %s: held by %q (in_progress); pass --force only if their claim is abandoned", id, holder)
}

// errNotActors is bd's refusal to close an issue assigned to someone else.
func errNotActors(id, assignee, actor string) error {
	return fmt.Errorf("cannot close %s: assignee is %q, actor is %q; reclaim or use --force to override", id, assignee, actor)
}

// errBlocked is bd's refusal to close an issue an open issue blocks.
func errBlocked(id string, by []string) error {
	return fmt.Errorf("cannot close blocked issue: %s is blocked by %v (use --force to override)", id, by)
}

// openBlockers lists the issues r depends on ("blocks") that are not
// closed. Callers hold f.mu.
func (f *Fake) openBlockers(r *record) []string {
	var out []string
	for _, e := range r.deps {
		if t, ok := f.issues[e.to]; ok && e.typ == depBlocks && t.issue.Status != string(beads.StatusClosed) {
			out = append(out, e.to)
		}
	}
	return out
}

// updateCloseRefusal is why bd would refuse to move r to closed through an
// update without --force: open children or an open blocker. Callers hold
// f.mu.
func (f *Fake) updateCloseRefusal(r *record) error {
	id := r.issue.ID
	if n := f.openChildren(id); n > 0 {
		return errOpenChildren(id, n)
	}
	if by := f.openBlockers(r); len(by) > 0 {
		return errBlocked(id, by)
	}
	return nil
}

// closeRefusal is why bd would refuse to close r without --force: an update's
// refusals, or an assignee other than the actor. Callers hold f.mu.
func (f *Fake) closeRefusal(r *record) error {
	id := r.issue.ID
	if err := f.updateCloseRefusal(r); err != nil {
		return err
	}
	if a := r.issue.Assignee; a != "" && a != f.actor {
		return errNotActors(id, a, f.actor)
	}
	return nil
}

// errOpenChildren is bd's refusal to close a parent with open children.
func errOpenChildren(id string, n int) error {
	return fmt.Errorf("cannot close %s: %d open child issue(s); close children first or use --force to override", id, n)
}

// setStatus moves r to status, stamping or clearing the close fields as bd
// does. reason is recorded as given: bd close defaults it to "Closed", an
// update to status closed leaves it empty. Callers hold f.mu.
func (f *Fake) setStatus(r *record, status, reason string) {
	r.issue.Status = status
	if status == string(beads.StatusClosed) {
		r.issue.ClosedAt = f.now()
		r.issue.CloseReason = reason
		return
	}
	r.issue.ClosedAt = ""
	r.issue.CloseReason = ""
}

// Update applies opts. Like bd it refuses, unless opts.Force, to reassign an
// in_progress issue another assignee holds, and to close an issue that has
// open children or an open blocker. (Close's assignee refusal does not apply
// to an update.) SetLabels replaces the labels; otherwise AddLabels and
// RemoveLabels apply.
func (f *Fake) Update(id string, opts beads.UpdateOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return notFound(id)
	}
	f.tick()
	if !opts.Force {
		if opts.Assignee != nil && r.issue.Status == string(beads.StatusInProgress) &&
			r.issue.Assignee != "" && r.issue.Assignee != *opts.Assignee {
			return errClaimHeld(id, r.issue.Assignee)
		}
		if opts.Status != nil && *opts.Status == string(beads.StatusClosed) {
			if err := f.updateCloseRefusal(r); err != nil {
				return err
			}
		}
	}
	is := &r.issue
	if opts.Title != nil {
		is.Title = *opts.Title
	}
	if opts.Status != nil {
		f.setStatus(r, *opts.Status, "")
	}
	if opts.Priority != nil {
		is.Priority = *opts.Priority
	}
	if opts.Description != nil {
		is.Description = *opts.Description
	}
	if opts.Assignee != nil {
		is.Assignee = *opts.Assignee
	}
	switch {
	case len(opts.SetLabels) > 0:
		is.Labels = sortedSet(opts.SetLabels)
	default:
		labels := append([]string(nil), is.Labels...)
		labels = append(labels, opts.AddLabels...)
		var kept []string
		for _, l := range labels {
			drop := false
			for _, rm := range opts.RemoveLabels {
				if l == rm {
					drop = true
				}
			}
			if !drop {
				kept = append(kept, l)
			}
		}
		is.Labels = sortedSet(kept)
	}
	is.UpdatedAt = f.now()
	return nil
}

func (f *Fake) close(reason string, force bool, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick()
	if reason == "" {
		reason = defaultCloseReason
	}
	// A missing ID fails the whole batch before anything closes, as bd does.
	for _, id := range ids {
		if _, ok := f.issues[id]; !ok {
			return notFound(id)
		}
	}
	// A refused issue is skipped and the rest close. The batch fails with
	// the first refusal when every issue in it is refused (bd 1.2), and
	// with a *beads.PartialCloseError when only some are, as *beads.Beads
	// reports what bd left open.
	var firstRefusal error
	var closed, refused []string
	for _, id := range ids {
		r := f.issues[id]
		if !force {
			if err := f.closeRefusal(r); err != nil {
				if firstRefusal == nil {
					firstRefusal = err
				}
				refused = append(refused, id)
				continue
			}
		}
		f.setStatus(r, string(beads.StatusClosed), reason)
		r.issue.UpdatedAt = f.now()
		closed = append(closed, id)
	}
	switch {
	case len(refused) == 0:
		return nil
	case len(closed) == 0:
		return firstRefusal
	}
	return &beads.PartialCloseError{Closed: closed, NotClosed: refused}
}

// Close closes ids with bd's default reason, "Closed". An issue with open
// children or an open blocker, or assigned to someone other than the actor,
// is refused: skipped, the rest closing, and reported in a
// *beads.PartialCloseError (the first refusal when every issue is refused).
func (f *Fake) Close(ids ...string) error { return f.close("", false, ids) }

// CloseWithReason closes ids recording reason, with Close's refusals.
func (f *Fake) CloseWithReason(reason string, ids ...string) error {
	return f.close(reason, false, ids)
}

// ForceCloseWithReason closes ids recording reason, past Close's refusals.
func (f *Fake) ForceCloseWithReason(reason string, ids ...string) error {
	return f.close(reason, true, ids)
}

// Release returns an issue to open and clears its assignee, whoever holds it.
func (f *Fake) Release(id string) error { return f.ReleaseWithReason(id, "") }

// ReleaseWithReason is Release, recording "Released: <reason>" in the notes.
func (f *Fake) ReleaseWithReason(id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return notFound(id)
	}
	f.tick()
	f.setStatus(r, string(beads.StatusOpen), "")
	r.issue.Assignee = ""
	if reason != "" {
		r.issue.Notes = "Released: " + reason
	}
	r.issue.UpdatedAt = f.now()
	return nil
}

// AppendNotes appends note to the issue's notes, after a newline when
// there are notes already, as bd update --append-notes does.
func (f *Fake) AppendNotes(id, note string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return notFound(id)
	}
	f.tick()
	if r.issue.Notes != "" {
		r.issue.Notes += "\n"
	}
	r.issue.Notes += note
	r.issue.UpdatedAt = f.now()
	return nil
}

// ReleaseIfAssignee is TransferIfAssignee to open with no assignee.
func (f *Fake) ReleaseIfAssignee(id, expected string) (bool, error) {
	return f.TransferIfAssignee(id, expected, string(beads.StatusOpen), "")
}

// TransferIfAssignee sets status and assignee only while expected is the
// assignee (an empty expected means unassigned), and reports false with no
// error, writing nothing, when it is not. The guard stands in for the claim
// fence: an in_progress claim moves when its holder is the one expected.
func (f *Fake) TransferIfAssignee(id, expected, status, assignee string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return false, notFound(id)
	}
	if r.issue.Assignee != expected {
		return false, nil
	}
	f.tick()
	f.setStatus(r, status, "")
	r.issue.Assignee = assignee
	r.issue.UpdatedAt = f.now()
	return true, nil
}

// AddComment appends a comment authored by the fake's actor.
func (f *Fake) AddComment(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[id]
	if !ok {
		return notFound(id)
	}
	f.tick()
	r.comments = append(r.comments, beads.Comment{
		ID:        fmt.Sprintf("c%d", len(r.comments)+1),
		IssueID:   id,
		Author:    f.actor,
		Text:      text,
		CreatedAt: f.now(),
	})
	return nil
}

// AddDependency makes issue depend on dependsOn ("blocks"). Both must exist;
// adding an existing dependency again is a no-op.
func (f *Fake) AddDependency(issue, dependsOn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[issue]
	if !ok {
		return notFound(issue)
	}
	if _, ok := f.issues[dependsOn]; !ok {
		return notFound(dependsOn)
	}
	for _, e := range r.deps {
		if e.to == dependsOn {
			return nil
		}
	}
	r.deps = append(r.deps, edge{to: dependsOn, typ: depBlocks})
	return nil
}

// RemoveDependency removes the dependency of issue on dependsOn.
func (f *Fake) RemoveDependency(issue, dependsOn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.issues[issue]
	if !ok {
		return notFound(issue)
	}
	kept := r.deps[:0]
	for _, e := range r.deps {
		if e.to != dependsOn {
			kept = append(kept, e)
		}
	}
	r.deps = kept
	return nil
}

// Seed stores issues exactly as given, IDs included, replacing any issue
// with the same ID. It sets up a test's starting state (an issue with a
// fixed ID, such as a rig identity bead) and is not a bd operation: labels,
// status and timestamps are taken as they are, and no dependencies are
// recorded.
func (f *Fake) Seed(issues ...beads.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range issues {
		f.seq++
		is.Labels = sortedSet(is.Labels)
		if is.Status == "" {
			is.Status = string(beads.StatusOpen)
		}
		f.issues[is.ID] = &record{seq: f.seq, issue: is}
	}
}
