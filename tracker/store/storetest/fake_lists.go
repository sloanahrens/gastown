package storetest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// The fake pages a list as the real store does, so a caller that walks pages
// sees the same sizes here.
const (
	fakeDefaultPageSize = 500
	fakeMaxPageSize     = 1000
)

func fakePageSize(limit int) int {
	switch {
	case limit <= 0:
		return fakeDefaultPageSize
	case limit > fakeMaxPageSize:
		return fakeMaxPageSize
	}
	return limit
}

// fakeCursor is the sort key of the last row of a page. It keeps the real
// store's JSON names, so a page number means the same thing here.
type fakeCursor struct {
	Priority  int       `json:"p"`
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

func encodeFakeCursor(is api.Issue) string {
	c := fakeCursor{Priority: is.Priority, CreatedAt: is.CreatedAt.UTC(), ID: is.ID}
	b, _ := json.Marshal(c) // three plain fields; Marshal cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeFakeCursor reads a cursor; "" is the first page. A cursor the fake
// cannot read is refused rather than served as a first page, which would
// silently repeat rows.
func decodeFakeCursor(s string) (*fakeCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, api.Invalid("invalid cursor")
	}
	var c fakeCursor
	if err := json.Unmarshal(b, &c); err != nil || c.ID == "" {
		return nil, api.Invalid("invalid cursor")
	}
	return &c, nil
}

// after reports whether is sorts after the cursor's row.
func (c fakeCursor) after(is api.Issue) bool {
	if is.Priority != c.Priority {
		return is.Priority > c.Priority
	}
	if !is.CreatedAt.Equal(c.CreatedAt) {
		return is.CreatedAt.After(c.CreatedAt)
	}
	return is.ID > c.ID
}

// ListIssues returns the page of issues matching f. The fake models the
// filters the shim's read handlers send; a filter it does not model is
// refused with store.ErrUnimplemented rather than dropped, so a caller never
// reads a wider answer than it asked for.
func (f *Fake) ListIssues(_ context.Context, flt api.ListFilter) (api.IssuePage, error) {
	if err := refuseUnmodeledListFilter(flt); err != nil {
		return api.IssuePage{}, err
	}
	cur, err := decodeFakeCursor(flt.Cursor)
	if err != nil {
		return api.IssuePage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var rows []api.Issue
	for _, is := range f.issues {
		if !listMatches(is, flt) {
			continue
		}
		if cur != nil && !cur.after(is) {
			continue
		}
		rows = append(rows, is)
	}
	slices.SortFunc(rows, func(a, b api.Issue) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})

	page := api.IssuePage{}
	if size := fakePageSize(flt.Limit); len(rows) > size {
		rows = rows[:size]
		page.NextCursor = encodeFakeCursor(rows[size-1])
	}
	f.fillCountsLocked(rows)
	page.Issues = rows
	return page, nil
}

// refuseUnmodeledListFilter names the filter the fake does not model, or nil.
func refuseUnmodeledListFilter(flt api.ListFilter) error {
	var unmodeled string
	switch {
	case flt.Rig != "":
		unmodeled = "Rig"
	case len(flt.Rigs) > 0:
		unmodeled = "Rigs"
	case flt.Assignee != "":
		unmodeled = "Assignee"
	case flt.NoAssignee:
		unmodeled = "NoAssignee"
	case flt.Priority != nil:
		unmodeled = "Priority"
	case !flt.ClosedAfter.IsZero() || !flt.ClosedBefore.IsZero():
		unmodeled = "ClosedAfter/ClosedBefore"
	case !flt.CreatedAfter.IsZero() || !flt.CreatedBefore.IsZero():
		unmodeled = "CreatedAfter/CreatedBefore"
	}
	if unmodeled == "" {
		return nil
	}
	return fmt.Errorf("%w: Fake.ListIssues cannot express the %s filter", store.ErrUnimplemented, unmodeled)
}

// listMatches reports whether is is in flt's answer, applying the same rules
// the list query does.
func listMatches(is api.Issue, flt api.ListFilter) bool {
	if is.Ephemeral != flt.Ephemeral {
		return false
	}
	if !flt.IncludeInfra && flt.IssueType == "" && api.IsInfraType(is.Type) {
		return false
	}
	if !slices.Contains(flt.Statuses, "all") {
		if len(flt.Statuses) == 0 {
			if is.Status == "closed" {
				return false
			}
		} else if !slices.Contains(flt.Statuses, is.Status) {
			return false
		}
	}
	labels := uniqueLabels(flt.Labels)
	if len(labels) > 0 {
		have := map[string]bool{}
		for _, l := range is.Labels {
			have[l] = true
		}
		for _, l := range labels {
			if !have[l] {
				return false
			}
		}
	}
	if flt.Parent != "" && is.Parent != flt.Parent {
		return false
	}
	if flt.IssueType != "" && is.Type != flt.IssueType {
		return false
	}
	if len(flt.IDs) > 0 && !slices.Contains(flt.IDs, is.ID) {
		return false
	}
	return true
}

// uniqueLabels keeps one copy of each label, as the list query does.
func uniqueLabels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// fillCountsLocked adds the three counts a list fills to each row: comments,
// links out of the issue, and links into it, counting every relation as the
// real store's list does. f.mu is held.
func (f *Fake) fillCountsLocked(rows []api.Issue) {
	if len(rows) == 0 {
		return
	}
	comments := map[string]int{}
	for _, c := range f.comments {
		comments[c.IssueID]++
	}
	out, in := map[string]int{}, map[string]int{}
	for _, l := range f.links {
		out[l.IssueID]++
		in[l.DependsOn]++
	}
	for i := range rows {
		id := rows[i].ID
		rows[i].CommentCount = comments[id]
		rows[i].DependencyCount = out[id]
		rows[i].DependentCount = in[id]
	}
}

// ChildrenOf returns the direct children of each parent, keyed by parent, in
// the order the real store lists them (creation time, then id). A parent with
// no children is absent.
func (f *Fake) ChildrenOf(_ context.Context, parents []string) (map[string][]api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]api.Issue{}
	wanted := map[string]bool{}
	for _, p := range parents {
		wanted[p] = true
	}
	for _, is := range f.issues {
		if is.Parent == "" || !wanted[is.Parent] {
			continue
		}
		out[is.Parent] = append(out[is.Parent], is)
	}
	for parent := range out {
		slices.SortFunc(out[parent], func(a, b api.Issue) int {
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	return out, nil
}

// Comments returns the issue's comments, oldest first. A missing issue is not
// found, as the real store answers.
func (f *Fake) Comments(_ context.Context, id string) ([]api.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[id]; !ok {
		return nil, api.NotFound(id)
	}
	out := []api.Comment{}
	for _, c := range f.comments {
		if c.IssueID == id {
			out = append(out, c)
		}
	}
	return out, nil
}
