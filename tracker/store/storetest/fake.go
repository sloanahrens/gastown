// Package storetest holds the in-memory store.Store for the unit tier. It
// implements the methods its Fake-flagged contract cases cover: create and
// get, the claim, release and transfer guards, the plain close those guards
// refuse, the creates a Batch applies, the comments and links an export
// reads, the Dependents a read of an issue names, the counts a stats read
// reports, and the tokens an admin issues and revokes. Every other method
// returns store.ErrUnimplemented, so a unit test
// that reaches past them fails loudly rather than reading a store that only
// looks modeled.
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

func (f *Fake) GetIssues(_ context.Context, ids []string) (map[string]api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]api.Issue{}
	for _, id := range ids {
		if is, ok := f.issues[id]; ok {
			out[id] = is
		}
	}
	f.fillDependentsLocked(out)
	return out, nil
}

// fillDependentsLocked sets Dependents on the issues in out from the links:
// the issues that depend on each, in the order Postgres lists them (link
// creation, then the dependent's id). A link names its target plainly, or as
// an external:<rig>:<id> reference whose last colon-separated part is the
// target, matched exactly. A dependent with both kinds of link to one target
// is listed once, at the plain link's relation. f.mu is held.
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
// it names it plainly. An external reference names its last colon-separated
// part, and only when it has a rig and an id: external:<rig>:<id>.
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

func (*Fake) ListIssues(context.Context, api.ListFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("ListIssues")
}
func (*Fake) CountIssues(context.Context, api.ListFilter) (int, error) {
	return 0, unimplemented("CountIssues")
}
func (*Fake) Assigned(context.Context, api.AssignedRequest) (api.AssignedPage, error) {
	return api.AssignedPage{}, unimplemented("Assigned")
}
func (*Fake) UpdateIssue(context.Context, string, api.UpdatePatch, api.Guard) error {
	return unimplemented("UpdateIssue")
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

// CloseIssues closes the issues of ids, or none when one is missing. It models
// only what the assignment guards read: status, close reason and times.
// Children and blockers are not modeled, so no close is ever refused.
func (f *Fake) CloseIssues(_ context.Context, ids []string, reason string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.issues[id]; !ok {
			return api.NotFound(id)
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
func (*Fake) DeleteIssues(context.Context, []string) error { return unimplemented("DeleteIssues") }

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
func (*Fake) Comments(context.Context, string) ([]api.Comment, error) {
	return nil, unimplemented("Comments")
}

// AddDependency records a link from issue to dependsOn. Export and the
// Dependents that GetIssues fills read the links; Dependencies stays
// unimplemented.
func (f *Fake) AddDependency(ctx context.Context, issue, dependsOn, depType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.issues[issue]; !ok {
		return api.NotFound(issue)
	}
	f.links = append(f.links, api.ExportDependency{
		IssueID: issue, DependsOn: dependsOn, Type: depType, CreatedBy: api.ActorFrom(ctx), CreatedAt: f.now(),
	})
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
func (*Fake) ChildrenOf(context.Context, []string) (map[string][]api.Issue, error) {
	return nil, unimplemented("ChildrenOf")
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
