// Package trackerclient implements beads.Client over the town tracker service
// (tracker/client). It is the thin adapter ADR 0006 puts behind the seam of
// ADR 0001: code written against beads.Client does not know which store it is
// talking to, and beadsfake.RunClientContract holds this adapter to the same
// behavior as bd.
package trackerclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// defaultPriority is what bd gives an issue created with no priority.
const defaultPriority = 2

// Client is a beads.Client over the tracker. The tracker attributes every
// write to the actor of the client's token.
type Client struct {
	tc *client.Client
	// rig is the rig a create with no Rig, ID or Parent goes to.
	rig string
}

var _ beads.Client = (*Client)(nil)

// The tracker holds every rig in one database, so it answers a cross-rig list
// too (beads.ListAcross, internal/metrics' landing column).
var _ beads.AcrossStore = (*Client)(nil)

// New returns a Client over tc. defaultRig is the rig new issues go to when
// CreateOptions names neither a rig, an ID nor a parent.
func New(tc *client.Client, defaultRig string) *Client { return &Client{tc: tc, rig: defaultRig} }

func ctx() context.Context { return context.Background() }

// Show returns one issue, or an error wrapping beads.ErrNotFound.
func (c *Client) Show(id string) (*beads.Issue, error) {
	a, err := c.tc.Show(ctx(), id)
	if err != nil {
		return nil, mapErr(err)
	}
	return issue(a), nil
}

// ShowMultiple returns the issues that exist among ids, keyed by ID.
func (c *Client) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if len(ids) == 0 {
		return map[string]*beads.Issue{}, nil
	}
	m, err := c.tc.ShowMany(ctx(), ids)
	if err != nil {
		return nil, mapErr(err)
	}
	return issueMap(m), nil
}

// Comments returns the comments on an issue, oldest first.
func (c *Client) Comments(id string) ([]beads.Comment, error) {
	cs, err := c.tc.Comments(ctx(), id)
	if err != nil {
		return nil, mapErr(err)
	}
	return comments(cs), nil
}

// ErrActorOption is Create refusing CreateOptions.Actor. The tracker records
// the creator as the actor of the client's token and does not take one named in
// a request (a request must not say who it is), so an option that names one
// is refused rather than ignored. Leave Actor empty: the creator is the
// token's actor, which beadsfake.RunActorContract pins.
var ErrActorOption = errors.New("trackerclient: CreateOptions.Actor is not supported: the tracker records the creator as the token's actor")

// ErrListFilter is List refusing a ListOptions filter the tracker's list query
// cannot express. Dropping one would answer the caller's narrower question
// with a wider set, so, as with ErrActorOption, the filter is refused rather
// than ignored, and no request is sent.
var ErrListFilter = errors.New("trackerclient: the tracker's list query cannot express this filter")

// ErrUnsupported is an admin verb the tracker does not serve: it holds issues,
// not a bd build or a Dolt migration level, so Version and SchemaLevel have no
// answer to give. The refusing method names itself and sends no request, so a
// caller cannot read a refusal as an answer.
var ErrUnsupported = errors.New("trackerclient: the tracker does not serve this verb")

// Create creates an issue and returns it. Every CreateOptions field is carried
// except Actor, which is refused with ErrActorOption.
func (c *Client) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if opts.Actor != "" {
		return nil, ErrActorOption
	}
	if strings.HasPrefix(opts.Title, "-") {
		return nil, fmt.Errorf("refusing to create a bead: %w (got %q)", beads.ErrFlagTitle, opts.Title)
	}
	labels := append([]string(nil), opts.Labels...)
	if opts.Label != "" {
		labels = append(labels, opts.Label)
	}
	if opts.Type != "" {
		labels = append(labels, "gt:"+opts.Type) // deprecated type: kept as the label bd made of it
	}
	p := api.CreateParams{
		ID: opts.ID, Rig: opts.Rig, Title: opts.Title, Labels: labels, Priority: opts.Priority,
		Description: opts.Description, Acceptance: opts.Acceptance, Parent: opts.Parent, Assignee: opts.Assignee,
		Ephemeral: opts.Ephemeral, EventKind: opts.EventKind, EventPayload: opts.EventPayload,
	}
	if p.Priority < 0 {
		p.Priority = defaultPriority
	}
	if p.Rig == "" && p.ID == "" && p.Parent == "" {
		p.Rig = c.rig
	}
	if opts.EventKind != "" {
		p.Type = "event"
	}
	a, err := c.tc.Create(ctx(), p)
	if err != nil {
		return nil, mapErr(err)
	}
	return issue(a), nil
}

// Update changes the fields opts sets.
func (c *Client) Update(id string, opts beads.UpdateOptions) error {
	return mapErr(c.tc.Update(ctx(), id, patch(opts), api.Guard{}))
}

func patch(o beads.UpdateOptions) api.UpdatePatch {
	return api.UpdatePatch{
		Title: o.Title, Status: o.Status, Priority: o.Priority, Description: o.Description, Assignee: o.Assignee,
		Acceptance: o.Acceptance, AddLabels: o.AddLabels, RemoveLabels: o.RemoveLabels, SetLabels: o.SetLabels,
		AppendNotes: o.AppendNotes, Persistent: o.Persistent, Force: o.Force,
	}
}

// Close closes issues.
func (c *Client) Close(ids ...string) error { return c.closeIDs("", false, ids) }

// CloseWithReason closes issues and records reason.
func (c *Client) CloseWithReason(reason string, ids ...string) error {
	return c.closeIDs(reason, false, ids)
}

// ForceCloseWithReason is CloseWithReason past the close fences.
func (c *Client) ForceCloseWithReason(reason string, ids ...string) error {
	return c.closeIDs(reason, true, ids)
}

// defaultCloseReason is what bd records when a close names none.
const defaultCloseReason = "Closed"

// closeIDs closes ids one at a time, in argument order, as bd does: a refused
// issue (open children, an open blocker, a pin, another actor's claim) is
// skipped and the rest are still closed, and an issue whose blocker comes later
// in the same batch is refused at its turn. The result:
//   - an id that does not exist: beads.ErrNotFound, after the others are closed;
//   - some closed, some refused: a *beads.PartialCloseError;
//   - every issue refused: the first refusal, as bd fails it.
func (c *Client) closeIDs(reason string, force bool, ids []string) error {
	if reason == "" {
		reason = defaultCloseReason
	}
	var closed, notClosed, missing []string
	var firstRefusal error
	for _, id := range ids {
		err := c.tc.Close(ctx(), []string{id}, reason, force)
		switch {
		case err == nil:
			closed = append(closed, id)
		case errors.Is(err, api.ErrNotFound):
			missing = append(missing, id)
		case errors.Is(err, api.ErrConflict): // the close fences: children, blockers, a pin, another actor's claim
			notClosed = append(notClosed, id)
			if firstRefusal == nil {
				firstRefusal = err
			}
		default:
			return mapErr(err) // unavailable, invalid: not a verdict on this issue
		}
	}
	switch {
	case len(missing) > 0:
		return mapErr(api.NotFound(strings.Join(missing, ", ")))
	case len(notClosed) == 0:
		return nil
	case len(closed) == 0:
		return firstRefusal
	}
	return &beads.PartialCloseError{Closed: closed, NotClosed: notClosed}
}

// DeleteIssues permanently deletes ids, all or none.
func (c *Client) DeleteIssues(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return mapErr(c.tc.Delete(ctx(), ids))
}

// Release returns a claimed issue to open and clears its assignee.
func (c *Client) Release(id string) error {
	open, none := "open", ""
	return mapErr(c.tc.Update(ctx(), id, api.UpdatePatch{Status: &open, Assignee: &none}, api.Guard{}))
}

// AddComment appends a comment as the client's actor.
func (c *Client) AddComment(id, comment string) error { return c.AddCommentAs(id, "", comment) }

// AddCommentAs appends a comment recorded as written by author.
func (c *Client) AddCommentAs(id, author, comment string) error {
	return mapErr(c.tc.AddComment(ctx(), id, author, comment))
}

// AddDependency makes issue depend on (be blocked by) dependsOn.
func (c *Client) AddDependency(issue, dependsOn string) error {
	return c.AddTypedDependency(issue, dependsOn, "blocks")
}

// AddTypedDependency records that issue depends on dependsOn with relation depType.
func (c *Client) AddTypedDependency(issue, dependsOn, depType string) error {
	return mapErr(c.tc.AddDependency(ctx(), issue, dependsOn, depType))
}

// DepList returns the issues id depends on with relation depType ("" for any).
func (c *Client) DepList(id, depType string) ([]beads.IssueDep, error) {
	d, err := c.tc.Dependencies(ctx(), id, depType)
	if err != nil {
		return nil, mapErr(err)
	}
	return deps(d), nil
}

// DepListUp returns the issues that depend on id with relation depType ("" for
// any). The store fills Dependents on every issue it reads, wisps included, so
// one Show answers the read; a missing issue is the error Show returns.
func (c *Client) DepListUp(id, depType string) ([]beads.IssueDep, error) {
	a, err := c.tc.Show(ctx(), id)
	if err != nil {
		return nil, mapErr(err)
	}
	out := []beads.IssueDep{}
	for _, d := range deps(a.Dependents) {
		if depType == "" || d.DependencyType == depType {
			out = append(out, d)
		}
	}
	return out, nil
}

// RemoveDependency removes that dependency, whatever its type.
func (c *Client) RemoveDependency(issue, dependsOn string) error {
	return mapErr(c.tc.RemoveDependency(ctx(), issue, dependsOn))
}

// AppendNotes appends note to the issue's notes.
func (c *Client) AppendNotes(id, note string) error {
	return mapErr(c.tc.Update(ctx(), id, api.UpdatePatch{AppendNotes: note}, api.Guard{}))
}

// GateCheck reports nothing: the tracker keeps no gates to evaluate, so no
// gate of any type can be resolved. Read task Mo5 of
// docs/plans/2026-10-25-town-tracker-plan.md before adding gate polling here.
func (c *Client) GateCheck(types ...string) (string, error) { return "", nil }

// ShowText returns one issue as a short terminal line: its id, title, priority
// and status. The tracker serves issues, not bd's prose, so the line is this
// client's own render. A flag in args is an error, not a silent drop, and no
// request is sent for it. A missing issue is the error Show returns.
func (c *Client) ShowText(id string, args []string) (string, error) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return "", fmt.Errorf("trackerclient: ShowText takes no bd flags, got %q", a)
		}
	}
	is, err := c.Show(id)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("◇ %s · %s   [P%d · %s]\n", is.ID, is.Title, is.Priority, strings.ToUpper(is.Status)), nil
}

// ReleaseIfAssignee returns the issue to open with no assignee, but only while
// expected still holds it. released=false with a nil error means the guard no
// longer held and nothing was written.
func (c *Client) ReleaseIfAssignee(id, expected string) (bool, error) {
	return c.guarded(id, expected, "open", "")
}

// TransferIfAssignee sets the issue's status and assignee, but only while
// expected still holds it.
func (c *Client) TransferIfAssignee(id, expected, status, assignee string) (bool, error) {
	return c.guarded(id, expected, status, assignee)
}

func (c *Client) guarded(id, expected, status, assignee string) (bool, error) {
	err := c.tc.Update(ctx(), id, api.UpdatePatch{Status: &status, Assignee: &assignee}, api.Guard{IfAssignee: &expected})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, api.ErrGuardFailed):
		return false, nil
	}
	return false, mapErr(err)
}
