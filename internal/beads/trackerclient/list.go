package trackerclient

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/tracker/api"
)

// listFilter maps beads.ListOptions to the tracker's filter. The tracker shares
// bd's status rule: no status means every issue that is not closed, "all"
// means every issue, and a comma list names statuses. ListOptions.Rig is not
// mapped: in bd it narrows merge requests by the rig named in their
// description (beads.ListMergeRequests does that), not by the database.
func listFilter(o beads.ListOptions) api.ListFilter {
	f := api.ListFilter{
		Labels: o.Labels, Parent: o.Parent, Assignee: o.Assignee, NoAssignee: o.NoAssignee,
		IssueType: o.IssueType, IDs: o.IDs, Ephemeral: o.Ephemeral, IncludeInfra: o.IncludeInfra,
		ClosedAfter: o.ClosedAfter, CreatedAfter: o.CreatedAfter, Limit: o.Limit,
	}
	label := o.Label
	if label == "" && o.Type != "" {
		label = "gt:" + o.Type // the deprecated type filter is a label
	}
	if label != "" {
		f.Labels = append([]string{label}, f.Labels...)
	}
	if p := int(o.Priority) - 1; o.Priority > beads.PriorityAny {
		f.Priority = &p
	}
	switch o.Status {
	case "":
	case "all":
		f.Statuses = []string{"all"}
	default:
		f.Statuses = strings.Split(o.Status, ",")
	}
	return f
}

// refusedListFilter names the ListOptions filter the tracker's list query
// cannot express, or nil. Dropping one would answer the caller's narrower
// question with a wider set, so List refuses it instead.
func refusedListFilter(o beads.ListOptions) error {
	switch {
	case o.NotesContains != "":
		return fmt.Errorf("List{NotesContains}: %w", ErrListFilter)
	case !o.UpdatedAfter.IsZero():
		return fmt.Errorf("List{UpdatedAfter}: %w", ErrListFilter)
	}
	return nil
}

// List returns the issues matching opts.
func (c *Client) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if err := refusedListFilter(opts); err != nil {
		return nil, err
	}
	f := listFilter(opts)
	if f.Limit > 0 {
		page, err := c.tc.List(ctx(), f)
		if err != nil {
			return nil, mapErr(err)
		}
		return issues(page.Issues), nil
	}
	all, err := c.tc.ListAll(ctx(), f)
	if err != nil {
		return nil, mapErr(err)
	}
	return issues(all), nil
}

// ListAcross returns the issues matching opts for each rig of rigs, from one
// request: the tracker holds every rig in one database, so a rig scope list
// answers them together. Every rig asked for is in the answer, empty when it
// matched nothing; a rig the tracker does not hold, and an opts filter the list
// query cannot express, refuse the call. Each issue is filed under the rig its
// id starts with, which is the rig the store holds it under: newID mints
// "<rig>-", and a child keeps its parent's prefix.
func (c *Client) ListAcross(rigs []string, opts beads.ListOptions) (beads.AcrossResult, error) {
	res := beads.AcrossResult{Rigs: map[string][]*beads.Issue{}, Errors: map[string]error{}}
	if err := refusedListFilter(opts); err != nil {
		return res, err
	}
	if len(rigs) == 0 {
		return res, nil
	}
	f := listFilter(opts)
	f.Rigs = rigs
	var page []api.Issue
	if f.Limit > 0 {
		p, err := c.tc.List(ctx(), f)
		if err != nil {
			return res, mapErr(err)
		}
		page = p.Issues
	} else {
		all, err := c.tc.ListAll(ctx(), f)
		if err != nil {
			return res, mapErr(err)
		}
		page = all
	}
	for _, rig := range rigs {
		res.Rigs[rig] = []*beads.Issue{}
	}
	for _, a := range page {
		rig, _, hasPrefix := strings.Cut(a.ID, "-")
		if _, asked := res.Rigs[rig]; !hasPrefix || !asked {
			return res, fmt.Errorf("trackerclient: issue %s is filed under rig %q, which this read did not ask for", a.ID, rig)
		}
		res.Rigs[rig] = append(res.Rigs[rig], issue(a))
	}
	return res, nil
}

// ListByAssignee returns every issue, open or closed, assigned to assignee.
func (c *Client) ListByAssignee(assignee string) ([]*beads.Issue, error) {
	return c.List(beads.ListOptions{Status: "all", Assignee: assignee, Priority: beads.PriorityAny})
}

// GetAssignedIssue returns assignee's issue, preferring open over in_progress
// over hooked; nil when there is none.
func (c *Client) GetAssignedIssue(assignee string) (*beads.Issue, error) {
	all, err := c.ListByAssignee(assignee)
	if err != nil {
		return nil, err
	}
	for _, status := range []string{"open", "in_progress", beads.StatusHooked} {
		for _, is := range all {
			if is.Status == status {
				return is, nil
			}
		}
	}
	return nil, nil
}

func statusNames(statuses []beads.IssueStatus) []string {
	out := make([]string, 0, len(statuses))
	for _, s := range statuses {
		if s != "" {
			out = append(out, string(s))
		}
	}
	return out
}

// ListIssueStatuses returns the durable issues in any of statuses.
func (c *Client) ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	names := statusNames(statuses)
	if len(names) == 0 {
		return nil, nil
	}
	all, err := c.tc.ListAll(ctx(), api.ListFilter{Statuses: names})
	if err != nil {
		return nil, mapErr(err)
	}
	return issues(all), nil
}

// ListAssignedIssueStatuses returns assignee's issues and wisps in any of statuses.
func (c *Client) ListAssignedIssueStatuses(assignee string, statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	names := statusNames(statuses)
	if assignee == "" || len(names) == 0 {
		return nil, nil
	}
	var out []*beads.Issue
	for _, wisps := range []bool{false, true} {
		all, err := c.tc.ListAll(ctx(), api.ListFilter{Statuses: names, Assignee: assignee, Ephemeral: wisps, IncludeInfra: true})
		if err != nil {
			return nil, mapErr(err)
		}
		out = append(out, issues(all)...)
	}
	return out, nil
}

// readyFilter is the dispatchable board: the same exclusions the fake models.
func readyFilter() api.ReadyFilter {
	return api.ReadyFilter{ExcludeLabels: constants.NonDispatchableBeadLabels, ExcludeTypes: constants.NonDispatchableBeadTypes}
}

// Ready returns open, unblocked, dispatchable issues (the first page).
func (c *Client) Ready() ([]*beads.Issue, error) {
	page, err := c.tc.Ready(ctx(), readyFilter())
	if err != nil {
		return nil, mapErr(err)
	}
	return issues(page.Issues), nil
}

// ReadyAll returns every issue Ready would, across all pages.
func (c *Client) ReadyAll() ([]*beads.Issue, error) {
	all, err := c.tc.ReadyAll(ctx(), readyFilter())
	if err != nil {
		return nil, mapErr(err)
	}
	return issues(all), nil
}

// ReadyFiltered returns the ready issues matching opts. A zero Limit asks for
// the whole board, so it pages the tracker's ready query to the end; a
// positive one is that one page, and a page whose NextCursor is set has more
// rows behind it, so it comes back with *ErrReadyTruncated as the Client
// contract says. The tracker reports a cursor, not a board size, so the
// sentinel's TrueCount is the proven lower bound: one more than the page.
func (c *Client) ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error) {
	f := api.ReadyFilter{
		Unassigned:    opts.Unassigned,
		ExcludeLabels: opts.ExcludeLabels,
		ExcludeTypes:  opts.ExcludeTypes,
		Limit:         opts.Limit,
	}
	if opts.Limit == 0 {
		all, err := c.tc.ReadyAll(ctx(), f)
		if err != nil {
			return nil, mapErr(err)
		}
		return issues(all), nil
	}
	page, err := c.tc.Ready(ctx(), f)
	if err != nil {
		return nil, mapErr(err)
	}
	out := issues(page.Issues)
	if page.NextCursor != "" {
		return out, &beads.ErrReadyTruncated{Found: len(out), Cap: opts.Limit, TrueCount: len(out) + 1}
	}
	return out, nil
}

// Children returns the direct children of parentID.
func (c *Client) Children(parentID string) ([]*beads.Issue, error) {
	m, err := c.ChildrenOf(parentID)
	if err != nil {
		return nil, err
	}
	return m[parentID], nil
}

// ChildrenOf returns the direct children of each parent in one read, keyed by
// parent; a parent with no children is absent.
func (c *Client) ChildrenOf(parentIDs ...string) (map[string][]*beads.Issue, error) {
	m, err := c.tc.ChildrenOf(ctx(), parentIDs)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make(map[string][]*beads.Issue, len(m))
	for parent, kids := range m {
		out[parent] = issues(kids)
	}
	return out, nil
}
