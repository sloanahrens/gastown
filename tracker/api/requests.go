package api

import (
	"strings"
	"unicode"
)

// Request and response bodies of the /v1 routes. A route with no body type
// here uses ListFilter, ReadyFilter, CreateParams, Issue or IssuePage directly.

// ShowManyRequest asks for several issues by id.
type ShowManyRequest struct {
	IDs []string `json:"ids"`
}

// ShowManyResponse holds the issues that exist among the ids asked for.
type ShowManyResponse struct {
	Issues map[string]Issue `json:"issues"`
}

// UpdateRequest is a patch applied while a guard holds.
type UpdateRequest struct {
	Patch UpdatePatch `json:"patch"`
	Guard Guard       `json:"guard"`
}

// TransferRequest hands the caller's issue to another actor. Status is the
// status the issue moves to, and an empty one keeps the status it has.
// Assignee is required: leaving an issue unheld is a release, not a transfer.
// Closing is not a transfer, whatever status it names; CloseRequest does that.
type TransferRequest struct {
	Assignee string `json:"assignee"`
	Status   string `json:"status,omitempty"`
}

// MaxAssigneeLen bounds the assignee a transfer names: a seat address is
// short, so a longer string is a caller's mistake rather than a seat.
const MaxAssigneeLen = 128

// Validate refuses a transfer that names no usable assignee or asks for a
// close; an empty status is valid and keeps the issue's own.
func (r TransferRequest) Validate() error {
	if err := ValidateTransferAssignee(r.Assignee); err != nil {
		return err
	}
	return ValidateTransferStatus(r.Status)
}

// ValidateTransferAssignee refuses an assignee a transfer may not name: blank,
// padded or holding whitespace (so not one address), over MaxAssigneeLen, or
// holding a control character (so not one a log line can print). The error for
// a blank one names the verb the caller probably meant.
func ValidateTransferAssignee(assignee string) error {
	switch {
	case strings.TrimSpace(assignee) == "":
		return Invalid("transfer needs the assignee taking the issue; release leaves it unheld")
	case len(assignee) > MaxAssigneeLen:
		return Invalid("assignee is %d bytes, at most %d", len(assignee), MaxAssigneeLen)
	case strings.ContainsFunc(assignee, unicode.IsSpace):
		return Invalid("assignee %q holds whitespace; a seat address has none", assignee)
	case strings.ContainsFunc(assignee, unicode.IsControl):
		return Invalid("assignee %q holds a control character; a seat address has none", assignee)
	}
	return nil
}

// ValidateTransferStatus refuses the status a transfer may not set: the close
// verb owns closing, with the fences that keep a blocked issue open.
func ValidateTransferStatus(status string) error {
	if status == "closed" {
		return Invalid("transfer cannot close an issue; close it with the close verb")
	}
	return nil
}

// CloseRequest closes several issues together.
type CloseRequest struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason,omitempty"`
	Force  bool     `json:"force,omitempty"`
}

// DeleteRequest deletes several issues together.
type DeleteRequest struct {
	IDs []string `json:"ids"`
}

// CommentRequest adds a comment.
type CommentRequest struct {
	Text string `json:"text"`
	// Author is who the comment is recorded under, defaulting to the caller's
	// actor. A body author overrides it: the override is the add-as contract
	// that land's review records and done's completion notes rely on, so it
	// stays, and limiting it to an operator-class token is the follow-up
	// (gt-qr8pe).
	Author string `json:"author,omitempty"`
}

// CommentsResponse lists an issue's comments, oldest first.
type CommentsResponse struct {
	Comments []Comment `json:"comments"`
}

// DependencyRequest links the issue in the path to DependsOn.
type DependencyRequest struct {
	DependsOn string `json:"depends_on"`
	Type      string `json:"type"`
}

// DependenciesResponse lists what an issue depends on.
type DependenciesResponse struct {
	Dependencies []IssueDep `json:"dependencies"`
}

// ChildrenOfRequest asks for the direct children of several parents.
type ChildrenOfRequest struct {
	Parents []string `json:"parents"`
}

// ChildrenOfResponse maps each parent that has children to them.
type ChildrenOfResponse struct {
	Children map[string][]Issue `json:"children"`
}

// AssignedRequest asks for the issues of several assignees in one call, each
// restricted to the request's statuses. Assignees and Statuses are required:
// an empty list is an invalid error naming the field, not an answer about
// every seat.
type AssignedRequest struct {
	// Assignees are the assignee strings as stored on the issue, such as
	// "gastown/polecats/bullet".
	Assignees []string `json:"assignees"`
	// Statuses are the statuses an issue must have to be answered.
	Statuses []string `json:"statuses"`
	// Rig restricts the answer to one rig's issues; empty reads every rig.
	Rig string `json:"rig,omitempty"`
	// IncludeWisps adds the wisps, which are left out by default.
	IncludeWisps bool `json:"include_wisps,omitempty"`
	// Limit is the most rows one page holds; 0 takes the server's default page
	// size, and a page with more rows left carries NextCursor.
	Limit int `json:"limit,omitempty"`
	// Cursor is the previous page's NextCursor; empty reads the first page.
	Cursor string `json:"cursor,omitempty"`
}

// Bounds on one assigned request.
const (
	MaxAssignedAssignees = 256
	MaxAssignedStatuses  = 8
)

// Validate refuses a request with no assignee or no status, naming the empty
// field, and one over either bound.
func (r AssignedRequest) Validate() error {
	n := countNonEmpty(r.Assignees)
	if n == 0 {
		return Invalid("assignees is required")
	}
	if n > MaxAssignedAssignees {
		return Invalid("assignees has %d entries, at most %d", n, MaxAssignedAssignees)
	}
	if n = countNonEmpty(r.Statuses); n == 0 {
		return Invalid("statuses is required")
	}
	if n > MaxAssignedStatuses {
		return Invalid("statuses has %d entries, at most %d", n, MaxAssignedStatuses)
	}
	return nil
}

func countNonEmpty(ss []string) int {
	n := 0
	for _, s := range ss {
		if s != "" {
			n++
		}
	}
	return n
}

// AssignedPage is one page of the assigned read, keyed by assignee. Rows keep
// list order (priority, then creation, then id); an assignee with no row on
// this page is absent. Count is the number of rows across the map.
type AssignedPage struct {
	Assigned   map[string][]Issue `json:"assigned"`
	Count      int                `json:"count"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// CountRequest asks how many issues match its filter.
type CountRequest struct {
	Filter ListFilter `json:"filter"`
}

// Validate applies ValidateCount to the request's filter.
func (r CountRequest) Validate() error { return ValidateCount(r.Filter) }

// ValidateCount refuses a filter that carries a page, naming the field: a
// count is of every issue the filter matches, so a caller holding a page has
// the list's answer, not this one's.
func ValidateCount(f ListFilter) error {
	if f.Limit != 0 {
		return Invalid("count takes no limit; it counts every issue the filter matches")
	}
	if f.Cursor != "" {
		return Invalid("count takes no cursor; it counts every issue the filter matches")
	}
	return nil
}

// CountResponse is how many issues match a filter.
type CountResponse struct {
	Count int `json:"count"`
}

// MaxPurgeWisps bounds one purge batch: past it a caller should walk the wisp
// list and send the expired ids in smaller batches.
const MaxPurgeWisps = 500

// PurgeWispsRequest names the wisps to purge. The wisp policy decides which of
// them are expired; the rest are kept and reported with the reason.
type PurgeWispsRequest struct {
	IDs []string `json:"ids"`
}

// Validate refuses an empty batch, one past MaxPurgeWisps, and an empty id.
func (r PurgeWispsRequest) Validate() error {
	switch {
	case len(r.IDs) == 0:
		return Invalid("purge needs at least one id")
	case len(r.IDs) > MaxPurgeWisps:
		return Invalid("purge takes at most %d ids, got %d", MaxPurgeWisps, len(r.IDs))
	}
	for _, id := range r.IDs {
		if id == "" {
			return Invalid("purge ids must not be empty")
		}
	}
	return nil
}

// PurgeWispsResult says which named wisps were deleted and which were kept.
type PurgeWispsResult struct {
	Purged []string   `json:"purged"`
	Kept   []KeptWisp `json:"kept"`
}

// KeptWisp is a named wisp the purge left in place, with the policy's reason.
type KeptWisp struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// DispatchBoardRequest asks for everything one dispatch tick reads about the
// work board: per rig the ready issues, the issues a seat holds and the
// children of the ready issues, plus the status of a few named issues.
type DispatchBoardRequest struct {
	// Rigs are the rig prefixes to read. A prefix the tracker does not know
	// is reported in Errors, not failed.
	Rigs []string `json:"rigs"`
	// ExcludeLabels and ExcludeTypes narrow the ready issues, as in ReadyFilter.
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	ExcludeTypes  []string `json:"exclude_types,omitempty"`
	// HeldStatuses are the statuses that mean a seat holds an issue.
	HeldStatuses []string `json:"held_statuses,omitempty"`
	// ProbeIDs are issues whose status is wanted (revert beads).
	ProbeIDs []string `json:"probe_ids,omitempty"`
}

// HeldBead is an assigned issue in one of the held statuses.
type HeldBead struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Assignee    string `json:"assignee"`
	Description string `json:"description"`
}

// ChildStatus is a direct child of a ready issue.
type ChildStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// RigBoard is one rig's part of the dispatch board. Ready, Held and Children
// are never null.
type RigBoard struct {
	Ready    []Issue                  `json:"ready"`
	Held     []HeldBead               `json:"held"`
	Children map[string][]ChildStatus `json:"children"`
}

// Probe is the status of one probed issue; Found is false when it does not exist.
type Probe struct {
	Found  bool   `json:"found"`
	Status string `json:"status,omitempty"`
}

// DispatchBoardResponse is one consistent snapshot of the board. Rigs holds
// the rigs that were read; Errors says why each other requested rig was not.
type DispatchBoardResponse struct {
	Rigs   map[string]RigBoard `json:"rigs"`
	Probes map[string]Probe    `json:"probes"`
	Errors map[string]string   `json:"errors"`
}

// Stats counts a rig's durable issues by status and its wisps as one total;
// a wisp's status is not broken out, so Total and ByStatus stay durable only.
type Stats struct {
	Total    int            `json:"total"`
	Wisps    int            `json:"wisps"`
	ByStatus map[string]int `json:"by_status"`
}

// ConfigValue is a configuration key's value; Set is false for a key that has
// none, whose Value is then empty.
type ConfigValue struct {
	Value string `json:"value"`
	Set   bool   `json:"set"`
}

// ConfigSetRequest sets a configuration key.
type ConfigSetRequest struct {
	Value string `json:"value"`
}

// Limits on one graph.
const (
	MaxGraphNodes = 500
	MaxGraphEdges = 2000
)

// GraphNode is one issue of a graph. Key is the caller's own name for it,
// unique within the request; edges refer to it by that name. The embedded
// CreateParams is the issue; its ID, Parent, Rig and Actor must be empty (the
// server mints the id, edges set parents, the request names the rig and the
// token names the actor).
type GraphNode struct {
	Key string `json:"key"`
	CreateParams
}

// GraphEdge links two issues of a graph, or an issue of the graph to one that
// already exists. From depends on To, with the relation Type ("blocks",
// "parent-child" with From the child, ...). From and To are node keys; one
// that is not a key names an existing issue (or external:<rig>:<id>).
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// GraphRequest creates a set of issues and the links between them, all or
// none: a molecule with its steps, or any batch of related work.
type GraphRequest struct {
	Rig   string      `json:"rig"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges,omitempty"`
}

// GraphResult maps each node key to the id of the issue created for it.
type GraphResult struct {
	IDs map[string]string `json:"ids"`
}
