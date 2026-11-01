// Package storetest holds the in-memory store.Store for the unit tier. It
// implements the methods its Fake-flagged contract cases cover: create and
// get, the claim, release and transfer guards, the plain close those guards
// refuse, a status write with the guards and the close fences, the creates a
// Batch applies, the comments and links an export reads, the counts a stats
// read reports, and the tokens an admin issues and revokes. GetIssues fills
// Dependencies and Dependents, as the real store does; ListIssues, ChildrenOf
// and Comments answer the reads the bd shim's handlers make (gt-p85pw.94),
// CommentCounts returns each issue's comment count in one read, UpdateIssue
// answers the status the shim's update, close and reopen write (gt-p85pw.96)
// and the text fields its delete and defer write (gt-p85pw.97), and DeleteIssues
// deletes a batch with its links. Every other method returns
// store.ErrUnimplemented, so a unit test that reaches past them fails loudly
// rather than reading a store that only looks modeled.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Fake is an in-memory store.Store.
type Fake struct {
	mu       sync.Mutex
	now      func() time.Time
	issues   map[string]api.Issue
	comments []api.Comment
	links    []api.ExportDependency
	// tokens is keyed by the token itself; the fake keeps no hash because
	// nothing it holds ever leaves this process.
	tokens    map[string]fakeToken
	next      int
	nextToken int
}

// fakeToken is one issued token as the fake keeps it.
type fakeToken struct {
	id      string
	actor   string
	scope   api.Scope
	expires time.Time // zero means never
	revoked bool
}

var _ store.Store = (*Fake)(nil)

// NewFake returns an empty Fake using the real clock.
func NewFake() *Fake { return NewFakeWithClock(time.Now) }

// NewFakeWithClock returns an empty Fake whose timestamps come from now.
func NewFakeWithClock(now func() time.Time) *Fake {
	return &Fake{now: now, issues: map[string]api.Issue{}, tokens: map[string]fakeToken{}}
}

// rigOf is the rig prefix of an id: the text before its first dash.
func rigOf(id string) string {
	rig, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	return rig
}

func (f *Fake) CreateIssue(_ context.Context, p api.CreateParams) (api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createLocked(p)
}

// createLocked applies p with f.mu held, so Batch can write through it.
func (f *Fake) createLocked(p api.CreateParams) (api.Issue, error) {
	if strings.TrimSpace(p.Title) == "" {
		return api.Issue{}, api.Invalid("title is required")
	}
	id := p.ID
	switch {
	case id != "":
		rig := rigOf(id)
		if rig == "" {
			return api.Issue{}, api.Invalid("id %q has no rig prefix", id)
		}
		if p.Rig != "" && p.Rig != rig {
			return api.Issue{}, api.Invalid("id %q does not belong to rig %q", id, p.Rig)
		}
	case p.Rig != "":
		f.next++
		id = fmt.Sprintf("%s-%05d", p.Rig, f.next)
	default:
		return api.Issue{}, api.Invalid("a rig or an id is required")
	}
	if _, dup := f.issues[id]; dup {
		return api.Issue{}, api.Conflict("issue %s already exists", id)
	}
	typ := p.Type
	if typ == "" {
		typ = "task"
	}
	labels := slices.Clone(p.Labels)
	slices.Sort(labels)
	now := f.now()
	is := api.Issue{
		ID: id, Title: p.Title, Description: p.Description, AcceptanceCriteria: p.Acceptance,
		Status: "open", Priority: p.Priority, Type: typ, Assignee: p.Assignee, CreatedBy: p.Actor,
		CreatedAt: now, UpdatedAt: now, Parent: p.Parent, Labels: labels, Ephemeral: p.Ephemeral,
		EventKind: p.EventKind, Payload: p.EventPayload,
	}
	f.issues[id] = is
	return is, nil
}

// GetIssues returns the issues that exist among ids, with their links. Every
// caller sees Dependencies filled, as it does on Postgres: a named read
// answers what the issue depends on, and the contract case "a named read
// carries each link's own record" holds both stores to it.
func (f *Fake) GetIssues(_ context.Context, ids []string) (map[string]api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]api.Issue{}
	for _, id := range ids {
		if is, ok := f.issues[id]; ok {
			// A named read fills Dependents even when nothing depends on the
			// issue (api.Issue.Dependents); depsNoDependentsIsEmpty pins it.
			is.Dependents = make([]api.IssueDep, 0)
			out[id] = is
		}
	}
	f.fillDependenciesLocked(out)
	f.fillDependentsLocked(out)
	return out, nil
}

// fillDependenciesLocked sets Dependencies on the issues in out from the
// links, in the order Postgres lists them (link creation, then the target's
// id). Targets that are not issues here (external references) are left out,
// as the real store does. f.mu is held.
func (f *Fake) fillDependenciesLocked(out map[string]api.Issue) {
	type link struct {
		dep api.IssueDep
		at  time.Time
	}
	byOwner := map[string][]link{}
	for _, l := range f.links {
		target, ok := f.issues[l.DependsOn]
		if !ok {
			continue
		}
		byOwner[l.IssueID] = append(byOwner[l.IssueID], link{
			dep: api.IssueDep{
				ID: target.ID, Title: target.Title, Status: target.Status, Priority: target.Priority,
				Type: target.Type, DependencyType: l.Type, CloseReason: target.CloseReason,
				CreatedAt: l.CreatedAt, CreatedBy: l.CreatedBy, Metadata: l.Metadata,
			},
			at: l.CreatedAt,
		})
	}
	for owner, links := range byOwner {
		is, ok := out[owner]
		if !ok {
			continue
		}
		slices.SortStableFunc(links, func(a, b link) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return strings.Compare(a.dep.ID, b.dep.ID)
		})
		is.Dependencies = make([]api.IssueDep, 0, len(links))
		for _, l := range links {
			is.Dependencies = append(is.Dependencies, l.dep)
		}
		out[owner] = is
	}
}

// fillDependentsLocked sets Dependents on the issues in out from the links, in
// the order Postgres lists them (link creation, then the dependent's id). The
// link rule is stated on the contract case depsExternalDependents. f.mu is
// held.
func (f *Fake) fillDependentsLocked(out map[string]api.Issue) {
	type pick struct {
		dep   api.IssueDep
		at    time.Time
		plain bool
	}
	picks := map[string]map[string]pick{}
	for _, l := range f.links {
		dependent, ok := f.issues[l.IssueID]
		if !ok {
			continue
		}
		target, plain, ok := linkTarget(l.DependsOn, out)
		if !ok {
			continue
		}
		if have, dup := picks[target][l.IssueID]; dup && (have.plain || !plain) {
			continue
		}
		if picks[target] == nil {
			picks[target] = map[string]pick{}
		}
		picks[target][l.IssueID] = pick{
			dep: api.IssueDep{
				ID: dependent.ID, Title: dependent.Title, Status: dependent.Status, Priority: dependent.Priority,
				Type: dependent.Type, DependencyType: l.Type, CloseReason: dependent.CloseReason,
			},
			at:    l.CreatedAt,
			plain: plain,
		}
	}
	for target, byDependent := range picks {
		ps := make([]pick, 0, len(byDependent))
		for _, p := range byDependent {
			ps = append(ps, p)
		}
		slices.SortFunc(ps, func(a, b pick) int {
			if c := a.at.Compare(b.at); c != 0 {
				return c
			}
			return strings.Compare(a.dep.ID, b.dep.ID)
		})
		is := out[target]
		is.Dependents = make([]api.IssueDep, 0, len(ps))
		for _, p := range ps {
			is.Dependents = append(is.Dependents, p.dep)
		}
		out[target] = is
	}
}

// linkTarget reports which issue in out a link's depends_on names, and whether
// it names it plainly.
func linkTarget(dependsOn string, out map[string]api.Issue) (target string, plain, ok bool) {
	if _, found := out[dependsOn]; found {
		return dependsOn, true, true
	}
	if !strings.HasPrefix(dependsOn, "external:") || strings.Count(dependsOn, ":") < 2 {
		return "", false, false
	}
	local := dependsOn[strings.LastIndex(dependsOn, ":")+1:]
	if _, found := out[local]; found && local != "" {
		return local, false, true
	}
	return "", false, false
}

func unimplemented(method string) error {
	return fmt.Errorf("%w: Fake.%s", store.ErrUnimplemented, method)
}

func (*Fake) CountIssues(context.Context, api.ListFilter) (int, error) {
	return 0, unimplemented("CountIssues")
}
func (*Fake) Assigned(context.Context, api.AssignedRequest) (api.AssignedPage, error) {
	return api.AssignedPage{}, unimplemented("Assigned")
}

// builtinStatuses are the statuses a write may name without a custom status,
// as the Postgres store's isBuiltinStatus decides it. The fake keeps no
// custom_statuses table, so a name outside this list is refused rather than
// stored.
var builtinStatuses = []string{"open", "in_progress", "closed", "deferred", "hooked", "pinned"}

// UpdateIssue applies the fields of patch the fake models while g holds: the
// status the bd shim's update, close and reopen handlers write (gt-p85pw.96),
// and the title, description, acceptance criteria and appended notes its delete
// and defer handlers write (gt-p85pw.97). A status of closed obeys the close
// fences and a status that leaves closed clears the close, as the Postgres
// store does; a guard that does not hold writes nothing. Any other patch field
// is refused rather than applied in part. (contract: updateStatusOnly)
func (f *Fake) UpdateIssue(_ context.Context, id string, patch api.UpdatePatch, g api.Guard) error {
	if err := fakePatch(patch); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is, ok := f.issues[id]
	if !ok {
		return api.NotFound(id)
	}
	if g.IfAssignee != nil && is.Assignee != *g.IfAssignee {
		return api.GuardFailed(fmt.Sprintf("%s is assigned to %q, not %q", id, is.Assignee, *g.IfAssignee))
	}
	if g.IfStatus != nil && is.Status != *g.IfStatus {
		return api.GuardFailed(fmt.Sprintf("%s has status %q, not %q", id, is.Status, *g.IfStatus))
	}
	changed := false
	now := f.now()
	if patch.Title != nil && *patch.Title != is.Title {
		if strings.TrimSpace(*patch.Title) == "" {
			return api.Invalid("title cannot be empty")
		}
		is.Title, changed = *patch.Title, true
	}
	if patch.Description != nil && *patch.Description != is.Description {
		is.Description, changed = *patch.Description, true
	}
	if patch.Acceptance != nil && *patch.Acceptance != is.AcceptanceCriteria {
		is.AcceptanceCriteria, changed = *patch.Acceptance, true
	}
	if patch.AppendNotes != "" {
		is.Notes, changed = appendNote(is.Notes, patch.AppendNotes), true
	}
	if patch.Status != nil && *patch.Status != is.Status {
		status := *patch.Status
		if !slices.Contains(builtinStatuses, status) {
			return api.Invalid("unknown status %q", status)
		}
		switch {
		case status == "closed":
			if err := f.fenceCloseLocked(is, []string{id}, patch.Force, patch.Actor); err != nil {
				return err
			}
			closedAt := now
			is.Status, is.CloseReason, is.ClosedAt = "closed", "", &closedAt
		case is.Status == "closed":
			is.Status, is.CloseReason, is.ClosedAt = status, "", nil
		default:
			is.Status = status
		}
		changed = true
	}
	if !changed {
		return nil
	}
	is.UpdatedAt = now
	f.issues[id] = is
	return nil
}

// appendNote joins line to notes with one newline between, as the Postgres
// store's bd update --append-notes does.
func appendNote(notes, line string) string {
	if line == "" {
		return notes
	}
	if notes == "" {
		return line
	}
	return strings.TrimRight(notes, "\n") + "\n" + line
}

// fakePatch refuses a patch carrying a field the fake does not model, so a
// caller never reads a write that happened in part. Actor is the request's
// identity, not a field; title, description, acceptance, notes and status are
// the modeled ones.
func fakePatch(patch api.UpdatePatch) error {
	switch {
	case patch.Priority != nil, patch.Assignee != nil,
		len(patch.AddLabels) > 0, len(patch.RemoveLabels) > 0, len(patch.SetLabels) > 0,
		patch.Persistent, patch.DeferUntil != nil:
		return unimplemented("UpdateIssue: a patch field other than title, description, acceptance, notes or status")
	}
	return nil
}

// fenceCloseLocked refuses the close of is that the Postgres store refuses
// unless forced (closeRow): a pinned issue, an issue another actor holds, an
// open child and an unresolved blocker, where the ids the same call closes
// (closing) do not count. A child is a parent_id or a parent-child link, since
// the fixture seeds either. f.mu is held.
func (f *Fake) fenceCloseLocked(is api.Issue, closing []string, force bool, actor string) error {
	if force {
		return nil
	}
	if is.Status == "pinned" {
		return api.Conflict("%s is pinned (release it, or force)", is.ID)
	}
	if is.Assignee != "" && is.Assignee != actor {
		return api.Conflict("%s is assigned to %s, not to %s (release it, or force)", is.ID, is.Assignee, actor)
	}
	inBatch := make(map[string]bool, len(closing))
	for _, id := range closing {
		inBatch[id] = true
	}
	var children, blockers []string
	for id, kid := range f.issues {
		if inBatch[id] || kid.Status == "closed" {
			continue
		}
		if kid.Parent == is.ID || f.hasLink(id, is.ID, "parent-child") {
			children = append(children, id)
		}
	}
	for _, l := range f.links {
		if l.IssueID != is.ID || !slices.Contains(api.BlockingLinkTypes, l.Type) {
			continue
		}
		target, ok := f.issues[l.DependsOn]
		if !ok || inBatch[l.DependsOn] || slices.Contains(api.ResolvedStatuses, target.Status) {
			continue
		}
		blockers = append(blockers, l.DependsOn)
	}
	slices.Sort(children)
	slices.Sort(blockers)
	if len(children) > 0 {
		return api.Conflict("%s has open children: %s (close them first, or force)", is.ID, strings.Join(children, ", "))
	}
	if len(blockers) > 0 {
		return api.Conflict("%s is blocked by open issues: %s (close them first, or force)", is.ID, strings.Join(blockers, ", "))
	}
	return nil
}

// hasLink reports whether issue dependsOn link of type exists. f.mu is held.
func (f *Fake) hasLink(issue, dependsOn, depType string) bool {
	for _, l := range f.links {
		if l.IssueID == issue && l.DependsOn == dependsOn && l.Type == depType {
			return true
		}
	}
	return false
}

// assignVerb runs one assignment verb on issue id: it refuses a request with
// no actor, a missing issue and a closed issue before change sees the row, so
// change only decides the holder guard and the write. verb names the verb as
// the no-actor error says it; done names it as the closed refusal says it.
func (f *Fake) assignVerb(ctx context.Context, id, verb, done string, change func(*api.Issue, string) error) error {
	actor := api.ActorFrom(ctx)
	if actor == "" {
		return api.Invalid("%s needs an actor; the request carries no identity", verb)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is, ok := f.issues[id]
	if !ok {
		return api.NotFound(id)
	}
	if is.Status == "closed" {
		return api.GuardFailed(fmt.Sprintf("%s is closed, so it cannot be %s", id, done))
	}
	if err := change(&is, actor); err != nil {
		return err
	}
	f.issues[id] = is
	return nil
}

// Claim moves the issue to in_progress for the actor on the context.
func (f *Fake) Claim(ctx context.Context, id string) error {
	return f.assignVerb(ctx, id, "claim", "claimed", func(is *api.Issue, actor string) error {
		if is.Assignee != "" && is.Assignee != actor {
			return api.GuardFailed(fmt.Sprintf("%s is held by another actor", id))
		}
		if is.Assignee == actor && is.Status == "in_progress" {
			return nil // nothing changes, so nothing is written
		}
		is.Status, is.Assignee, is.UpdatedAt = "in_progress", actor, f.now()
		return nil
	})
}

// Release returns the holder's issue to open and unassigned.
func (f *Fake) Release(ctx context.Context, id string) error {
	return f.assignVerb(ctx, id, "release", "released", func(is *api.Issue, actor string) error {
		if is.Assignee != actor {
			return api.GuardFailed(fmt.Sprintf("%s is assigned to another actor", id))
		}
		is.Status, is.Assignee, is.UpdatedAt = "open", "", f.now()
		return nil
	})
}

// Transfer hands the holder's issue to assignee at status (the current status
// when empty).
func (f *Fake) Transfer(ctx context.Context, id, status, assignee string) error {
	if err := api.ValidateTransferAssignee(assignee); err != nil {
		return err
	}
	if err := api.ValidateTransferStatus(status); err != nil {
		return err
	}
	return f.assignVerb(ctx, id, "transfer", "transferred", func(is *api.Issue, actor string) error {
		if is.Assignee != actor {
			return api.GuardFailed(fmt.Sprintf("%s is assigned to another actor", id))
		}
		if status == "" {
			status = is.Status
		}
		if is.Status == status && is.Assignee == assignee {
			return nil // nothing changes, so nothing is written
		}
		is.Status, is.Assignee, is.UpdatedAt = status, assignee, f.now()
		return nil
	})
}

// CloseIssues closes the issues of ids, or none when one is missing or fenced.
// The fences are the Postgres store's: a pin, another actor's claim, an open
// child and an unresolved blocker, with the ids this call closes counted as
// closed. An issue already closed is skipped, not refused.
func (f *Fake) CloseIssues(ctx context.Context, ids []string, reason string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.issues[id]; !ok {
			return api.NotFound(id)
		}
	}
	actor := api.ActorFrom(ctx)
	for _, id := range ids {
		is := f.issues[id]
		if is.Status == "closed" {
			continue
		}
		if err := f.fenceCloseLocked(is, ids, force, actor); err != nil {
			return err
		}
	}
	now := f.now()
	for _, id := range ids {
		is := f.issues[id]
		if is.Status == "closed" {
			continue
		}
		closedAt := now
		is.Status, is.CloseReason, is.ClosedAt, is.UpdatedAt = "closed", reason, &closedAt, now
		f.issues[id] = is
	}
	return nil
}

// DeleteIssues deletes ids, or none: an id the fake does not hold refuses the
// batch before anything is written. Every link touching a deleted issue goes in
// both directions, and a child that survives its deleted parent is left without
// one, as the Postgres store does.
func (f *Fake) DeleteIssues(_ context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := f.issues[id]; !ok {
			return api.NotFound(id)
		}
		gone[id] = true
	}
	for id := range gone {
		delete(f.issues, id)
	}
	kept := f.links[:0]
	for _, l := range f.links {
		if gone[l.IssueID] || gone[l.DependsOn] {
			continue
		}
		kept = append(kept, l)
	}
	f.links = kept
	for id, is := range f.issues {
		if gone[is.Parent] {
			is.Parent = ""
			f.issues[id] = is
		}
	}
	return nil
}

// AddComment appends a comment to id. Export reads the comments; Comments
// stays unimplemented.
func (f *Fake) AddComment(_ context.Context, id, author, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[id]; !ok {
		return api.NotFound(id)
	}
	f.comments = append(f.comments, api.Comment{
		ID: fmt.Sprintf("comment-%d", len(f.comments)+1), IssueID: id, Author: author, Text: text, CreatedAt: f.now(),
	})
	return nil
}

// CommentCounts returns how many comments each of ids has, refusing an id the
// fake does not hold as the real store does. Empty ids are ignored.
func (f *Fake) CommentCounts(_ context.Context, ids []string) (map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := f.issues[id]; !ok {
			return nil, api.NotFound(id)
		}
		out[id] = 0
	}
	for _, c := range f.comments {
		if _, ok := out[c.IssueID]; ok {
			out[c.IssueID]++
		}
	}
	return out, nil
}

// AddDependency records a link from issue to dependsOn. Export, the Dependents
// that GetIssues fills, and the dependency reads GetIssues and ListIssues answer
// all read the links.
func (f *Fake) AddDependency(ctx context.Context, issue, dependsOn, depType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[issue]; !ok {
		return api.NotFound(issue)
	}
	f.links = append(f.links, api.ExportDependency{
		IssueID: issue, DependsOn: dependsOn, Type: depType, CreatedBy: api.ActorFrom(ctx), CreatedAt: f.now(),
	})
	// The real store keeps a child's parent on the issue row as well as in the
	// link, and ChildrenOf and a list's parent field read it.
	if depType == "parent-child" {
		is := f.issues[issue]
		if is.Parent == "" {
			is.Parent = dependsOn
			f.issues[issue] = is
		}
	}
	return nil
}
func (*Fake) RemoveDependency(context.Context, string, string) error {
	return unimplemented("RemoveDependency")
}
func (*Fake) Dependencies(context.Context, string, string) ([]api.IssueDep, error) {
	return nil, unimplemented("Dependencies")
}
func (*Fake) Ready(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Ready")
}
func (*Fake) Blocked(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Blocked")
}

// Batch applies the creates the fake models in request order, and restores the
// issues it already holds when one fails: a refused batch leaves nothing. Any
// other op kind is unimplemented, as the fake models no update, close or
// comment.
func (f *Fake) Batch(ctx context.Context, req api.BatchRequest) (api.BatchResult, error) {
	if err := req.Validate(); err != nil {
		return api.BatchResult{}, err
	}
	actor := api.ActorFrom(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	before, next := maps.Clone(f.issues), f.next
	res := api.BatchResult{Results: make([]api.BatchOperationResult, 0, len(req.Operations))}
	for i, op := range req.Operations {
		if op.Op != api.BatchOpCreate {
			f.issues, f.next = before, next // a refused batch writes none of its operations
			return api.BatchResult{}, unimplemented("Batch")
		}
		params := *op.Create
		params.Actor = actor
		is, err := f.createLocked(params)
		if err != nil {
			f.issues, f.next = before, next
			return api.BatchResult{}, api.BatchError(i, err)
		}
		res.Results = append(res.Results, api.BatchOperationResult{Index: i, IDs: []string{is.ID}, Issue: &is})
	}
	return res, nil
}
func (*Fake) CreateGraph(context.Context, api.GraphRequest) (api.GraphResult, error) {
	return api.GraphResult{}, unimplemented("CreateGraph")
}
func (*Fake) DispatchBoard(context.Context, api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
	return api.DispatchBoardResponse{}, unimplemented("DispatchBoard")
}
func (*Fake) TownSnapshot(context.Context, api.SnapshotRequest) (api.TownSnapshot, error) {
	return api.TownSnapshot{}, unimplemented("TownSnapshot")
}

// Stats counts the durable issues of rig (every rig when empty) by status, and
// its wisps as one total. A status only wisps hold is left out of ByStatus, as
// the Postgres store leaves it out.
func (f *Fake) Stats(_ context.Context, rig string) (api.Stats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := api.Stats{ByStatus: map[string]int{}}
	for _, is := range f.issues {
		if rig != "" && rigOf(is.ID) != rig {
			continue
		}
		if is.Ephemeral {
			out.Wisps++
			continue
		}
		out.Total++
		out.ByStatus[is.Status]++
	}
	return out, nil
}
func (*Fake) ConfigGet(context.Context, string) (string, bool, error) {
	return "", false, unimplemented("ConfigGet")
}
func (*Fake) ConfigSet(context.Context, string, string) error { return unimplemented("ConfigSet") }

// IssueTokenScoped stores a token for actor with scope and returns it. The
// token is random like the Postgres store's, so two issues never collide, and
// an actor the token routes refuse is refused here, so a unit test cannot issue
// for an identity the API never mints.
func (f *Fake) IssueTokenScoped(_ context.Context, actor string, scope api.Scope, expires *time.Time) (string, error) {
	if err := api.ValidateTokenActor(actor); err != nil {
		return "", err
	}
	if !scope.Valid() {
		return "", api.Invalid("scope %q is not seat or admin", scope)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("storetest: generate token: %w", err)
	}
	token := "trk_" + hex.EncodeToString(b)
	var exp time.Time
	if expires != nil {
		exp = *expires
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextToken++
	f.tokens[token] = fakeToken{id: fmt.Sprintf("token-%05d", f.nextToken), actor: actor, scope: scope, expires: exp}
	return token, nil
}

// Identify returns the actor and scope of a live token, or api.ErrUnknownToken
// for one that is not known, was revoked or has expired.
func (f *Fake) Identify(_ context.Context, token string) (api.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[token]
	if !ok || !f.liveLocked(t) {
		return api.Identity{}, api.ErrUnknownToken
	}
	return api.Identity{Actor: t.actor, Scope: t.scope}, nil
}

// liveLocked reports whether t still authorizes: not revoked and not expired.
func (f *Fake) liveLocked(t fakeToken) bool {
	switch {
	case t.revoked:
		return false
	case t.expires.IsZero():
		return true
	}
	return f.now().Before(t.expires)
}

// RevokeActorTokens revokes every live token of actor and reports how many it
// ended; an actor with none is a success that ended zero. An actor the token
// routes refuse is refused here too, so a unit test cannot revoke for an
// identity the API never names.
func (f *Fake) RevokeActorTokens(_ context.Context, actor string) (int, error) {
	if err := api.ValidateTokenActor(actor); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for token, t := range f.tokens {
		if t.actor == actor && f.liveLocked(t) {
			t.revoked = true
			f.tokens[token] = t
			n++
		}
	}
	return n, nil
}

// RevokeTokenID revokes the live token with id, reporting false when id names
// none that is live.
func (f *Fake) RevokeTokenID(_ context.Context, id string) (bool, error) {
	if id == "" {
		return false, api.Invalid("revoking by id needs the token id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for token, t := range f.tokens {
		if t.id == id && f.liveLocked(t) {
			t.revoked = true
			f.tokens[token] = t
			return true, nil
		}
	}
	return false, nil
}
func (*Fake) EventsAfter(context.Context, int64, int) (api.EventsPage, error) {
	return api.EventsPage{}, unimplemented("EventsAfter")
}

// Export emits rig's records in the Postgres store's order: its issues by id,
// then their labels, then the dependencies and comments of those issues, each
// in insertion order within an issue. The records are built under the lock and
// emitted after it is released, so emit may call back into the fake.
func (f *Fake) Export(_ context.Context, rig string, emit func(api.ExportRecord) error) error {
	for _, rec := range f.exportRecords(rig) {
		if err := emit(rec); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) exportRecords(rig string) []api.ExportRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var issues []api.Issue
	for _, is := range f.issues {
		if rigOf(is.ID) == rig {
			issues = append(issues, is)
		}
	}
	slices.SortFunc(issues, func(a, b api.Issue) int { return strings.Compare(a.ID, b.ID) })
	inRig := func(issueID string) bool { return rigOf(issueID) == rig }
	links := slices.DeleteFunc(slices.Clone(f.links), func(d api.ExportDependency) bool { return !inRig(d.IssueID) })
	comments := slices.DeleteFunc(slices.Clone(f.comments), func(c api.Comment) bool { return !inRig(c.IssueID) })
	slices.SortStableFunc(links, func(a, b api.ExportDependency) int { return strings.Compare(a.IssueID, b.IssueID) })
	slices.SortStableFunc(comments, func(a, b api.Comment) int { return strings.Compare(a.IssueID, b.IssueID) })

	var out []api.ExportRecord
	for _, is := range issues {
		rec := is
		rec.Labels = nil
		out = append(out, api.ExportRecord{Type: api.RecordIssue, Issue: &rec})
	}
	for _, is := range issues {
		for _, l := range is.Labels {
			out = append(out, api.ExportRecord{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: is.ID, Label: l}})
		}
	}
	for _, d := range links {
		out = append(out, api.ExportRecord{Type: api.RecordDependency, Dependency: &d})
	}
	for _, c := range comments {
		out = append(out, api.ExportRecord{Type: api.RecordComment, Comment: &c})
	}
	return out
}
